package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Per-task agent time (KIN-75): task_agent_time keeps, per task, the sum of the periods that no longer change
// (domain.Attempt.Work is anything but running, and plan discussion answers) and the number of running attempts.
// refreshAgentTime rewrites a task's row in the transaction that changed its attempts or answers; AgentTimes reads
// the rows and only the attempts of tasks with running ones, so the task list does not scan every attempt.

// refreshAgentTime recomputes the task's aggregate from its attempts and discussion answers.
func refreshAgentTime(ctx context.Context, c db.Executor, taskID string) error {
	attempts, err := taskAttempts(ctx, c, taskID)
	if err != nil {
		return err
	}
	settled, running := domain.AgentTime{Status: domain.WorkNone}, 0
	for _, a := range attempts {
		// The period's status does not depend on the clock, only a running period's length does.
		if st, d := a.Work(time.Time{}); st == domain.WorkRunning {
			running++
		} else {
			settled.Add(st, d)
		}
	}
	var answers, ms int64
	if err := c.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(work_ms), 0) FROM plan_discussion_messages
		WHERE task_id = ? AND role = 'assistant'`, taskID).Scan(&answers, &ms); err != nil {
		return fmt.Errorf("reading the discussion agent time of task %s: %w", taskID, err)
	}
	if answers > 0 {
		settled.Add(domain.WorkComplete, time.Duration(ms)*time.Millisecond)
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO task_agent_time(task_id, settled_ns, settled_status, running)
		VALUES (?, ?, ?, ?) ON CONFLICT(task_id) DO UPDATE SET settled_ns = excluded.settled_ns,
		settled_status = excluded.settled_status, running = excluded.running`,
		taskID, int64(settled.Total), settled.Status, running); err != nil {
		return fmt.Errorf("saving the agent time of task %s: %w", taskID, err)
	}
	return nil
}

// AgentTimes returns the agent time (attempts' work periods and plan discussion answers) of the tasks ids and the
// sum over every task f selects; a running period counts up to now.
func (r *Repository) AgentTimes(ctx context.Context, f app.TaskFilter, ids []string, now time.Time) (map[string]domain.AgentTime, domain.AgentTime, error) {
	c := db.Conn(ctx, r.db)
	out := make(map[string]domain.AgentTime, len(ids))
	for _, id := range ids {
		out[id] = domain.AgentTime{Status: domain.WorkNone}
	}
	total := domain.AgentTime{Status: domain.WorkNone}
	live := map[string]bool{} // listed tasks with running attempts
	if err := listedAgentTimes(ctx, c, ids, out, live); err != nil {
		return nil, total, err
	}
	running := func(id string, a domain.Attempt, inTotal bool) {
		st, d := a.Work(now)
		if st != domain.WorkRunning {
			return // settled: already in the aggregate
		}
		if inTotal {
			total.Add(st, d)
		}
		if t, ok := out[id]; ok {
			t.Add(st, d)
			out[id] = t
		}
	}
	if where, args, ok := taskWhere(f); ok {
		var ns int64
		var rank int
		if err := c.QueryRowContext(ctx, `SELECT coalesce(sum(g.settled_ns), 0), coalesce(max(CASE g.settled_status
			WHEN 'incomplete' THEN 2 WHEN 'complete' THEN 1 ELSE 0 END), 0)
			FROM task_agent_time g JOIN tasks t ON t.id = g.task_id WHERE `+where, args...).Scan(&ns, &rank); err != nil {
			return nil, total, fmt.Errorf("reading the agent time total: %w", err)
		}
		total.Add([]domain.WorkStatus{domain.WorkNone, domain.WorkComplete, domain.WorkIncomplete}[rank], time.Duration(ns))
		// The attempts of the selected tasks with running periods, found by a join (no id list to bound).
		err := eachAttemptWhere(ctx, c, `JOIN task_agent_time g ON g.task_id = t.id AND g.running > 0`, where, args,
			func(id string, a domain.Attempt) {
				delete(live, id) // read here, also for the listed time
				running(id, a, true)
			})
		if err != nil {
			return nil, total, err
		}
	}
	if len(live) > 0 { // listed tasks outside f (at most the list's length)
		runIDs := make([]string, 0, len(live))
		for id := range live {
			runIDs = append(runIDs, id)
		}
		where, args, _ := taskWhere(app.TaskFilter{TaskIDs: runIDs})
		if err := eachAttemptWhere(ctx, c, "", where, args, func(id string, a domain.Attempt) { running(id, a, false) }); err != nil {
			return nil, total, err
		}
	}
	return out, total, nil
}

// taskAttempts loads every attempt of the task's executions.
func taskAttempts(ctx context.Context, c db.Executor, taskID string) ([]domain.Attempt, error) {
	rows, err := c.QueryContext(ctx, `SELECT e.task_id, `+attemptCols+` FROM executions e
		JOIN workflow_steps s ON s.execution_id = e.id JOIN step_attempts a ON a.step_id = s.id WHERE e.task_id = ?`, taskID)
	if err != nil {
		return nil, fmt.Errorf("reading the agent time of task %s: %w", taskID, err)
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		var id string
		a, err := scanAttempt(rows, &id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the agent time of task %s: %w", taskID, err)
	}
	return out, nil
}

// listedAgentTimes adds the aggregates of the tasks ids to out and marks those with running attempts in live.
func listedAgentTimes(ctx context.Context, c db.Executor, ids []string, out map[string]domain.AgentTime, live map[string]bool) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := c.QueryContext(ctx, `SELECT task_id, settled_ns, settled_status, running FROM task_agent_time
		WHERE task_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return fmt.Errorf("reading agent time: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var ns, running int64
		var st domain.WorkStatus
		if err := rows.Scan(&id, &ns, &st, &running); err != nil {
			return fmt.Errorf("reading agent time: %w", err)
		}
		t := out[id]
		t.Add(st, time.Duration(ns))
		out[id] = t
		if running > 0 {
			live[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading agent time: %w", err)
	}
	return nil
}

// eachAttemptWhere streams the attempts of the tasks t the condition selects (join adds tables to the tasks t).
func eachAttemptWhere(ctx context.Context, c db.Executor, join, where string, args []any, fn func(taskID string, a domain.Attempt)) error {
	rows, err := c.QueryContext(ctx, `SELECT t.id, `+attemptCols+` FROM tasks t `+join+`
		JOIN executions e ON e.task_id = t.id JOIN workflow_steps s ON s.execution_id = e.id
		JOIN step_attempts a ON a.step_id = s.id WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("reading running agent time: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		a, err := scanAttempt(rows, &id)
		if err != nil {
			return err
		}
		fn(id, a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading running agent time: %w", err)
	}
	return nil
}
