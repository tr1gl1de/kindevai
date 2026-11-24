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
	return &app.Service{Repo: sqlite.New(d), Projects: projects{}, Tx: db.Transactor{DB: d}, Now: now,
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
	if _, err := svc.SubmitPlan(ctx, task.ID, plan(), domain.RiskL1, "planner"); err != nil {
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
	rev, err := svc.SubmitPlan(ctx, task.ID, plan(), domain.RiskL2, "planner")
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
