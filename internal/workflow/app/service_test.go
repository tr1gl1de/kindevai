package app

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type env struct {
	svc   *Service
	repo  *memRepo
	clock *clock
}

func newEnv() env {
	repo, c := newMemRepo(), &clock{now: t0}
	var n atomic.Int64
	svc := &Service{Repo: repo, Projects: fakeProjects{"p1": {"api", "web"}}, Tx: &memTx{}, Now: c.Now,
		NewID: func() string { return fmt.Sprintf("id%d", n.Add(1)) }}
	return env{svc: svc, repo: repo, clock: c}
}

func plan(risk domain.Risk) domain.Plan {
	return domain.Plan{Risk: risk, MaxParallel: 2, Summary: "s", Steps: []domain.PlanStep{
		{Key: "backend", Title: "API", Repository: "api"},
		{Key: "frontend", Title: "UI", Repository: "web", DependsOn: []string{"backend"}},
	}}
}

// readyTask creates a task with an accepted L1 plan.
func (e env) readyTask(t *testing.T) domain.Task {
	t.Helper()
	ctx := t.Context()
	task, err := e.svc.CreateTask(ctx, "p1", "Login", "KIN-7", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SubmitPlan(ctx, task.ID, plan(domain.RiskL1), domain.RiskL1, "planner"); err != nil {
		t.Fatal(err)
	}
	task, _ = e.repo.Task(ctx, task.ID)
	if task.State != domain.TaskReady {
		t.Fatalf("task = %s", task.State)
	}
	return task
}

func TestCreateTaskKeepsTheOptionalYouTrackIssue(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task, err := e.svc.CreateTask(ctx, "p1", "Login", "KIN-7", "u1")
	if err != nil || task.YouTrackIssue != "KIN-7" || task.State != domain.TaskCreated || !task.CreatedAt.Equal(t0) {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if _, err := e.svc.CreateTask(ctx, "p1", "Again", "KIN-7", "u1"); !errors.Is(err, ErrDuplicateIssue) {
		t.Fatalf("second task for one issue = %v", err)
	}
	manual, err := e.svc.CreateTask(ctx, "p1", "Manual", "", "u1")
	if err != nil || manual.YouTrackIssue != "" {
		t.Fatalf("manual task = %+v, %v", manual, err)
	}
	if _, err := e.svc.CreateTask(ctx, "p1", "", "", "u1"); err == nil {
		t.Fatal("empty title accepted")
	}
}

func TestSubmitPlanValidatesTheDAGAgainstTheProjectsRepositories(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task, _ := e.svc.CreateTask(ctx, "p1", "Login", "", "u1")
	if _, err := e.svc.SubmitPlan(ctx, task.ID, plan(domain.RiskL1), domain.RiskL1, "planner"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("plan before analysis = %v", err)
	}
	_, _ = e.svc.StartAnalysis(ctx, task.ID)
	foreign := plan(domain.RiskL1)
	foreign.Steps[1].Repository = "someone-elses"
	var ve *domain.ValidationError
	if _, err := e.svc.SubmitPlan(ctx, task.ID, foreign, domain.RiskL1, "planner"); !errors.As(err, &ve) || ve.Code != "repository_not_in_project" {
		t.Fatalf("foreign repository = %v", err)
	}
	if _, err := e.repo.Revision(ctx, task.ID, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("a rejected plan must not be stored")
	}
	if got, _ := e.repo.Task(ctx, task.ID); got.State != domain.TaskAnalyzing || got.PlanRevision != 0 {
		t.Fatalf("task = %+v", got)
	}
}

func TestPlanRevisionsRouteByRiskAndStayImmutable(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task, _ := e.svc.CreateTask(ctx, "p1", "Login", "", "u1")
	_, _ = e.svc.StartAnalysis(ctx, task.ID)
	r1, err := e.svc.SubmitPlan(ctx, task.ID, plan(domain.RiskL1), domain.RiskL2, "planner") // policy raised the risk
	if err != nil || r1.Number() != 1 || r1.Risk() != domain.RiskL2 {
		t.Fatalf("revision 1 = %+v, %v", r1, err)
	}
	if got, _ := e.repo.Task(ctx, task.ID); got.State != domain.TaskAwaitingApproval || got.PlanRevision != 1 {
		t.Fatalf("task = %+v", got)
	}
	// The plan chat produces revision 2; revision 1 is kept as it was and is no longer current.
	p2 := plan(domain.RiskL0)
	p2.Summary = "smaller"
	r2, err := e.svc.SubmitPlan(ctx, task.ID, p2, domain.RiskL0, "chat")
	if err != nil || r2.Number() != 2 || r2.Hash() == r1.Hash() {
		t.Fatalf("revision 2 = %+v, %v", r2, err)
	}
	if got, _ := e.repo.Task(ctx, task.ID); got.State != domain.TaskReady || got.PlanRevision != 2 {
		t.Fatalf("task = %+v", got)
	}
	if old, _ := e.repo.Revision(ctx, task.ID, 1); old.Hash() != r1.Hash() || old.Plan().Summary != "s" {
		t.Fatal("revision 1 changed")
	}
}

func TestStartExecutionRunsTheCurrentRevision(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task, _ := e.svc.CreateTask(ctx, "p1", "Login", "", "u1")
	if _, err := e.svc.StartExecution(ctx, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("execution of a created task = %v", err)
	}
	task = e.readyTask(t)
	ex, err := e.svc.StartExecution(ctx, task.ID)
	if err != nil || ex.PlanRevision != 1 || ex.State != domain.ExecutionRunning || ex.StepByKey("backend").State != domain.StepReady {
		t.Fatalf("execution = %+v, %v", ex, err)
	}
	if got, _ := e.repo.Task(ctx, task.ID); got.State != domain.TaskRunning {
		t.Fatalf("task = %s", got.State)
	}
	if _, err := e.svc.StartExecution(ctx, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second execution = %v", err)
	}
}

func TestStepsRunToCompletionAndTheTaskMovesToVerifying(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	for _, key := range []string{"backend", "frontend"} {
		cur, _ := e.repo.Execution(ctx, ex.ID)
		id := cur.StepByKey(key).ID
		lease, err := e.svc.Claim(ctx, id, "r1")
		if err != nil || lease.Fence != 1 || !lease.ExpiresAt.Equal(e.clock.Now().Add(DefaultLeaseTTL)) {
			t.Fatalf("claim %s = %+v, %v", key, lease, err)
		}
		if err := e.svc.StartStep(ctx, id, "r1", lease.Fence); err != nil {
			t.Fatal(err)
		}
		e.clock.Add(30 * time.Second)
		if exp, err := e.svc.RenewLease(ctx, id, "r1", lease.Fence); err != nil || !exp.Equal(e.clock.Now().Add(DefaultLeaseTTL)) {
			t.Fatalf("renew = %v, %v", exp, err)
		}
		if err := e.svc.CompleteStep(ctx, id, "r1", lease.Fence); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := e.repo.Execution(ctx, ex.ID)
	if got.State != domain.ExecutionSucceeded {
		t.Fatalf("execution = %s", got.State)
	}
	if tk, _ := e.repo.Task(ctx, task.ID); tk.State != domain.TaskVerifying {
		t.Fatalf("task = %s", tk.State)
	}
}

func TestStaleFencingTokenIsRejectedAndChangesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	id := ex.StepByKey("backend").ID
	old, _ := e.svc.Claim(ctx, id, "r1")
	e.clock.Add(DefaultLeaseTTL)
	if n, err := e.svc.ExpireLeases(ctx); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	fresh, err := e.svc.Claim(ctx, id, "r2")
	if err != nil || fresh.Fence != old.Fence+1 {
		t.Fatalf("re-claim = %+v, %v", fresh, err)
	}
	before, _ := e.repo.Execution(ctx, ex.ID)
	if err := e.svc.StartStep(ctx, id, "r1", old.Fence); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale start = %v", err)
	}
	if err := e.svc.CompleteStep(ctx, id, "r1", old.Fence); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale complete = %v", err)
	}
	if after, _ := e.repo.Execution(ctx, ex.ID); after.Version != before.Version {
		t.Fatal("a rejected report was saved")
	}
}

func TestFailedStepIsRetriedOnceThenTheTaskFails(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	id := ex.StepByKey("backend").ID
	for i := range 2 {
		lease, err := e.svc.Claim(ctx, id, "r1")
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		_ = e.svc.StartStep(ctx, id, "r1", lease.Fence)
		if err := e.svc.FailStep(ctx, id, "r1", lease.Fence, "exit 1"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := e.repo.Execution(ctx, ex.ID)
	if got.State != domain.ExecutionFailed || len(got.StepByKey("backend").Attempts) != DefaultMaxAttempts {
		t.Fatalf("execution = %+v", got)
	}
	tk, _ := e.repo.Task(ctx, task.ID)
	if tk.State != domain.TaskFailed || tk.Reason.Detail == "" {
		t.Fatalf("task = %+v", tk)
	}
}

func TestBlockResumeAndCancelTask(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	got, err := e.svc.BlockTask(ctx, task.ID, domain.WaitNetwork, "proxy down")
	if err != nil || got.State != domain.TaskBlocked || got.BlockReason != domain.WaitNetwork {
		t.Fatalf("block = %+v, %v", got, err)
	}
	if got, err = e.svc.ResumeTask(ctx, task.ID); err != nil || got.State != domain.TaskRunning {
		t.Fatalf("resume = %+v, %v", got, err)
	}
	if _, err := e.svc.CancelTask(ctx, task.ID, ""); err == nil {
		t.Fatal("cancel without a reason")
	}
	if got, err = e.svc.CancelTask(ctx, task.ID, "not needed"); err != nil || got.State != domain.TaskCancelled {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if cur, _ := e.repo.Execution(ctx, ex.ID); cur.State != domain.ExecutionCancelled {
		t.Fatalf("execution = %s", cur.State)
	}
	if _, err := e.svc.Claim(ctx, ex.StepByKey("backend").ID, "r1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("claim after cancel = %v", err)
	}
}

func TestExecutionFinishingWhileTheTaskIsBlocked(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	id := ex.StepByKey("backend").ID
	lease, _ := e.svc.Claim(ctx, id, "r1")
	_ = e.svc.StartStep(ctx, id, "r1", lease.Fence)
	_ = e.svc.FailStep(ctx, id, "r1", lease.Fence, "x")
	lease, _ = e.svc.Claim(ctx, id, "r1")
	_ = e.svc.StartStep(ctx, id, "r1", lease.Fence)
	if _, err := e.svc.BlockTask(ctx, task.ID, domain.WaitCLILimit, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FailStep(ctx, id, "r1", lease.Fence, "x"); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e.repo.Task(ctx, task.ID); tk.State != domain.TaskFailed {
		t.Fatalf("task = %s", tk.State)
	}
}

func TestUnknownIDs(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if _, err := e.svc.Claim(ctx, "nope", "r1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("claim = %v", err)
	}
	if _, err := e.svc.StartAnalysis(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("analysis = %v", err)
	}
	if _, err := e.svc.CreateTask(ctx, "p-unknown", "x", "", "u1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project = %v", err)
	}
}

func TestClaimRequiresARunningTask(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	_, _ = e.svc.BlockTask(ctx, task.ID, domain.WaitApproval, "scope grew")
	if _, err := e.svc.Claim(ctx, ex.StepByKey("backend").ID, "r1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("claim of a blocked task's step = %v", err)
	}
	_, _ = e.svc.ResumeTask(ctx, task.ID)
	if _, err := e.svc.Claim(ctx, ex.StepByKey("backend").ID, "r1"); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileStep(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	id := ex.StepByKey("backend").ID
	l, _ := e.svc.Claim(ctx, id, "r1")
	_ = e.svc.StartStep(ctx, id, "r1", l.Fence)
	e.clock.Add(DefaultLeaseTTL)
	_, _ = e.svc.ExpireLeases(ctx)
	if err := e.svc.ReconcileStep(ctx, id, domain.ProcessGone); err != nil {
		t.Fatal(err)
	}
	if cur, _ := e.repo.Execution(ctx, ex.ID); cur.StepByKey("backend").State != domain.StepReady {
		t.Fatalf("step = %s", cur.StepByKey("backend").State)
	}
}
