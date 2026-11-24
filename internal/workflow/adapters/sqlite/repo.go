// Package sqlite stores the workflow context in SQLite. Every statement runs on db.Conn, so a use case's changes to
// a task, its plan revision and its execution commit together. Tasks and executions are saved whole with an
// optimistic version check.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// tsFormat has a fixed width, so stored timestamps compare correctly as text (lease sweeps).
const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository implements app.Repository.
type Repository struct{ db *sql.DB }

var _ app.Repository = (*Repository)(nil)

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func nullStr[T ~string](s T) any {
	if s == "" {
		return nil
	}
	return string(s)
}

func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func parseNullTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTS(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

type scanner interface{ Scan(dest ...any) error }

const taskCols = `id, project_id, title, youtrack_issue, state, block_reason, blocked_from, reason, plan_revision,
	created_by, created_at, updated_at, version`

func scanTask(row scanner) (domain.Task, error) {
	var t domain.Task
	var issue, blockReason, blockedFrom, createdBy sql.NullString
	var created, updated string
	err := row.Scan(&t.ID, &t.ProjectID, &t.Title, &issue, &t.State, &blockReason, &blockedFrom, &t.Reason.Detail,
		&t.PlanRevision, &createdBy, &created, &updated, &t.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return t, domain.ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("reading task: %w", err)
	}
	t.YouTrackIssue, t.CreatedBy = issue.String, createdBy.String
	t.BlockReason, t.BlockedFrom = domain.WaitReason(blockReason.String), domain.TaskState(blockedFrom.String)
	t.Reason.Code = t.BlockReason
	if t.CreatedAt, err = parseTS(created); err == nil {
		t.UpdatedAt, err = parseTS(updated)
	}
	if err != nil {
		return t, fmt.Errorf("reading task %s: %w", t.ID, err)
	}
	return t, nil
}

// CreateTask inserts a new task at version 1.
func (r *Repository) CreateTask(ctx context.Context, t *domain.Task) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO tasks(`+taskCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		t.ID, t.ProjectID, t.Title, nullStr(t.YouTrackIssue), t.State, nullStr(t.BlockReason), nullStr(t.BlockedFrom),
		t.Reason.Detail, t.PlanRevision, nullStr(t.CreatedBy), ts(t.CreatedAt), ts(t.UpdatedAt))
	switch {
	case db.IsUniqueViolation(err) && strings.Contains(err.Error(), "youtrack_issue"):
		return app.ErrDuplicateIssue
	case db.IsUniqueViolation(err):
		return domain.ErrConflict
	case db.IsForeignKeyViolation(err):
		return fmt.Errorf("project %s or creator: %w", t.ProjectID, domain.ErrNotFound)
	case err != nil:
		return fmt.Errorf("creating task: %w", err)
	}
	t.Version = 1
	return nil
}

// Task loads a task.
func (r *Repository) Task(ctx context.Context, id string) (domain.Task, error) {
	return scanTask(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+taskCols+" FROM tasks WHERE id = ?", id))
}

// TaskByIssue loads the project's task for a YouTrack issue.
func (r *Repository) TaskByIssue(ctx context.Context, projectID, youtrackIssue string) (domain.Task, error) {
	return scanTask(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+taskCols+
		" FROM tasks WHERE project_id = ? AND youtrack_issue = ?", projectID, youtrackIssue))
}

// SaveTask writes the task's mutable fields if its version is unchanged and increments the version.
func (r *Repository) SaveTask(ctx context.Context, t *domain.Task) error {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE tasks SET title = ?, state = ?, block_reason = ?, blocked_from = ?,
		reason = ?, plan_revision = ?, updated_at = ?, version = version + 1 WHERE id = ? AND version = ?`,
		t.Title, t.State, nullStr(t.BlockReason), nullStr(t.BlockedFrom), t.Reason.Detail, t.PlanRevision,
		ts(t.UpdatedAt), t.ID, t.Version)
	if err := oneRow(res, err, "saving task"); err != nil {
		return err
	}
	t.Version++
	return nil
}

func oneRow(res sql.Result, err error, op string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return domain.ErrConflict
	}
	return nil
}

// CreateRevision inserts an immutable plan revision with its canonical JSON.
func (r *Repository) CreateRevision(ctx context.Context, rev domain.PlanRevision) error {
	c, err := rev.Plan().CanonicalJSON()
	if err != nil {
		return err
	}
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO task_plan_revisions(task_id, revision, plan_json,
		plan_hash, risk, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rev.TaskID(), rev.Number(), string(c), rev.Hash(), rev.Risk(), rev.CreatedBy(), ts(rev.CreatedAt()))
	switch {
	case db.IsUniqueViolation(err):
		return domain.ErrConflict
	case db.IsForeignKeyViolation(err):
		return fmt.Errorf("task %s: %w", rev.TaskID(), domain.ErrNotFound)
	case err != nil:
		return fmt.Errorf("creating plan revision: %w", err)
	}
	return nil
}

// Revision loads a plan revision and verifies its hash.
func (r *Repository) Revision(ctx context.Context, taskID string, number int) (domain.PlanRevision, error) {
	var planJSON, hash, risk, by, created string
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT plan_json, plan_hash, risk, created_by, created_at
		FROM task_plan_revisions WHERE task_id = ? AND revision = ?`, taskID, number).
		Scan(&planJSON, &hash, &risk, &by, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PlanRevision{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.PlanRevision{}, fmt.Errorf("reading plan revision: %w", err)
	}
	at, err := parseTS(created)
	if err != nil {
		return domain.PlanRevision{}, fmt.Errorf("reading plan revision: %w", err)
	}
	return domain.RestorePlanRevision(taskID, number, []byte(planJSON), hash, domain.Risk(risk), by, at)
}

// CreateExecution inserts the execution with its steps at version 1; domain.ErrConflict if the task already has a
// running execution.
func (r *Repository) CreateExecution(ctx context.Context, e *domain.Execution) error {
	c := db.Conn(ctx, r.db)
	_, err := c.ExecContext(ctx, `INSERT INTO executions(id, task_id, plan_revision, plan_hash, state, max_parallel,
		reason, created_at, finished_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		e.ID, e.TaskID, e.PlanRevision, e.PlanHash, e.State, e.MaxParallel, e.Reason, ts(e.CreatedAt), nullTS(e.FinishedAt))
	switch {
	case db.IsUniqueViolation(err):
		return domain.ErrConflict
	case db.IsForeignKeyViolation(err):
		return fmt.Errorf("plan revision %s/%d: %w", e.TaskID, e.PlanRevision, domain.ErrNotFound)
	case err != nil:
		return fmt.Errorf("creating execution: %w", err)
	}
	for i, s := range e.Steps {
		deps, err := json.Marshal(s.DependsOn)
		if err != nil {
			return fmt.Errorf("encoding dependencies: %w", err)
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO workflow_steps(id, execution_id, position, step_key, title,
			repository, depends_on, state, wait_reason, reason, fence, runner_id, lease_expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, e.ID, i, s.Key, s.Title, s.Repository, string(deps), s.State, nullStr(s.WaitReason), s.Reason,
			s.Fence, nullStr(s.RunnerID), nullTS(s.LeaseExpiresAt)); err != nil {
			return fmt.Errorf("creating step %s: %w", s.Key, err)
		}
		if len(s.Attempts) > 0 {
			return errors.New("creating execution: a new execution has no attempts")
		}
	}
	e.Version = 1
	return nil
}

const execCols = "id, task_id, plan_revision, plan_hash, state, max_parallel, reason, created_at, finished_at, version"

// Execution loads an execution with its steps and attempts.
func (r *Repository) Execution(ctx context.Context, id string) (domain.Execution, error) {
	return r.loadExecution(ctx, "SELECT "+execCols+" FROM executions WHERE id = ?", id)
}

// ExecutionByStep loads the execution that owns the step.
func (r *Repository) ExecutionByStep(ctx context.Context, stepID string) (domain.Execution, error) {
	return r.loadExecution(ctx, "SELECT "+execCols+` FROM executions
		WHERE id = (SELECT execution_id FROM workflow_steps WHERE id = ?)`, stepID)
}

// RunningExecution loads the task's running execution.
func (r *Repository) RunningExecution(ctx context.Context, taskID string) (domain.Execution, error) {
	return r.loadExecution(ctx, "SELECT "+execCols+" FROM executions WHERE task_id = ? AND state = 'RUNNING'", taskID)
}

func (r *Repository) loadExecution(ctx context.Context, q string, arg string) (domain.Execution, error) {
	c := db.Conn(ctx, r.db)
	var e domain.Execution
	var created string
	var finished sql.NullString
	err := c.QueryRowContext(ctx, q, arg).Scan(&e.ID, &e.TaskID, &e.PlanRevision, &e.PlanHash, &e.State,
		&e.MaxParallel, &e.Reason, &created, &finished, &e.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return e, domain.ErrNotFound
	}
	if err != nil {
		return e, fmt.Errorf("reading execution: %w", err)
	}
	if e.CreatedAt, err = parseTS(created); err == nil {
		e.FinishedAt, err = parseNullTS(finished)
	}
	if err != nil {
		return e, fmt.Errorf("reading execution %s: %w", e.ID, err)
	}
	if e.Steps, err = r.steps(ctx, c, e.ID); err != nil {
		return e, err
	}
	return e, nil
}

func (r *Repository) steps(ctx context.Context, c db.Executor, execID string) ([]domain.Step, error) {
	rows, err := c.QueryContext(ctx, `SELECT id, step_key, title, repository, depends_on, state, wait_reason, reason,
		fence, runner_id, lease_expires_at FROM workflow_steps WHERE execution_id = ? ORDER BY position`, execID)
	if err != nil {
		return nil, fmt.Errorf("reading steps: %w", err)
	}
	defer rows.Close()
	var steps []domain.Step
	index := map[string]int{}
	for rows.Next() {
		var s domain.Step
		var deps string
		var wait, runner, lease sql.NullString
		if err := rows.Scan(&s.ID, &s.Key, &s.Title, &s.Repository, &deps, &s.State, &wait, &s.Reason, &s.Fence,
			&runner, &lease); err != nil {
			return nil, fmt.Errorf("reading step: %w", err)
		}
		if err := json.Unmarshal([]byte(deps), &s.DependsOn); err != nil {
			return nil, fmt.Errorf("reading step %s dependencies: %w", s.ID, err)
		}
		s.WaitReason, s.RunnerID = domain.WaitReason(wait.String), runner.String
		if s.LeaseExpiresAt, err = parseNullTS(lease); err != nil {
			return nil, fmt.Errorf("reading step %s: %w", s.ID, err)
		}
		index[s.ID] = len(steps)
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading steps: %w", err)
	}
	arows, err := c.QueryContext(ctx, `SELECT a.step_id, `+attemptCols+` FROM step_attempts a
		JOIN workflow_steps s ON s.id = a.step_id WHERE s.execution_id = ? ORDER BY a.step_id, a.number`, execID)
	if err != nil {
		return nil, fmt.Errorf("reading attempts: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var stepID string
		a, err := scanAttempt(arows, &stepID)
		if err != nil {
			return nil, err
		}
		if i, ok := index[stepID]; ok {
			steps[i].Attempts = append(steps[i].Attempts, a)
		}
	}
	if err := arows.Err(); err != nil {
		return nil, fmt.Errorf("reading attempts: %w", err)
	}
	return steps, nil
}

// SaveExecution writes the execution, its steps and attempts if its version is unchanged and increments the version.
func (r *Repository) SaveExecution(ctx context.Context, e *domain.Execution) error {
	c := db.Conn(ctx, r.db)
	res, err := c.ExecContext(ctx, `UPDATE executions SET state = ?, reason = ?, finished_at = ?, version = version + 1
		WHERE id = ? AND version = ?`, e.State, e.Reason, nullTS(e.FinishedAt), e.ID, e.Version)
	if err := oneRow(res, err, "saving execution"); err != nil {
		return err
	}
	for _, s := range e.Steps {
		res, err := c.ExecContext(ctx, `UPDATE workflow_steps SET state = ?, wait_reason = ?, reason = ?, fence = ?,
			runner_id = ?, lease_expires_at = ? WHERE id = ? AND execution_id = ?`,
			s.State, nullStr(s.WaitReason), s.Reason, s.Fence, nullStr(s.RunnerID), nullTS(s.LeaseExpiresAt), s.ID, e.ID)
		if err := oneRow(res, err, "saving step "+s.Key); err != nil {
			return err
		}
		for _, a := range s.Attempts {
			if _, err := c.ExecContext(ctx, `INSERT INTO step_attempts(step_id, number, runner_id, fence, state, error,
				started_at, finished_at, agent, work_started_at, work_ended_at, work_incomplete)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(step_id, number) DO UPDATE SET state = excluded.state, error = excluded.error,
				finished_at = excluded.finished_at, agent = excluded.agent,
				work_started_at = coalesce(work_started_at, excluded.work_started_at),
				work_ended_at = coalesce(work_ended_at, excluded.work_ended_at),
				work_incomplete = max(work_incomplete, excluded.work_incomplete)`,
				s.ID, a.Number, a.RunnerID, a.Fence, a.State, a.Error, ts(a.StartedAt), nullTS(a.FinishedAt), a.Agent,
				nullTS(a.WorkStart), nullTS(a.WorkEnd), a.WorkIncomplete); err != nil {
				return fmt.Errorf("saving attempt %d of step %s: %w", a.Number, s.Key, err)
			}
		}
	}
	e.Version++
	return nil
}

// ExpiredLeaseExecutions lists running executions with a step lease that ran out at now.
func (r *Repository) ExpiredLeaseExecutions(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT DISTINCT s.execution_id FROM workflow_steps s
		JOIN executions e ON e.id = s.execution_id
		WHERE e.state = 'RUNNING' AND s.lease_expires_at IS NOT NULL AND s.lease_expires_at <= ?
		ORDER BY s.execution_id`, ts(now))
	if err != nil {
		return nil, fmt.Errorf("listing expired leases: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing expired leases: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *Repository) ids(ctx context.Context, op, q string, args ...any) ([]string, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RunningExecutions lists the ids of every running execution, oldest first.
func (r *Repository) RunningExecutions(ctx context.Context) ([]string, error) {
	return r.ids(ctx, "listing running executions",
		`SELECT id FROM executions WHERE state = 'RUNNING' ORDER BY created_at, id`)
}

// TasksInState lists the ids of the tasks in state, oldest first.
func (r *Repository) TasksInState(ctx context.Context, state domain.TaskState) ([]string, error) {
	return r.ids(ctx, "listing tasks by state", `SELECT id FROM tasks WHERE state = ? ORDER BY created_at, id`, state)
}

// ExecutionsHeldBy lists the running executions with a step whose current claim (or unknown attempt) is the
// runner's.
func (r *Repository) ExecutionsHeldBy(ctx context.Context, runnerID string) ([]string, error) {
	return r.ids(ctx, "listing executions held by a runner", `SELECT DISTINCT e.id FROM executions e
		JOIN workflow_steps s ON s.execution_id = e.id
		WHERE e.state = 'RUNNING' AND s.runner_id = ? ORDER BY e.id`, runnerID)
}

// taskWhere is the WHERE clause (on tasks aliased t) and arguments for f; ok is false when f selects nothing.
func taskWhere(f app.TaskFilter) (string, []any, bool) {
	if len(f.ProjectIDs) == 0 && len(f.TaskIDs) == 0 {
		return "", nil, false
	}
	var conds []string
	var args []any
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		conds = append(conds, col+" IN (?"+strings.Repeat(", ?", len(vals)-1)+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in("t.project_id", f.ProjectIDs)
	in("t.id", f.TaskIDs)
	states := make([]string, 0, len(f.States))
	for _, st := range f.States {
		states = append(states, string(st))
	}
	in("t.state", states)
	return strings.Join(conds, " AND "), args, true
}

// ListTasks returns the tasks f selects, newest first, at most limit.
func (r *Repository) ListTasks(ctx context.Context, f app.TaskFilter, limit int) ([]domain.Task, error) {
	where, args, ok := taskWhere(f)
	if !ok {
		return nil, nil
	}
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+taskCols+" FROM tasks t WHERE "+where+
		" ORDER BY created_at DESC, id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	defer rows.Close()
	var out []domain.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const attemptCols = `a.number, a.runner_id, a.fence, a.state, a.error, a.started_at, a.finished_at, a.agent,
	a.work_started_at, a.work_ended_at, a.work_incomplete`

func scanAttempt(row scanner, key *string) (domain.Attempt, error) {
	var a domain.Attempt
	var started string
	var finished, workStart, workEnd sql.NullString
	if err := row.Scan(key, &a.Number, &a.RunnerID, &a.Fence, &a.State, &a.Error, &started, &finished, &a.Agent,
		&workStart, &workEnd, &a.WorkIncomplete); err != nil {
		return a, fmt.Errorf("reading attempt: %w", err)
	}
	var err error
	if a.StartedAt, err = parseTS(started); err == nil {
		if a.FinishedAt, err = parseNullTS(finished); err == nil {
			if a.WorkStart, err = parseNullTS(workStart); err == nil {
				a.WorkEnd, err = parseNullTS(workEnd)
			}
		}
	}
	if err != nil {
		return a, fmt.Errorf("reading attempt %d: %w", a.Number, err)
	}
	return a, nil
}

// EachAttempt streams the attempts of every execution of the tasks f selects (rows are not buffered).
func (r *Repository) EachAttempt(ctx context.Context, f app.TaskFilter, fn func(taskID string, a domain.Attempt) error) error {
	where, args, ok := taskWhere(f)
	if !ok {
		return nil
	}
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT t.id, `+attemptCols+` FROM tasks t
		JOIN executions e ON e.task_id = t.id JOIN workflow_steps s ON s.execution_id = e.id
		JOIN step_attempts a ON a.step_id = s.id WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("reading agent time: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var taskID string
		a, err := scanAttempt(rows, &taskID)
		if err != nil {
			return err
		}
		if err := fn(taskID, a); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExecutionSpans returns, per task with executions, its first execution's start and its last one's end (nil
// while one runs). Timestamps have a fixed width, so MIN/MAX compare them correctly as text.
func (r *Repository) ExecutionSpans(ctx context.Context, taskIDs []string) (map[string]app.ExecutionSpan, error) {
	out := map[string]app.ExecutionSpan{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(taskIDs))
	for _, id := range taskIDs {
		args = append(args, id)
	}
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT task_id, min(created_at),
		CASE WHEN sum(finished_at IS NULL) > 0 THEN NULL ELSE max(finished_at) END
		FROM executions WHERE task_id IN (?`+strings.Repeat(", ?", len(taskIDs)-1)+`) GROUP BY task_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading execution spans: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, start string
		var end sql.NullString
		if err := rows.Scan(&id, &start, &end); err != nil {
			return nil, fmt.Errorf("reading execution spans: %w", err)
		}
		var sp app.ExecutionSpan
		if sp.Start, err = parseTS(start); err == nil {
			sp.End, err = parseNullTS(end)
		}
		if err != nil {
			return nil, fmt.Errorf("reading execution span of %s: %w", id, err)
		}
		out[id] = sp
	}
	return out, rows.Err()
}

// LatestExecution loads the task's most recent execution.
func (r *Repository) LatestExecution(ctx context.Context, taskID string) (domain.Execution, error) {
	return r.loadExecution(ctx, "SELECT "+execCols+
		" FROM executions WHERE task_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1", taskID)
}
