package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// fakeRouter is the routing context: one chain per role, an optional block detail, cool-downs and the calls made.
type fakeRouter struct {
	mu      sync.Mutex
	chains  map[string][]domain.AgentChoice
	blocked string
	cooling map[string]time.Time // runner/agent → until
	calls   []string             // "project role author"
}

func (r *fakeRouter) Route(_ context.Context, projectID, role, author string) (AgentRoute, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, projectID+" "+role+" "+author)
	cool := maps(r.cooling)
	return AgentRoute{Chain: slices.Clone(r.chains[role]), Blocked: r.blocked,
		Cooling: func(runnerID, agent string) (domain.Cooling, bool) {
			u, ok := cool[runnerID+"/"+agent]
			return domain.Cooling{Until: u, NeedsOwner: u.Equal(parkedUntil)}, ok
		}}, nil
}

// parkedUntil marks a fakeRouter cool-down that waits for the Owner (KIN-83).
var parkedUntil = t0.Add(7 * 24 * time.Hour)

func maps(m map[string]time.Time) map[string]time.Time {
	out := make(map[string]time.Time, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *fakeRouter) set(f func(r *fakeRouter)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func newRoutedEnv() (schedEnv, *fakeRouter) {
	e := newSchedEnv()
	r := &fakeRouter{chains: map[string][]domain.AgentChoice{
		"implementer": {{Agent: "codex", Model: "gpt-5"}, {Agent: "claude"}},
		"reviewer":    {{Agent: "claude", Model: "opus"}},
	}}
	e.svc.Agents = r
	return e, r
}

func cliRunner(id string, tools ...string) Candidate {
	return Candidate{ID: id, Tools: append([]string{"git"}, tools...), DiskHeavyAllowed: true}
}

func (d *fakeDispatcher) takeAssigned() []Assignment {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.assigned
	d.assigned, d.cmds = nil, nil
	return out
}

// Acceptance (KIN-33): routing System → Project → Role → runner capabilities → session availability: the step
// goes to a runner that has the first permitted CLI of its role's chain, and the assignment names CLI, model and role.
func TestScheduleRoutesTheStepToARunnerWithTheBoundCLI(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	task := e.task(t, onlyStep("git"))
	e.pool.set("p1", cliRunner("r1", "claude"), cliRunner("r2", "codex"))
	e.schedule(t)
	got := e.disp.takeAssigned()
	if len(got) != 1 || got[0].Agent != "codex" || got[0].Model != "gpt-5" || got[0].Role != "implementer" {
		t.Fatalf("assignments %+v", got)
	}
	_, ex := e.state(t, task.ID)
	if st := ex.Steps[0]; st.RunnerID != "r2" || st.Agent != "codex" || st.Model != "gpt-5" {
		t.Fatalf("step %+v", st)
	}
	if !slices.Equal(r.calls, []string{"p1 implementer "}) {
		t.Fatalf("router calls %v", r.calls)
	}
}

// Acceptance: the fallback is used only when no runner can run the primary CLI.
func TestScheduleFallsBackInBindingOrder(t *testing.T) {
	t.Parallel()
	e, _ := newRoutedEnv()
	e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude"))
	e.schedule(t)
	if got := e.disp.takeAssigned(); len(got) != 1 || got[0].Agent != "claude" || got[0].Model != "" {
		t.Fatalf("assignments %+v", got)
	}
}

// Acceptance: nothing permitted (CLIs disabled) → the step WAITs for an agent and the task is BLOCKED(agent) with
// the router's explicit reason; it resumes by itself once a CLI is enabled again. No silent fallback: a runner with
// a disabled CLI is never used.
func TestNoPermittedCLIBlocksTheTaskWithAReason(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	const why = "no permitted AI CLI for role implementer: codex, claude disabled"
	r.set(func(r *fakeRouter) { r.chains["implementer"], r.blocked = nil, why })
	e.schedule(t)
	got, ex := e.state(t, task.ID)
	if got.State != domain.TaskBlocked || got.BlockReason != domain.WaitAgent || got.Reason.Detail != why {
		t.Fatalf("task %+v", got)
	}
	if st := ex.Steps[0]; st.State != domain.StepWaiting || st.WaitReason != domain.WaitAgent {
		t.Fatalf("step %+v", st)
	}
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("commands %v", c)
	}
	r.set(func(r *fakeRouter) { r.chains["implementer"], r.blocked = []domain.AgentChoice{{Agent: "claude"}}, "" })
	e.schedule(t)
	got, ex = e.state(t, task.ID)
	if got.State != domain.TaskRunning || ex.Steps[0].State != domain.StepClaimed || ex.Steps[0].Agent != "claude" {
		t.Fatalf("after enabling: task %s step %+v", got.State, ex.Steps[0])
	}
}

// Every runner with the bound CLIs cools down after a usage limit: BLOCKED(cli_limit) with the reset time; the
// reason follows the situation (a runner going away turns it into BLOCKED(runner)) and the step is assigned once
// the cool-down ends.
func TestCoolingCLIsBlockTheTaskForTheCLILimit(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	until := t0.Add(time.Hour)
	r.set(func(r *fakeRouter) { r.cooling = map[string]time.Time{"r1/codex": until, "r1/claude": until} })
	e.schedule(t)
	got, ex := e.state(t, task.ID)
	if got.State != domain.TaskBlocked || got.BlockReason != domain.WaitCLILimit ||
		!strings.Contains(got.Reason.Detail, until.Format("15:04")) || ex.Steps[0].WaitReason != domain.WaitCLILimit {
		t.Fatalf("task %+v step %+v", got, ex.Steps[0])
	}
	e.pool.set("p1")
	e.schedule(t)
	if got, ex = e.state(t, task.ID); got.BlockReason != domain.WaitRunner || ex.Steps[0].WaitReason != domain.WaitRunner {
		t.Fatalf("no runner: task %+v step %+v", got, ex.Steps[0])
	}
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	r.set(func(r *fakeRouter) { r.cooling = map[string]time.Time{"r1/codex": until} })
	e.schedule(t)
	if got := e.disp.takeAssigned(); len(got) != 1 || got[0].Agent != "claude" {
		t.Fatalf("after claude reset: %+v", got)
	}
}

// Acceptance: a reviewer step asks routing for the reviewer role with the CLI that authored its dependencies, so
// different-from-author can pick another CLI.
func TestReviewerStepIsRoutedWithTheAuthorsCLI(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	p := domain.Plan{Risk: domain.RiskL1, MaxParallel: 1, Summary: "s", Steps: []domain.PlanStep{
		{Key: "impl", Title: "Implement", Repository: "api"},
		{Key: "review", Title: "Review", Repository: "api", DependsOn: []string{"impl"}, Role: "reviewer"}}}
	task := e.task(t, p)
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	impl := ex.StepByKey("impl")
	if err := e.svc.StartStep(t.Context(), impl.ID, "r1", impl.Fence); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteStep(t.Context(), impl.ID, "r1", impl.Fence); err != nil {
		t.Fatal(err)
	}
	e.disp.takeAssigned()
	// The implementer step's own independent review (KIN-35) comes first, then the plan's reviewer step.
	e.schedule(t)
	if got := e.disp.takeAssigned(); len(got) != 1 || got[0].StepKey != "impl" || got[0].Role != "reviewer" || got[0].Agent != "claude" {
		t.Fatalf("in-step review assignment %+v", got)
	}
	_, ex = e.state(t, task.ID)
	impl = ex.StepByKey("impl")
	if err := e.svc.StartStep(t.Context(), impl.ID, "r1", impl.Fence); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RecordAgentReport(t.Context(), impl.ID, "r1", impl.Fence,
		domain.AgentReport{Result: `{"verdict":"approve","summary":"ok","findings":[]}`}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteStep(t.Context(), impl.ID, "r1", impl.Fence); err != nil {
		t.Fatal(err)
	}
	e.schedule(t)
	got := e.disp.takeAssigned()
	if len(got) != 1 || got[0].StepKey != "review" || got[0].Agent != "claude" || got[0].Role != "reviewer" {
		t.Fatalf("review assignment %+v", got)
	}
	if last := r.calls[len(r.calls)-1]; last != "p1 reviewer codex" {
		t.Fatalf("reviewer routed as %q, want author codex", last)
	}
}

// Mock plans run the mock agent: no routing, no CLI switches (no AI).
func TestMockPlansAreNotRouted(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	ctx := t.Context()
	task, err := e.svc.CreateTask(ctx, "p1", "Mock", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	mp, _ := MockPlan([]string{"api"}, "success")
	if _, err := e.svc.SubmitPlan(ctx, task.ID, mp, MockPlanner); err != nil {
		t.Fatal(err)
	}
	r.set(func(r *fakeRouter) { r.chains, r.blocked = nil, "everything disabled" })
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	if got := e.disp.takeAssigned(); len(got) != 1 || got[0].Agent != AgentMock {
		t.Fatalf("mock assignment %+v", got)
	}
	if len(r.calls) != 0 {
		t.Fatalf("mock plan routed: %v", r.calls)
	}
}

// Acceptance: the Owner's emergency stop of a CLI cancels its running attempts at once (runner command) without
// using their retry budget; the steps are routed again (here: to the fallback CLI).
func TestStopAgentCancelsItsAttempts(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := run(t, e, ex)
	e.disp.takeAssigned()
	n, err := e.svc.StopAgent(t.Context(), "codex")
	if err != nil || n != 1 {
		t.Fatalf("StopAgent = %d, %v", n, err)
	}
	if c := e.disp.take(); !slices.Equal(c, []string{fmt.Sprintf("cancel r1 %s/%d %s", st.ID, st.Fence, CancelReasonAgentStopped)}) {
		t.Fatalf("commands %v", c)
	}
	_, ex = e.state(t, task.ID)
	if s := ex.Steps[0]; s.State != domain.StepWaiting || s.WaitReason != domain.WaitAgent ||
		s.Attempts[0].State != domain.AttemptCancelled {
		t.Fatalf("step %+v", s)
	}
	r.set(func(r *fakeRouter) { r.chains["implementer"] = []domain.AgentChoice{{Agent: "claude"}} })
	e.schedule(t)
	if got := e.disp.takeAssigned(); len(got) != 1 || got[0].Agent != "claude" {
		t.Fatalf("re-routed %+v", got)
	}
}

// Acceptance (KIN-83): a step held because its CLI is logged out on one runner moves at once to another permitted
// runner (or CLI); when only the Owner can bring the CLI back everywhere, the task is BLOCKED(cli_limit) saying so,
// and a success on the pair ends routing's streak.
func TestLoggedOutCLIHoldMovesTheStepOrWaitsForTheOwner(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	r.set(func(r *fakeRouter) { r.chains["implementer"] = []domain.AgentChoice{{Agent: "claude"}} })
	limits := &fakeCLILimits{hold: CLIHold{Known: true, Until: parkedUntil, NeedsOwner: true}}
	e.svc.CLILimits = limits
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude"))
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := ex.Steps[0]
	e.disp.takeAssigned()
	if err := e.svc.StartStep(t.Context(), st.ID, "r1", st.Fence); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HoldStepForCLILimit(t.Context(), st.ID, "r1", st.Fence, "auth", "cli_limit: auth: not logged in",
		time.Time{}, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	got, ex := e.state(t, task.ID)
	if !strings.Contains(got.Reason.Detail, "waiting for the Owner") || !strings.Contains(got.Reason.Detail, "runner r1") ||
		ex.Steps[0].WaitUntil == nil || !ex.Steps[0].WaitUntil.Equal(parkedUntil) {
		t.Fatalf("task %+v step %+v", got.Reason, ex.Steps[0])
	}
	if !slices.Equal(limits.got, []string{"r1 claude auth 0001-01-01T00:00:00Z"}) {
		t.Fatalf("cli limit hits %v", limits.got)
	}

	// Only r1 has claude and it is parked: the task stays blocked and names the Owner's action.
	r.set(func(r *fakeRouter) { r.cooling = map[string]time.Time{"r1/claude": parkedUntil} })
	e.schedule(t)
	if got, _ := e.state(t, task.ID); got.State != domain.TaskBlocked || got.BlockReason != domain.WaitCLILimit ||
		!strings.Contains(got.Reason.Detail, "waiting for the Owner: claude on runner r1") {
		t.Fatalf("task %s %+v", got.State, got.Reason)
	}
	if a := e.disp.takeAssigned(); len(a) != 0 {
		t.Fatalf("assigned to a parked CLI: %+v", a)
	}

	// Another runner with claude: the step moves there at once and the task resumes.
	e.pool.set("p1", cliRunner("r1", "claude"), cliRunner("r2", "claude"))
	e.schedule(t)
	a := e.disp.takeAssigned()
	if len(a) != 1 || a[0].Agent != "claude" {
		t.Fatalf("assignments %+v", a)
	}
	got, ex = e.state(t, task.ID)
	if got.State != domain.TaskRunning || ex.Steps[0].RunnerID != "r2" {
		t.Fatalf("task %s step %+v", got.State, ex.Steps[0])
	}
	st = ex.Steps[0]
	if err := e.svc.StartStep(t.Context(), st.ID, "r2", st.Fence); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteStep(t.Context(), st.ID, "r2", st.Fence); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(limits.worked, []string{"r2 claude"}) {
		t.Fatalf("worked %v", limits.worked)
	}
}
