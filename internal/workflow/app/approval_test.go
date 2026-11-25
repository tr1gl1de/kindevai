package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// fakePolicy keeps the planner's risk, or raises it to raise; lower returns a level below the proposal (a broken
// policy) to prove the workflow refuses it.
type fakePolicy struct {
	raise domain.Risk
	lower bool
}

func (f *fakePolicy) EffectiveRisk(_ context.Context, projectID string, p domain.Plan) (domain.Risk, error) {
	if projectID == "" {
		return "", errors.New("no project")
	}
	switch {
	case f.lower:
		return domain.RiskL0, nil
	case f.raise != "" && f.raise.AtLeast(p.Risk):
		return f.raise, nil
	}
	return p.Risk, nil
}

type fakeEvents struct {
	mu       sync.Mutex
	accepted []int // revision numbers
	closed   []string
	verified []string // task:revision of each entry into VERIFYING (KIN-106)
	fail     error
}

func (f *fakeEvents) TaskVerifying(_ context.Context, t domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified = append(f.verified, fmt.Sprintf("%s:%d", t.ID, t.PlanRevision))
	return nil
}

func (f *fakeEvents) verifying() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.verified)
}

func (f *fakeEvents) PlanAccepted(_ context.Context, _ domain.Task, rev domain.PlanRevision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.accepted = append(f.accepted, rev.Number())
	return nil
}

func (f *fakeEvents) TaskClosed(_ context.Context, t domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, t.ID+":"+string(t.State))
	return nil
}

func (e env) analyzingTask(t *testing.T) domain.Task {
	t.Helper()
	task, err := e.svc.CreateTask(t.Context(), "p1", "Login", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestSubmitPlanTakesTheRiskFromThePolicyNotTheCaller(t *testing.T) {
	t.Parallel()
	e := newEnv()
	task := e.analyzingTask(t)
	e.policy.raise = domain.RiskL3
	rev, err := e.svc.SubmitPlan(t.Context(), task.ID, plan(domain.RiskL0), "planner")
	if err != nil || rev.Risk() != domain.RiskL3 || rev.Plan().Risk != domain.RiskL0 {
		t.Fatalf("revision = %+v, %v", rev, err)
	}
	if got, _ := e.repo.Task(t.Context(), task.ID); got.State != domain.TaskAwaitingApproval {
		t.Fatalf("L3 task = %s, want AWAITING_APPROVAL", got.State)
	}
	if len(e.events.accepted) != 1 || e.events.accepted[0] != 1 {
		t.Fatalf("PlanAccepted events = %v", e.events.accepted)
	}
}

func TestSubmitPlanRefusesAPolicyThatLowersTheRisk(t *testing.T) {
	t.Parallel()
	e := newEnv()
	task := e.analyzingTask(t)
	e.policy.lower = true
	var ve *domain.ValidationError
	if _, err := e.svc.SubmitPlan(t.Context(), task.ID, plan(domain.RiskL2), "planner"); !errors.As(err, &ve) || ve.Code != "risk_below_proposed" {
		t.Fatalf("lowered risk = %v", err)
	}
	if _, err := e.repo.Revision(t.Context(), task.ID, 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("no revision may be stored")
	}
}

func TestSubmitPlanRollsBackWhenTheListenerFails(t *testing.T) {
	t.Parallel()
	e := newEnv()
	task := e.analyzingTask(t)
	e.events.fail = errors.New("approvals down")
	if _, err := e.svc.SubmitPlan(t.Context(), task.ID, plan(domain.RiskL2), "planner"); err == nil {
		t.Fatal("listener error swallowed")
	}
	if got, _ := e.repo.Task(t.Context(), task.ID); got.PlanRevision != 0 || got.State != domain.TaskAnalyzing {
		t.Fatalf("task changed: %+v", got)
	}
}

func TestApproveAndRejectPlanBindToRevisionAndHash(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.analyzingTask(t)
	r1, _ := e.svc.SubmitPlan(ctx, task.ID, plan(domain.RiskL2), "planner")
	p2 := plan(domain.RiskL2)
	p2.Summary = "v2"
	r2, err := e.svc.SubmitPlan(ctx, task.ID, p2, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ApprovePlan(ctx, task.ID, r1.Number(), r1.Hash()); !errors.Is(err, domain.ErrStaleRevision) {
		t.Fatalf("approve of revision 1 = %v", err)
	}
	if _, err := e.svc.ApprovePlan(ctx, task.ID, r2.Number(), r1.Hash()); !errors.Is(err, domain.ErrStaleRevision) {
		t.Fatalf("approve with another hash = %v", err)
	}
	if _, err := e.svc.RejectPlan(ctx, task.ID, r2.Number(), strings.Repeat("0", 64)); !errors.Is(err, domain.ErrStaleRevision) {
		t.Fatalf("reject with another hash = %v", err)
	}
	got, err := e.svc.RejectPlan(ctx, task.ID, r2.Number(), r2.Hash())
	if err != nil || got.State != domain.TaskAnalyzing {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	p3 := plan(domain.RiskL2)
	p3.Summary = "v3"
	r3, _ := e.svc.SubmitPlan(ctx, task.ID, p3, "chat")
	got, err = e.svc.ApprovePlan(ctx, task.ID, r3.Number(), r3.Hash())
	if err != nil || got.State != domain.TaskReady {
		t.Fatalf("approve = %+v, %v", got, err)
	}
}

func TestHoldAndResumeAStepForApproval(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, err := e.svc.StartExecution(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := ex.StepByKey("backend").ID
	l, _ := e.svc.Claim(ctx, id, "r1")
	if err := e.svc.StartStep(ctx, id, "r1", l.Fence); err != nil {
		t.Fatal(err)
	}
	info, err := e.svc.StepInfo(ctx, id)
	if err != nil || info.Step.Key != "backend" || info.Revision.Number() != 1 || info.Task.ID != task.ID {
		t.Fatalf("step info = %+v, %v", info, err)
	}
	if err := e.svc.HoldStep(ctx, id, "r1", l.Fence, "outside scope"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Execution(ctx, ex.ID)
	if s := got.Step(id); s.State != domain.StepWaiting || s.WaitReason != domain.WaitApproval {
		t.Fatalf("held step = %+v", s)
	}
	if err := e.svc.ResumeHeldStep(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResumeHeldStep(ctx, id); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("resuming a step that is not held = %v", err)
	}
	got, _ = e.svc.Execution(ctx, ex.ID)
	if got.Step(id).State != domain.StepReady {
		t.Fatalf("resumed step = %s", got.Step(id).State)
	}
}

func TestClosingATaskNotifiesTheListener(t *testing.T) {
	t.Parallel()
	e := newEnv()
	task := e.readyTask(t)
	if _, err := e.svc.CancelTask(t.Context(), task.ID, "not needed"); err != nil {
		t.Fatal(err)
	}
	if len(e.events.closed) != 1 || e.events.closed[0] != task.ID+":CANCELLED" {
		t.Fatalf("closed events = %v", e.events.closed)
	}
}
