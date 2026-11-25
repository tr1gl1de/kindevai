package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := db.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func testDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d := openDB(t, path)
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	return d, path
}

type projects struct{}

func (projects) Repositories(_ context.Context, id string) ([]string, error) {
	if id != "p1" {
		return nil, domain.ErrNotFound
	}
	return []string{"api", "web"}, nil
}

var ids atomic.Int64

func service(d *sql.DB, now func() time.Time) *app.Service {
	return &app.Service{Repo: sqlite.New(d), Projects: projects{}, Policy: asProposed{}, Tx: db.Transactor{DB: d}, Now: now,
		NewID: func() string { return fmt.Sprintf("id-%d", ids.Add(1)) }}
}

func fixed() time.Time { return t0 }

func plan() domain.Plan {
	return domain.Plan{Risk: domain.RiskL1, MaxParallel: 2, Summary: "Login <page> & more", Steps: []domain.PlanStep{
		{Key: "backend", Title: "API", Repository: "api", Scope: []string{"internal/"}, Capabilities: []string{"go"}},
		{Key: "frontend", Title: "UI", Repository: "web", DependsOn: []string{"backend"}},
		{Key: "lint", Title: "Lint", Repository: "api"},
	}}
}

// running creates a task with an accepted plan and a running execution.
func running(t *testing.T, svc *app.Service) (domain.Task, domain.Execution) {
	t.Helper()
	ctx := t.Context()
	task, err := svc.CreateTask(ctx, "p1", "Login", "KIN-7", "u1")
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

func TestTaskRoundTripWithTheYouTrackReference(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	task, err := svc.CreateTask(ctx, "p1", "Login", "KIN-7", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateTask(ctx, "p1", "Again", "KIN-7", "u1"); !errors.Is(err, app.ErrDuplicateIssue) {
		t.Fatalf("duplicate issue = %v", err)
	}
	manual, err := svc.CreateTask(ctx, "p1", "Manual", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Task(ctx, manual.ID); got.YouTrackIssue != "" || got.CreatedBy != "" {
		t.Fatalf("manual task = %+v", got)
	}
	blocked, err := svc.BlockTask(ctx, task.ID, domain.WaitNetwork, "proxy down")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != blocked || got.YouTrackIssue != "KIN-7" || got.BlockedFrom != domain.TaskCreated || got.Version != 2 {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, blocked)
	}
	if _, err := svc.Task(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown task = %v", err)
	}
}

func TestTaskByIssueAndSavedTitle(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	if _, err := r.TaskByIssue(ctx, "p1", "KIN-7"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	task, err := domain.NewTask("t1", "p1", "Login", "KIN-7", "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if _, err := task.Retitle("Login page", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	got, err := r.TaskByIssue(ctx, "p1", "KIN-7")
	if err != nil || got.ID != "t1" || got.Title != "Login page" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSaveTaskDetectsConcurrentChanges(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	repo := sqlite.New(d)
	ctx := t.Context()
	task, _ := domain.NewTask("t1", "p1", "Login", "", "u1", t0)
	if err := repo.CreateTask(ctx, &task); err != nil || task.Version != 1 {
		t.Fatalf("create = %v, version %d", err, task.Version)
	}
	a, _ := repo.Task(ctx, "t1")
	b, _ := repo.Task(ctx, "t1")
	_ = a.Transition(domain.TaskAnalyzing, domain.Reason{}, t0)
	_ = b.Transition(domain.TaskCancelled, domain.Reason{Detail: "x"}, t0)
	if err := repo.SaveTask(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTask(ctx, &b); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale save = %v", err)
	}
	unknown, _ := domain.NewTask("t9", "p9", "x", "", "", t0)
	if err := repo.CreateTask(ctx, &unknown); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("task of an unknown project = %v", err)
	}
}

func TestPlanRevisionsAreStoredCanonicallyAndImmutable(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	task, _ := svc.CreateTask(ctx, "p1", "Login", "", "u1")
	_, _ = svc.StartAnalysis(ctx, task.ID)
	p := plan()
	p.Risk = domain.RiskL2
	rev, err := svc.SubmitPlan(ctx, task.ID, p, "planner")
	if err != nil {
		t.Fatal(err)
	}
	got, err := sqlite.New(d).Revision(ctx, task.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash() != rev.Hash() || got.Risk() != domain.RiskL2 || got.CreatedBy() != "planner" || !got.CreatedAt().Equal(t0) {
		t.Fatalf("revision = %+v", got)
	}
	var stored string
	_ = d.QueryRowContext(ctx, "SELECT plan_json FROM task_plan_revisions WHERE task_id = ?", task.ID).Scan(&stored)
	if c, _ := rev.Plan().CanonicalJSON(); stored != string(c) {
		t.Fatalf("stored %s, want canonical %s", stored, c)
	}
	if _, err := d.ExecContext(ctx, "UPDATE task_plan_revisions SET plan_hash = ? WHERE task_id = ?", rev.Hash(), task.ID); err == nil {
		t.Fatal("a plan revision was updated")
	}
	if err := sqlite.New(d).CreateRevision(ctx, rev); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate revision = %v", err)
	}
	if _, err := sqlite.New(d).Revision(ctx, task.ID, 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing revision = %v", err)
	}
}

func TestExecutionRoundTrip(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	task, e := running(t, svc)
	id := e.StepByKey("backend").ID
	l, err := svc.Claim(ctx, id, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartStep(ctx, id, "r1", l.Fence); err != nil {
		t.Fatal(err)
	}
	lint := e.StepByKey("lint").ID
	l2, _ := svc.Claim(ctx, lint, "r2")
	_ = svc.StartStep(ctx, lint, "r2", l2.Fence)
	_ = svc.FailStep(ctx, lint, "r2", l2.Fence, "exit 1")

	repo := sqlite.New(d)
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := repo.ExecutionByStep(ctx, id)
	if got.ID != e.ID || got.TaskID != task.ID || got.PlanHash != e.PlanHash || got.MaxParallel != 2 || got.Version != want.Version {
		t.Fatalf("execution = %+v", got)
	}
	b := got.StepByKey("backend")
	if b.State != domain.StepRunning || b.Fence != 1 || b.RunnerID != "r1" || b.LeaseExpiresAt == nil ||
		!b.LeaseExpiresAt.Equal(t0.Add(app.DefaultLeaseTTL)) || len(b.Attempts) != 1 || b.Attempts[0].State != domain.AttemptRunning {
		t.Fatalf("backend = %+v", b)
	}
	if f := got.StepByKey("frontend"); f.State != domain.StepPending || len(f.DependsOn) != 1 || f.DependsOn[0] != "backend" {
		t.Fatalf("frontend = %+v", f)
	}
	if li := got.StepByKey("lint"); li.State != domain.StepReady || li.Attempts[0].State != domain.AttemptFailed ||
		li.Attempts[0].Error != "exit 1" || li.Attempts[0].FinishedAt == nil {
		t.Fatalf("lint = %+v", li)
	}
	if run, err := repo.RunningExecution(ctx, task.ID); err != nil || run.ID != e.ID {
		t.Fatalf("running = %v", err)
	}
	if _, err := repo.ExecutionByStep(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown step = %v", err)
	}
	if _, err := repo.Execution(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown execution = %v", err)
	}
}

func TestOneRunningExecutionPerTask(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	_, e := running(t, svc)
	again := e.Clone()
	again.ID = "other"
	for i := range again.Steps {
		again.Steps[i].ID = fmt.Sprintf("other-%d", i)
	}
	if err := sqlite.New(d).CreateExecution(t.Context(), &again); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second running execution = %v", err)
	}
}

func TestStaleExecutionSaveIsAConflict(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	_, e := running(t, svc)
	repo := sqlite.New(d)
	id := e.StepByKey("backend").ID
	a, _ := repo.Execution(ctx, e.ID)
	b, _ := repo.Execution(ctx, e.ID)
	if _, err := a.Claim(id, "r1", t0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim(id, "r2", t0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveExecution(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveExecution(ctx, &b); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second claim from a stale read = %v", err)
	}
	if got, _ := repo.Execution(ctx, e.ID); got.StepByKey("backend").RunnerID != "r1" {
		t.Fatal("the losing claim overwrote the winner")
	}
}

func TestFenceNeverDecreasesInTheDatabase(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	_, e := running(t, svc)
	id := e.StepByKey("backend").ID
	if _, err := svc.Claim(t.Context(), id, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), "UPDATE workflow_steps SET fence = 0 WHERE id = ?", id); err == nil {
		t.Fatal("fence wound back")
	}
}

// TestConcurrentClaimsOneWinner is the acceptance race: runners connected through two separate database handles
// (as two Control Plane connections would be) claim the same step at once; exactly one gets the lease.
func TestConcurrentClaimsOneWinner(t *testing.T) {
	t.Parallel()
	d1, path := testDB(t)
	d2 := openDB(t, path)
	svcs := []*app.Service{service(d1, fixed), service(d2, fixed)}
	_, e := running(t, svcs[0])
	id := e.StepByKey("backend").ID

	const claimers = 8
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		mu     sync.Mutex
		leases []domain.Lease
		losses []error
	)
	for i := range claimers {
		wg.Go(func() {
			<-start
			l, err := svcs[i%2].Claim(t.Context(), id, fmt.Sprintf("r%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				losses = append(losses, err)
				return
			}
			leases = append(leases, l)
		})
	}
	close(start)
	wg.Wait()
	if len(leases) != 1 {
		t.Fatalf("%d claims won: %+v", len(leases), leases)
	}
	for _, err := range losses {
		if !errors.Is(err, domain.ErrInvalidTransition) && !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("losing claim failed with %v", err)
		}
	}
	got, _ := sqlite.New(d2).Execution(t.Context(), e.ID)
	if s := got.StepByKey("backend"); s.State != domain.StepClaimed || s.RunnerID != leases[0].RunnerID || s.Fence != 1 {
		t.Fatalf("stored step = %+v", s)
	}
}

func TestExpireLeasesSweepsOnlyExpiredExecutions(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	var now atomic.Pointer[time.Time]
	cur := t0
	now.Store(&cur)
	svc := service(d, func() time.Time { return *now.Load() })
	ctx := t.Context()
	task, e := running(t, svc)
	id := e.StepByKey("backend").ID
	l, _ := svc.Claim(ctx, id, "r1")
	_ = svc.StartStep(ctx, id, "r1", l.Fence)
	if n, err := svc.ExpireLeases(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep = %d, %v", n, err)
	}
	later := t0.Add(app.DefaultLeaseTTL)
	now.Store(&later)
	if n, err := svc.ExpireLeases(ctx); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	got, _ := svc.Execution(ctx, e.ID)
	s := got.StepByKey("backend")
	if s.State != domain.StepWaiting || s.WaitReason != domain.WaitRunner || s.Attempts[0].State != domain.AttemptUnknown {
		t.Fatalf("step = %+v", s)
	}
	if tk, _ := svc.Task(ctx, task.ID); tk.State != domain.TaskRunning {
		t.Fatalf("task = %s", tk.State)
	}
}

func TestCancelTaskIsAtomic(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	task, e := running(t, svc)
	// Make the execution save fail: the task must stay RUNNING.
	if _, err := d.ExecContext(ctx, `CREATE TRIGGER fail_cancel BEFORE UPDATE ON executions WHEN NEW.state = 'CANCELLED'
		BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelTask(ctx, task.ID, "stop"); err == nil {
		t.Fatal("cancel succeeded despite the failing execution save")
	}
	if tk, _ := svc.Task(ctx, task.ID); tk.State != domain.TaskRunning {
		t.Fatalf("task = %s after a rolled back cancel", tk.State)
	}
	if _, err := d.ExecContext(ctx, "DROP TRIGGER fail_cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelTask(ctx, task.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Execution(ctx, e.ID); got.State != domain.ExecutionCancelled || got.FinishedAt == nil {
		t.Fatalf("execution = %+v", got)
	}
}

// asProposed is a policy that keeps the planner's risk.
type asProposed struct{}

func (asProposed) EffectiveRisk(_ context.Context, _ string, p domain.Plan) (domain.Risk, error) {
	return p.Risk, nil
}

// The scheduler's and the task API's queries.
func TestSchedulingAndListingQueries(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, fixed)
	repo := sqlite.New(d)
	task, e := running(t, svc)
	other, err := svc.CreateTask(ctx, "p1", "Other", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if ids, err := repo.RunningExecutions(ctx); err != nil || len(ids) != 1 || ids[0] != e.ID {
		t.Fatalf("running %v %v", ids, err)
	}
	if ids, err := repo.TasksInState(ctx, domain.TaskCreated); err != nil || len(ids) != 1 || ids[0] != other.ID {
		t.Fatalf("created %v %v", ids, err)
	}
	if ids, _ := repo.ExecutionsHeldBy(ctx, "r1"); len(ids) != 0 {
		t.Fatalf("held before claim %v", ids)
	}
	if _, err := svc.Claim(ctx, e.StepByKey("backend").ID, "r1"); err != nil {
		t.Fatal(err)
	}
	if ids, err := repo.ExecutionsHeldBy(ctx, "r1"); err != nil || len(ids) != 1 || ids[0] != e.ID {
		t.Fatalf("held %v %v", ids, err)
	}
	list, err := repo.ListTasks(ctx, app.TaskFilter{ProjectIDs: []string{"p1", "nope"}}, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	if l, _ := repo.ListTasks(ctx, app.TaskFilter{ProjectIDs: []string{"p1"}}, 1); len(l) != 1 {
		t.Fatalf("limit %d", len(l))
	}
	if l, err := repo.ListTasks(ctx, app.TaskFilter{}, 10); err != nil || len(l) != 0 {
		t.Fatalf("no projects %v %v", l, err)
	}
	l, err := repo.ListTasks(ctx, app.TaskFilter{ProjectIDs: []string{"p1"}, States: []domain.TaskState{domain.TaskCreated, domain.TaskFailed}}, 10)
	if err != nil || len(l) != 1 || l[0].ID != other.ID {
		t.Fatalf("by state %+v %v", l, err)
	}
	if l, _ := repo.ListTasks(ctx, app.TaskFilter{TaskIDs: []string{task.ID}}, 10); len(l) != 1 || l[0].ID != task.ID {
		t.Fatalf("by id %+v", l)
	}
	if got, err := repo.LatestExecution(ctx, task.ID); err != nil || got.ID != e.ID || len(got.Steps) != 3 {
		t.Fatalf("latest %+v %v", got.ID, err)
	}
	if _, err := repo.LatestExecution(ctx, other.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("latest of a task without executions: %v", err)
	}
}

// Work periods, the agent and the incomplete mark survive a save and load; EachAttempt and ExecutionSpans feed the
// task's agent time and calendar duration (KIN-25).
func TestAgentTimeIsStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	now := t0
	svc := service(d, func() time.Time { return now })
	repo := sqlite.New(d)
	task, e := running(t, svc)
	back, lint := e.StepByKey("backend").ID, e.StepByKey("lint").ID
	for _, id := range []string{back, lint} {
		if _, err := svc.Claim(ctx, id, "r1"); err != nil {
			t.Fatal(err)
		}
		if err := svc.StartStepAt(ctx, id, "r1", 1, t0.Add(-time.Hour), "mock"); err != nil {
			t.Fatal(err)
		}
	}
	now = t0.Add(time.Minute)
	if err := svc.CompleteStepAt(ctx, back, "r1", 1, t0.Add(-time.Hour+1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := svc.FailStepAt(ctx, lint, "r1", 1, "boom", time.Time{}); err != nil { // unusable time
		t.Fatal(err)
	}
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := got.Step(back).Attempts[0]
	if a.Agent != "mock" || !a.WorkStart.Equal(t0.Add(-time.Hour)) || !a.WorkEnd.Equal(t0.Add(-time.Hour+1500*time.Millisecond)) || a.WorkIncomplete {
		t.Fatalf("backend attempt %+v", a)
	}
	if l := got.Step(lint).Attempts[0]; !l.WorkIncomplete || l.WorkEnd != nil {
		t.Fatalf("lint attempt %+v", l)
	}
	n := 0
	err = repo.EachAttempt(ctx, app.TaskFilter{ProjectIDs: []string{"p1"}, States: []domain.TaskState{domain.TaskRunning}},
		func(id string, a domain.Attempt) error {
			if id != task.ID {
				t.Errorf("attempt of %s", id)
			}
			n++
			return nil
		})
	if err != nil || n != 2 {
		t.Fatalf("EachAttempt %d %v", n, err)
	}
	if err := repo.EachAttempt(ctx, app.TaskFilter{TaskIDs: []string{"nope"}}, func(string, domain.Attempt) error {
		t.Error("attempt of another task")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	spans, err := repo.ExecutionSpans(ctx, []string{task.ID, "nope"})
	if err != nil || len(spans) != 1 || !spans[task.ID].Start.Equal(t0) || spans[task.ID].End != nil {
		t.Fatalf("running span %+v %v", spans, err)
	}
	if _, err := svc.CancelTask(ctx, task.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	spans, _ = repo.ExecutionSpans(ctx, []string{task.ID})
	if sp := spans[task.ID]; sp.End == nil || !sp.End.Equal(now) {
		t.Fatalf("finished span %+v", sp)
	}
}

// KIN-41: maintenance mode shows how many claimed or running steps still have to drain.
func TestActiveStepsCountsClaimedAndRunning(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	repo := sqlite.New(d)
	if n, err := repo.ActiveSteps(ctx); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	_, e := running(t, svc)
	id := e.StepByKey("backend").ID
	l, err := svc.Claim(ctx, id, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := repo.ActiveSteps(ctx); n != 1 {
		t.Fatalf("claimed: %d", n)
	}
	_ = svc.StartStep(ctx, id, "r1", l.Fence)
	lint := e.StepByKey("lint").ID
	_, _ = svc.Claim(ctx, lint, "r2")
	if n, _ := repo.ActiveSteps(ctx); n != 2 {
		t.Fatalf("running+claimed: %d", n)
	}
}

// KIN-65: the scheduler's load counts the steps a runner holds in running executions — CLAIMED, RUNNING and unknown
// attempts — and nothing else.
func TestHeldSteps(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	var now atomic.Int64
	now.Store(t0.UnixNano())
	svc := service(d, func() time.Time { return time.Unix(0, now.Load()).UTC() })
	repo := sqlite.New(d)
	task, e := running(t, svc)
	if held, err := repo.HeldSteps(ctx); err != nil || len(held) != 0 {
		t.Fatalf("held before claims %v %v", held, err)
	}
	l, err := svc.Claim(ctx, e.StepByKey("backend").ID, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Claim(ctx, e.StepByKey("lint").ID, "r2"); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartStep(ctx, e.StepByKey("backend").ID, "r1", l.Fence); err != nil {
		t.Fatal(err)
	}
	if held, err := repo.HeldSteps(ctx); err != nil || len(held) != 2 || held["r1"] != 1 || held["r2"] != 1 {
		t.Fatalf("held running+claimed %v %v", held, err)
	}
	now.Add(int64(2 * app.DefaultLeaseTTL)) // backend's attempt becomes unknown, lint's claim returns to READY
	if _, err := svc.ExpireLeases(ctx); err != nil {
		t.Fatal(err)
	}
	if held, err := repo.HeldSteps(ctx); err != nil || len(held) != 1 || held["r1"] != 1 {
		t.Fatalf("held after expiry %v %v", held, err)
	}
	if _, err := svc.CancelTask(ctx, task.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if held, err := repo.HeldSteps(ctx); err != nil || len(held) != 0 {
		t.Fatalf("held after the execution ended %v %v", held, err)
	}
}

// KIN-33: a step keeps the AI CLI and model of its claim, and a step or task may wait for an agent.
func TestStepAgentAndAgentWaitAreStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	svc := service(d, fixed)
	ctx := t.Context()
	_, e := running(t, svc)
	repo := sqlite.New(d)
	e, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, l := e.StepByKey("backend").ID, e.StepByKey("lint").ID
	if _, err := e.ClaimWith(b, "r1", domain.AgentChoice{Agent: "codex", Model: "gpt-5"}, t0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := e.WaitStep(l, domain.WaitAgent, t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveExecution(ctx, &e); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.StepByKey("backend"); s.Agent != "codex" || s.Model != "gpt-5" {
		t.Fatalf("backend agent %q model %q", s.Agent, s.Model)
	}
	if s := got.StepByKey("lint"); s.State != domain.StepWaiting || s.WaitReason != domain.WaitAgent {
		t.Fatalf("lint = %s/%s", s.State, s.WaitReason)
	}
}

// KIN-79: the CLI's report (session, model, version, usage) is stored with the attempt and the cli_limit wait
// keeps its retry time; both survive a save and load.
func TestAgentReportAndCLILimitWaitAreStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, func() time.Time { return t0 })
	repo := sqlite.New(d)
	_, e := running(t, svc)
	back := e.StepByKey("backend").ID
	if _, err := svc.Claim(ctx, back, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartStepAt(ctx, back, "r1", 1, t0, "claude"); err != nil {
		t.Fatal(err)
	}
	r := domain.AgentReport{Session: "sess-1", Model: "claude-sonnet", Version: "2.1.0", Usage: domain.AgentUsage{
		InputTokens: 1200, OutputTokens: 300, CacheReadTokens: 50, CacheWriteTokens: 7, CostUSD: 0.125, Turns: 4}}
	if err := svc.RecordAgentReport(ctx, back, "r1", 1, r); err != nil {
		t.Fatal(err)
	}
	resets := t0.Add(2 * time.Hour)
	if err := svc.HoldStepForCLILimit(ctx, back, "r1", 1, "rate_limit", "cli_limit: rate_limit", resets, t0); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := got.Step(back)
	if st.Attempts[0].Report != r {
		t.Fatalf("report %+v", st.Attempts[0].Report)
	}
	if st.WaitReason != domain.WaitCLILimit || st.WaitUntil == nil || !st.WaitUntil.Equal(resets) {
		t.Fatalf("step %+v", st)
	}
}
