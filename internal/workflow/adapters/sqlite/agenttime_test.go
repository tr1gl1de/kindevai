package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// scanTimes is the calculation before KIN-75, kept as the oracle: every attempt of every task f selects plus its
// plan discussion answers (the time of the tasks ids among them, and the sum).
func scanTimes(t *testing.T, d *sql.DB, f app.TaskFilter, ids []string, now time.Time) (map[string]domain.AgentTime, domain.AgentTime) {
	t.Helper()
	ctx := t.Context()
	out := map[string]domain.AgentTime{}
	for _, id := range ids {
		out[id] = domain.AgentTime{Status: domain.WorkNone}
	}
	total := domain.AgentTime{Status: domain.WorkNone}
	add := func(id string, s domain.WorkStatus, w time.Duration) {
		total.Add(s, w)
		if a, ok := out[id]; ok {
			a.Add(s, w)
			out[id] = a
		}
	}
	if err := sqlite.New(d).EachAttempt(ctx, f, func(id string, a domain.Attempt) error {
		st, w := a.Work(now)
		add(id, st, w)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.QueryContext(ctx, `SELECT t.id, t.project_id, t.state, SUM(m.work_ms) FROM tasks t
		JOIN plan_discussion_messages m ON m.task_id = t.id AND m.role = 'assistant' GROUP BY t.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, state string
		var ms int64
		if err := rows.Scan(&id, &project, &state, &ms); err != nil {
			t.Fatal(err)
		}
		if (len(f.ProjectIDs) == 0 || slices.Contains(f.ProjectIDs, project)) && (len(f.TaskIDs) == 0 || slices.Contains(f.TaskIDs, id)) &&
			(len(f.States) == 0 || slices.Contains(f.States, domain.TaskState(state))) {
			add(id, domain.WorkComplete, time.Duration(ms)*time.Millisecond)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out, total
}

var attemptStates = []domain.AttemptState{domain.AttemptRunning, domain.AttemptSucceeded, domain.AttemptFailed,
	domain.AttemptCancelled, domain.AttemptUnknown}

// randomAttempts appends attempts of every shape (no process, running, complete, missing or unusable ends, too
// long, ends before starts) to the execution's steps and saves it through the normal write path.
func randomAttempts(t *testing.T, repo *sqlite.Repository, id string, r *rand.Rand) {
	t.Helper()
	e, err := repo.Execution(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range e.Steps {
		s := &e.Steps[i]
		for range r.IntN(4) {
			n := len(s.Attempts) + 1
			a := domain.Attempt{Number: n, RunnerID: "r1", Fence: int64(100 + n), State: attemptStates[r.IntN(len(attemptStates))],
				StartedAt: t0, WorkIncomplete: r.IntN(8) == 0}
			if r.IntN(5) > 0 {
				start := t0.Add(-time.Duration(r.IntN(3600_000_000)) * time.Microsecond)
				a.WorkStart = &start
				if r.IntN(3) > 0 {
					end := start.Add(time.Duration(r.Int64N(int64(25*time.Hour))) - time.Minute)
					a.WorkEnd = &end
				}
			}
			s.Attempts = append(s.Attempts, a)
		}
	}
	if err := repo.SaveExecution(t.Context(), &e); err != nil {
		t.Fatal(err)
	}
}

// taskWithExecution creates a task in p1 with a running execution (no issue reference, so many can exist).
func taskWithExecution(t *testing.T, svc *app.Service) (domain.Task, domain.Execution) {
	t.Helper()
	ctx := t.Context()
	task, err := svc.CreateTask(ctx, "p1", "Timed", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, task.ID, plan(), "planner"); err != nil {
		t.Fatal(err)
	}
	e, err := svc.StartExecution(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task, e
}

func answer(t *testing.T, repo *sqlite.Repository, taskID string, role domain.MessageRole, work time.Duration) {
	t.Helper()
	m := domain.DiscussionMessage{ID: fmt.Sprintf("m-%s-%d", taskID, ids.Add(1)), TaskID: taskID, Role: role, Author: "planner/mock",
		Text: "ok", Status: domain.MessageAnswered, RunID: fmt.Sprintf("run-%d", ids.Add(1)), Work: work, CreatedAt: t0, UpdatedAt: t0}
	if err := repo.CreateMessage(t.Context(), &m); err != nil {
		t.Fatal(err)
	}
}

// agentFixture builds tasks with random attempts saved several times, discussion answers, cancelled
// tasks and tasks without any work; it returns every task id.
func agentFixture(t *testing.T, d *sql.DB, seed uint64) []string {
	t.Helper()
	ctx := t.Context()
	r := rand.New(rand.NewPCG(seed, 75))
	svc := service(d, fixed)
	repo := sqlite.New(d)
	var all []string
	for i := range 12 {
		task, e := taskWithExecution(t, svc)
		all = append(all, task.ID)
		randomAttempts(t, repo, e.ID, r)
		if i%3 == 0 { // cancelled: a later save still records an attempt's end (a late process end)
			if _, err := svc.CancelTask(ctx, task.ID, "stop"); err != nil {
				t.Fatal(err)
			}
		}
		if i%4 == 1 {
			answer(t, repo, task.ID, domain.RoleAssistant, time.Duration(r.IntN(90_000))*time.Millisecond)
			answer(t, repo, task.ID, domain.RoleUser, time.Minute) // not agent time
		}
		randomAttempts(t, repo, e.ID, r) // a later save changes the same execution again
	}
	idle, err := svc.CreateTask(ctx, "p1", "Idle", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	talk, err := svc.CreateTask(ctx, "p1", "Only talk", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	answer(t, repo, talk.ID, domain.RoleAssistant, 0) // an answer that took no time still makes the time complete
	return append(all, idle.ID, talk.ID)
}

func assertAgentTimesMatch(t *testing.T, d *sql.DB, all []string) {
	t.Helper()
	repo := sqlite.New(d)
	filters := []app.TaskFilter{
		{ProjectIDs: []string{"p1"}},
		{ProjectIDs: []string{"p1"}, States: []domain.TaskState{domain.TaskRunning}},
		{ProjectIDs: []string{"p1"}, States: []domain.TaskState{domain.TaskCancelled, domain.TaskRunning}},
		{TaskIDs: all[:3]},
		{ProjectIDs: []string{"nope"}},
	}
	for _, now := range []time.Time{t0, t0.Add(90 * time.Minute), t0.Add(48 * time.Hour)} {
		for _, f := range filters {
			ids := all[1:7]
			_, wantTotal := scanTimes(t, d, f, nil, now)
			wantIDs, _ := scanTimes(t, d, app.TaskFilter{TaskIDs: ids}, ids, now)
			gotIDs, gotTotal, err := repo.AgentTimes(t.Context(), f, ids, now)
			if err != nil {
				t.Fatal(err)
			}
			if gotTotal != wantTotal {
				t.Errorf("now %v filter %+v: total %+v, want %+v", now, f, gotTotal, wantTotal)
			}
			for _, id := range ids {
				if gotIDs[id] != wantIDs[id] {
					t.Errorf("now %v filter %+v task %s: %+v, want %+v", now, f, id, gotIDs[id], wantIDs[id])
				}
			}
		}
	}
}

// KIN-75: the materialized per-task agent time equals the scan over every attempt (complete, incomplete, running
// and no-process periods, several executions, discussion answers) for every filter and time.
func TestAgentTimesMatchTheAttemptScan(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			d, _ := testDB(t)
			all := agentFixture(t, d, seed)
			assertAgentTimesMatch(t, d, all)
		})
	}
}

// KIN-75: the migration fills the aggregate from the attempts and answers recorded before it.
func TestAgentTimeMigrationBackfillsExistingTasks(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	all := agentFixture(t, d, 9)
	body, err := fs.ReadFile(db.Migrations(), "0039_task_agent_time.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), "DROP TABLE task_agent_time; DROP INDEX executions_task"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), string(body)); err != nil {
		t.Fatal(err)
	}
	assertAgentTimesMatch(t, d, all)
}

// cloneTask copies a task with its plan revisions, executions, steps, attempts and agent time n times (ids get
// "-<i>"), in one statement per table.
func cloneTask(t testing.TB, d *sql.DB, taskID string, n int) {
	t.Helper()
	ctx := context.Background()
	tables := []struct {
		name  string
		ids   []string
		where string
	}{
		{"tasks", []string{"id"}, "id = ?1"},
		{"task_plan_revisions", []string{"task_id"}, "task_id = ?1"},
		{"executions", []string{"id", "task_id"}, "task_id = ?1"},
		{"workflow_steps", []string{"id", "execution_id"}, "execution_id IN (SELECT id FROM executions WHERE task_id = ?1)"},
		{"step_attempts", []string{"step_id"}, `step_id IN (SELECT s.id FROM workflow_steps s JOIN executions e
			ON e.id = s.execution_id WHERE e.task_id = ?1)`},
		{"task_agent_time", []string{"task_id"}, "task_id = ?1"},
	}
	for _, tb := range tables {
		cols := columns(t, d, tb.name)
		sel := make([]string, 0, len(cols))
		for _, c := range cols {
			if slices.Contains(tb.ids, c) {
				c += " || '-' || n"
			}
			sel = append(sel, c)
		}
		q := fmt.Sprintf(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?2)
			INSERT INTO %s(%s) SELECT %s FROM %s, seq WHERE %s`, tb.name, strings.Join(cols, ", "), strings.Join(sel, ", "), tb.name, tb.where)
		if _, err := d.ExecContext(ctx, q, taskID, n); err != nil {
			t.Fatalf("cloning %s: %v", tb.name, err)
		}
	}
}

func columns(t testing.TB, d *sql.DB, table string) []string {
	t.Helper()
	rows, err := d.QueryContext(context.Background(), "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols
}

// completedTask creates a task whose two attempts ran 40 s in total.
func completedTask(t testing.TB, d *sql.DB, svc *app.Service) domain.Task {
	t.Helper()
	ctx := context.Background()
	task, err := svc.CreateTask(ctx, "p1", "Done", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(ctx, task.ID, plan(), "planner"); err != nil {
		t.Fatal(err)
	}
	e, err := svc.StartExecution(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"backend", "lint"} {
		id := e.StepByKey(key).ID
		if _, err := svc.Claim(ctx, id, "r1"); err != nil {
			t.Fatal(err)
		}
		if err := svc.StartStepAt(ctx, id, "r1", 1, t0, "mock"); err != nil {
			t.Fatal(err)
		}
		if err := svc.CompleteStepAt(ctx, id, "r1", 1, t0.Add(20*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// KIN-75: with 10k tasks the list's total comes from the per-task aggregate: settled attempts are not read (the
// test changes them behind the aggregate's back and the total stays), only a running task's attempts are.
func TestAgentTimesOverTenThousandTasksReadTheAggregate(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	now := t0
	svc := service(d, func() time.Time { return now })
	done := completedTask(t, d, svc)
	const n = 10_000
	cloneTask(t, d, done.ID, n-1)
	live, e := taskWithExecution(t, svc)
	step := e.StepByKey("backend").ID
	if _, err := svc.Claim(ctx, step, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartStepAt(ctx, step, "r1", 1, t0, "mock"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "UPDATE step_attempts SET work_ended_at = NULL WHERE state <> 'RUNNING'"); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(5 * time.Second)
	ids := []string{done.ID, done.ID + "-17", live.ID}
	got, total, err := sqlite.New(d).AgentTimes(ctx, app.TaskFilter{ProjectIDs: []string{"p1"}}, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.AgentTime{Total: n*40*time.Second + 5*time.Second, Status: domain.WorkRunning}
	if total != want {
		t.Fatalf("total %+v, want %+v", total, want)
	}
	if got[done.ID+"-17"] != (domain.AgentTime{Total: 40 * time.Second, Status: domain.WorkComplete}) ||
		got[live.ID] != (domain.AgentTime{Total: 5 * time.Second, Status: domain.WorkRunning}) {
		t.Fatalf("per task %+v", got)
	}
}

// BenchmarkAgentTimes10k measures the list total over 10k finished tasks and one running task.
func BenchmarkAgentTimes10k(b *testing.B) {
	d, err := db.Open(b.Context(), b.TempDir()+"/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(b.Context(), d, db.Migrations()); err != nil {
		b.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(b.Context(), q); err != nil {
			b.Fatal(err)
		}
	}
	svc := service(d, fixed)
	done := completedTask(b, d, svc)
	cloneTask(b, d, done.ID, 9_999)
	repo := sqlite.New(d)
	f := app.TaskFilter{ProjectIDs: []string{"p1"}}
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := repo.AgentTimes(b.Context(), f, []string{done.ID}, t0); err != nil {
			b.Fatal(err)
		}
	}
}
