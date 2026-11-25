package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type fakePool struct {
	mu    sync.Mutex
	cands map[string][]Candidate // project → candidates
}

func (p *fakePool) Candidates(_ context.Context, projectID string) ([]Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.cands[projectID]), nil
}

func (p *fakePool) set(projectID string, cs ...Candidate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cands == nil {
		p.cands = map[string][]Candidate{}
	}
	p.cands[projectID] = cs
}

type fakeDispatcher struct {
	mu       sync.Mutex
	cmds     []string
	assigned []Assignment
}

func (d *fakeDispatcher) Assign(_ context.Context, runnerID string, a Assignment) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.assigned = append(d.assigned, a)
	d.cmds = append(d.cmds, fmt.Sprintf("assign %s %s/%d %s %s", runnerID, a.StepKey, a.Fence, a.ProjectID, a.Repository))
	return nil
}

func (d *fakeDispatcher) Cancel(_ context.Context, runnerID, stepID string, fence int64, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cmds = append(d.cmds, fmt.Sprintf("cancel %s %s/%d %s", runnerID, stepID, fence, reason))
	return nil
}

func (d *fakeDispatcher) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.cmds
	d.cmds = nil
	return out
}

type schedEnv struct {
	env
	pool *fakePool
	disp *fakeDispatcher
}

func newSchedEnv() schedEnv {
	e := newEnv()
	e.svc.Projects = fakeProjects{"p1": {"api", "web"}}
	pool, disp := &fakePool{}, &fakeDispatcher{}
	e.svc.Runners, e.svc.Dispatcher = pool, disp
	return schedEnv{env: e, pool: pool, disp: disp}
}

func onlyStep(caps ...string) domain.Plan {
	return domain.Plan{Risk: domain.RiskL1, MaxParallel: 1, Summary: "s", Steps: []domain.PlanStep{
		{Key: "only", Title: "Only", Repository: "api", Capabilities: caps, Scope: []string{"src/"}}}}
}

func (e schedEnv) task(t *testing.T, p domain.Plan) domain.Task {
	t.Helper()
	ctx := t.Context()
	task, err := e.svc.CreateTask(ctx, "p1", "Mock", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SubmitPlan(ctx, task.ID, p, "planner"); err != nil {
		t.Fatal(err)
	}
	return task
}

func (e schedEnv) schedule(t *testing.T) {
	t.Helper()
	if err := e.svc.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (e schedEnv) state(t *testing.T, taskID string) (domain.Task, domain.Execution) {
	t.Helper()
	task, err := e.repo.Task(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := e.repo.LatestExecution(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task, ex
}

// The scheduler starts a READY (L0/L1, or approved) task and assigns its READY step only to a candidate runner with
// the step's capabilities and disk headroom, queueing execution.assign with the claim's token.
func TestScheduleAssignsReadyStepsToAMatchingCandidate(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep("git"))
	e.pool.set("p1",
		Candidate{ID: "r-nogit", Tools: []string{"claude"}, DiskHeavyAllowed: true},
		Candidate{ID: "r-full", Tools: []string{"git"}, DiskHeavyAllowed: false},
		Candidate{ID: "r-ok", Tools: []string{"git", "claude"}, DiskHeavyAllowed: true})
	e.schedule(t)
	got, ex := e.state(t, task.ID)
	if got.State != domain.TaskRunning || ex.Steps[0].State != domain.StepClaimed || ex.Steps[0].RunnerID != "r-ok" {
		t.Fatalf("task %s step %+v", got.State, ex.Steps[0])
	}
	if c := e.disp.take(); !slices.Equal(c, []string{"assign r-ok only/1 p1 api"}) {
		t.Fatalf("commands %v", c)
	}
	e.schedule(t) // nothing new to assign
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("second pass queued %v", c)
	}
}

// No candidate: the step waits for a runner and the task is BLOCKED (reason runner) until one appears; then the
// task resumes by itself and the step is assigned.
func TestNoCandidateBlocksTheTaskUntilARunnerAppears(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.schedule(t)
	got, ex := e.state(t, task.ID)
	if got.State != domain.TaskBlocked || got.BlockReason != domain.WaitRunner || got.Reason.Detail != DetailNoRunner {
		t.Fatalf("task %+v", got)
	}
	if st := ex.Steps[0]; st.State != domain.StepWaiting || st.WaitReason != domain.WaitRunner {
		t.Fatalf("step %+v", st)
	}
	e.schedule(t) // still none: unchanged, nothing queued
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("commands %v", c)
	}
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	got, ex = e.state(t, task.ID)
	if got.State != domain.TaskRunning || ex.Steps[0].State != domain.StepClaimed {
		t.Fatalf("after a runner appeared: task %s step %s", got.State, ex.Steps[0].State)
	}
}

func run(t *testing.T, e schedEnv, ex domain.Execution) domain.Step {
	t.Helper()
	st := ex.Steps[0]
	if err := e.svc.StartStep(t.Context(), st.ID, st.RunnerID, st.Fence); err != nil {
		t.Fatal(err)
	}
	return st
}

// A lost runner: the lease runs out, the attempt becomes UNKNOWN and the step is not assigned again (no second
// process) — the task is BLOCKED until the runner reconciles. A live process returns to RUNNING under the same
// token; a gone one fails and is retried once with a new token; a second failure fails the task.
func TestLostRunnerIsReconciledBeforeAnyRetry(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true}, Candidate{ID: "r2", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := run(t, e, ex)
	e.disp.take()

	e.clock.Add(DefaultLeaseTTL + time.Second)
	e.schedule(t)
	got, ex := e.state(t, task.ID)
	if !ex.Steps[0].AttemptUnknown() || got.State != domain.TaskBlocked || got.Reason.Detail != DetailRunnerLost {
		t.Fatalf("after lease expiry: task %+v step %+v", got, ex.Steps[0])
	}
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("an unknown attempt was assigned again: %v", c)
	}

	// The runner comes back with the process alive: same token, RUNNING, task resumes.
	if err := e.svc.ReconcileRunner(t.Context(), "r1", []AttemptReport{{StepID: st.ID, Fence: st.Fence, Running: true}}); err != nil {
		t.Fatal(err)
	}
	e.schedule(t)
	got, ex = e.state(t, task.ID)
	if ex.Steps[0].State != domain.StepRunning || ex.Steps[0].Fence != st.Fence || got.State != domain.TaskRunning {
		t.Fatalf("after alive: task %s step %+v", got.State, ex.Steps[0])
	}

	// Lost again; this time the process is gone: the attempt fails and the step is retried with a new token.
	e.clock.Add(DefaultLeaseTTL + time.Second)
	if err := e.svc.ReconcileRunner(t.Context(), "r1", nil); err != nil {
		t.Fatal(err)
	}
	e.schedule(t)
	_, ex = e.state(t, task.ID)
	if s := ex.Steps[0]; s.State != domain.StepClaimed || s.Fence != st.Fence+1 || s.Attempts[0].State != domain.AttemptFailed {
		t.Fatalf("retry: %+v", s)
	}
	if c := e.disp.take(); len(c) != 1 {
		t.Fatalf("retry commands %v", c)
	}
	st = run(t, e, ex)
	// The retry's runner restarted and has no such process: the budget (1 retry) is used up.
	if err := e.svc.ReconcileRunner(t.Context(), st.RunnerID, nil); err != nil {
		t.Fatal(err)
	}
	got, ex = e.state(t, task.ID)
	if got.State != domain.TaskFailed || ex.State != domain.ExecutionFailed {
		t.Fatalf("after the second failure: task %s execution %s", got.State, ex.State)
	}
}

// A runner reporting a process the Control Plane no longer assigns to it is told to stop it; a kept result and a
// CLAIMED step whose assignment may still be in flight are left alone.
func TestReconcileCancelsStaleProcesses(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := ex.Steps[0]
	e.disp.take()
	err := e.svc.ReconcileRunner(t.Context(), "r1", []AttemptReport{
		{StepID: "old-step", Fence: 3, Running: true},
		{StepID: "done-step", Fence: 1, Result: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c := e.disp.take(); !slices.Equal(c, []string{"cancel r1 old-step/3 stale_fence"}) {
		t.Fatalf("commands %v", c)
	}
	_, ex = e.state(t, task.ID)
	if ex.Steps[0].State != domain.StepClaimed || ex.Steps[0].Fence != st.Fence {
		t.Fatalf("claimed step changed: %+v", ex.Steps[0])
	}
}

// Cancelling the task stops the runner's process group (execution.cancel in the same transaction).
func TestCancelTaskCancelsRunningAttempts(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := run(t, e, ex)
	e.disp.take()
	if _, err := e.svc.CancelTask(t.Context(), task.ID, "owner stopped it"); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("cancel r1 %s/%d cancelled", st.ID, st.Fence)
	if c := e.disp.take(); !slices.Equal(c, []string{want}) {
		t.Fatalf("commands %v", c)
	}
}

// Heartbeats renew the runner's live leases, so a long step keeps running.
func TestRenewRunnerLeases(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	run(t, e, ex)
	for range 3 {
		e.clock.Add(DefaultLeaseTTL / 2)
		if err := e.svc.RenewRunnerLeases(t.Context(), "r1"); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.RenewRunnerLeases(t.Context(), "someone-else"); err != nil {
			t.Fatal(err)
		}
	}
	e.schedule(t)
	if _, ex := e.state(t, task.ID); ex.Steps[0].State != domain.StepRunning {
		t.Fatalf("step %s", ex.Steps[0].State)
	}
}

// A claim the runner never starts (lost assignment) is not kept alive by heartbeats: its lease returns it to READY
// and the next pass assigns it again with a new token.
func TestHeartbeatsDoNotKeepAnUnstartedClaim(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	fence := ex.Steps[0].Fence
	for range 3 {
		e.clock.Add(DefaultLeaseTTL / 2)
		if err := e.svc.RenewRunnerLeases(t.Context(), "r1"); err != nil {
			t.Fatal(err)
		}
	}
	e.schedule(t)
	if _, ex := e.state(t, task.ID); ex.Steps[0].State != domain.StepClaimed || ex.Steps[0].Fence != fence+1 {
		t.Fatalf("step %+v", ex.Steps[0])
	}
}

// Disabling or revoking a runner (KIN-63) fails its running attempt at once and lets the step run again on another
// runner. A disabled runner gets execution.cancel in the same transaction; a revoked one never connects again, so
// nothing is queued for it (it stops its processes itself).
func TestReleaseRunnerFailsItsAttemptsAndRetriesElsewhere(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{ReleaseDisabled, ReleaseRevoked} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			e := newSchedEnv()
			task := e.task(t, onlyStep())
			e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
			e.schedule(t)
			_, ex := e.state(t, task.ID)
			st := run(t, e, ex)
			e.disp.take()
			if err := e.svc.ReleaseRunner(t.Context(), "r1", reason); err != nil {
				t.Fatal(err)
			}
			var want []string
			if reason == ReleaseDisabled {
				want = []string{fmt.Sprintf("cancel r1 %s/%d runner_disabled", st.ID, st.Fence)}
			}
			if c := e.disp.take(); !slices.Equal(c, want) {
				t.Fatalf("commands %v", c)
			}
			_, ex = e.state(t, task.ID)
			s := ex.Steps[0]
			if s.State != domain.StepReady || s.RunnerID != "" || s.Attempts[0].State != domain.AttemptFailed ||
				s.Attempts[0].Error != "runner "+reason+" by the Owner" {
				t.Fatalf("step %+v", s)
			}
			e.pool.set("p1", Candidate{ID: "r2", DiskHeavyAllowed: true})
			e.schedule(t)
			if c := e.disp.take(); !slices.Equal(c, []string{fmt.Sprintf("assign r2 only/%d p1 api", st.Fence+1)}) {
				t.Fatalf("retry commands %v", c)
			}
			if err := e.svc.ReleaseRunner(t.Context(), "r1", reason); err != nil { // nothing left: no-op
				t.Fatal(err)
			}
			if c := e.disp.take(); len(c) != 0 {
				t.Fatalf("second release queued %v", c)
			}
		})
	}
}

func twoParallelSteps() domain.Plan {
	return domain.Plan{Risk: domain.RiskL1, MaxParallel: 2, Summary: "s", Steps: []domain.PlanStep{
		{Key: "backend", Title: "Backend", Repository: "api", Capabilities: []string{"git"}, Scope: []string{"src/"}},
		{Key: "frontend", Title: "Frontend", Repository: "web", Capabilities: []string{"git"}, Scope: []string{"src/"}}}}
}

func gitRunner(id string) Candidate {
	return Candidate{ID: id, Tools: []string{"git"}, DiskHeavyAllowed: true}
}

// KIN-65: two parallel steps go to two runners, one each — the second assignment counts the first one made in the
// same pass.
func TestScheduleSpreadsParallelStepsOverRunners(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, twoParallelSteps())
	e.pool.set("p1", gitRunner("r2"), gitRunner("r1"))
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	if got := []string{ex.StepByKey("backend").RunnerID, ex.StepByKey("frontend").RunnerID}; !slices.Equal(got,
		[]string{"r1", "r2"}) {
		t.Fatalf("runners %v", got)
	}
	if c := e.disp.take(); !slices.Equal(c, []string{"assign r1 backend/1 p1 api", "assign r2 frontend/1 p1 web"}) {
		t.Fatalf("commands %v", c)
	}
}

// KIN-65: steps of other executions count as load — held in an earlier pass or assigned earlier in this one.
func TestScheduleCountsStepsHeldByOtherExecutions(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	e.pool.set("p1", gitRunner("r1"), gitRunner("r2"), gitRunner("r3"))
	first := e.task(t, onlyStep("git"))
	e.schedule(t) // r1 now holds a step
	second, third := e.task(t, onlyStep("git")), e.task(t, onlyStep("git"))
	e.schedule(t) // both in the same pass
	var got []string
	for _, id := range []string{first.ID, second.ID, third.ID} {
		_, ex := e.state(t, id)
		got = append(got, ex.Steps[0].RunnerID)
	}
	slices.Sort(got[1:]) // which of the two new tasks goes first is the task order's business
	if !slices.Equal(got, []string{"r1", "r2", "r3"}) {
		t.Fatalf("runners %v", got)
	}
}

// KIN-65: a lower id with more held steps loses; an ineligible idle runner is never picked.
func TestSchedulePrefersTheLeastLoadedEligibleRunner(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	e.pool.set("p1", gitRunner("r1"))
	busy := e.task(t, twoParallelSteps())
	e.schedule(t) // r1 holds both steps
	e.pool.set("p1", gitRunner("r1"), gitRunner("r2"), gitRunner("r3"),
		Candidate{ID: "r0", Tools: []string{"git"}, DiskHeavyAllowed: false})
	e.disp.take()
	task := e.task(t, onlyStep("git"))
	e.schedule(t)
	if _, ex := e.state(t, task.ID); ex.Steps[0].RunnerID != "r2" {
		t.Fatalf("picked %q", ex.Steps[0].RunnerID)
	}
	if _, ex := e.state(t, busy.ID); ex.Steps[0].RunnerID != "r1" || ex.Steps[1].RunnerID != "r1" {
		t.Fatalf("busy %+v", ex.Steps)
	}
}

// KIN-31: a runner that cannot verify the strict egress sandbox holds the step WAITING(network); the scheduler
// leaves it alone until the retry delay passed, then assigns it again (with a new token).
func TestStepHeldForNetworkIsRetriedAfterTheDelay(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := ex.Steps[0]
	_ = e.disp.take()
	if err := e.svc.HoldStepForNetwork(t.Context(), st.ID, "r1", st.Fence, "strict egress sandbox not verified", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	got, ex := e.state(t, task.ID)
	if !ex.Steps[0].WaitingForNetwork() || got.State != domain.TaskRunning {
		t.Fatalf("task %s step %+v", got.State, ex.Steps[0])
	}
	e.clock.Add(domain.NetworkRetryDelay - time.Second)
	e.schedule(t)
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("retried before the delay: %v", c)
	}
	e.clock.Add(time.Second)
	e.schedule(t)
	if c := e.disp.take(); !slices.Equal(c, []string{"assign r1 only/2 p1 api"}) {
		t.Fatalf("commands %v", c)
	}
	if _, ex = e.state(t, task.ID); ex.Steps[0].State != domain.StepClaimed {
		t.Fatalf("step %+v", ex.Steps[0])
	}
}

// KIN-51: holding a step whose runner changed files beyond the approved scope tells the runner to stop the
// attempt's process (reason scope_exceeded) in the same transaction; a hold with a superseded token sends nothing.
func TestHoldingAStepStopsItsProcess(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := ex.Steps[0]
	if err := e.svc.StartStep(t.Context(), st.ID, "r1", st.Fence); err != nil {
		t.Fatal(err)
	}
	e.disp.take()
	if err := e.svc.HoldStep(t.Context(), st.ID, "r1", st.Fence-1, "outside"); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale hold = %v", err)
	}
	if err := e.svc.HoldStep(t.Context(), st.ID, "r1", st.Fence, "outside"); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("cancel r1 %s/%d %s", st.ID, st.Fence, CancelReasonScopeExceeded)
	if got := e.disp.take(); len(got) != 1 || got[0] != want {
		t.Fatalf("commands %v, want %s", got, want)
	}
	if _, ex = e.state(t, task.ID); ex.Steps[0].State != domain.StepWaiting || ex.Steps[0].WaitReason != domain.WaitApproval {
		t.Fatalf("step %+v", ex.Steps[0])
	}
}

// KIN-79: a CLI that hit its limit holds the step WAITING(cli_limit) and the task BLOCKED(cli_limit) (the Alert
// Center hears of it) until the limit resets; then the scheduler assigns the step again and resumes the task.
func TestCLILimitBlocksTheTaskUntilTheLimitResets(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := ex.Steps[0]
	_ = e.disp.take()
	if err := e.svc.StartStep(t.Context(), st.ID, "r1", st.Fence); err != nil {
		t.Fatal(err)
	}
	hits := &fakeCLILimits{}
	e.svc.CLILimits = hits
	resets := e.clock.Now().Add(time.Hour)
	if err := e.svc.HoldStepForCLILimit(t.Context(), st.ID, "r1", st.Fence-1, "rate_limit", "x", resets, e.clock.Now()); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("stale hold = %v", err)
	}
	if err := e.svc.HoldStepForCLILimit(t.Context(), st.ID, "r1", st.Fence, "rate_limit", "cli_limit: rate_limit: usage limit reached",
		resets, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	got, ex := e.state(t, task.ID)
	if got.State != domain.TaskBlocked || got.BlockReason != domain.WaitCLILimit || !strings.Contains(got.Reason.Detail, "usage limit") ||
		ex.Steps[0].WaitReason != domain.WaitCLILimit || ex.Steps[0].Attempts[0].WorkEnd == nil {
		t.Fatalf("task %s %+v step %+v", got.State, got.Reason, ex.Steps[0])
	}
	if len(hits.got) != 1 || hits.got[0] != "r1 rate_limit "+resets.UTC().Format(time.RFC3339) {
		t.Fatalf("cli limit hits %v", hits.got)
	}
	e.clock.Add(time.Hour - time.Second)
	e.schedule(t)
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("retried before the reset: %v", c)
	}
	if got, _ := e.state(t, task.ID); got.State != domain.TaskBlocked {
		t.Fatalf("task %s", got.State)
	}
	e.clock.Add(time.Second)
	e.schedule(t)
	if c := e.disp.take(); !slices.Equal(c, []string{"assign r1 only/2 p1 api"}) {
		t.Fatalf("commands %v", c)
	}
	if got, ex := e.state(t, task.ID); got.State != domain.TaskRunning || ex.Steps[0].State != domain.StepClaimed {
		t.Fatalf("task %s step %+v", got.State, ex.Steps[0])
	}
}

// KIN-79: the CLI's usage and session are stored with the attempt, even when its end was refused (late report).
func TestAgentReportIsStoredWithTheAttempt(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	st := ex.Steps[0]
	if err := e.svc.StartStep(t.Context(), st.ID, "r1", st.Fence); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTask(t.Context(), task.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	r := domain.AgentReport{Session: "sess-1", Version: "2.1.0", Usage: domain.AgentUsage{InputTokens: 10, CostUSD: 0.1, Turns: 1}}
	if err := e.svc.RecordAgentReport(t.Context(), st.ID, "r1", st.Fence, r); err != nil {
		t.Fatal(err)
	}
	if _, ex := e.state(t, task.ID); ex.Steps[0].Attempts[0].Report != r {
		t.Fatalf("attempt %+v", ex.Steps[0].Attempts[0])
	}
	if err := e.svc.RecordAgentReport(t.Context(), st.ID, "r9", st.Fence, r); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("report of another runner = %v", err)
	}
}

// fakeCLILimits is routing's CLI holds: it records the calls and answers with hold (zero: none known).
type fakeCLILimits struct {
	got    []string
	worked []string
	hold   CLIHold
}

func (f *fakeCLILimits) CLILimitHit(_ context.Context, runnerID, agent, cause string, resets time.Time, _ string) (CLIHold, error) {
	f.got = append(f.got, strings.Join(strings.Fields(runnerID+" "+agent+" "+cause), " ")+" "+resets.UTC().Format(time.RFC3339))
	return f.hold, nil
}

func (f *fakeCLILimits) CLIWorked(_ context.Context, runnerID, agent string) error {
	f.worked = append(f.worked, runnerID+" "+agent)
	return nil
}

// KIN-79 with KIN-33: a step held for its CLI's limit waits for the reset, unless routing may pick another CLI or
// runner (the held runner's CLI cools down there).
func TestCLILimitHoldIsSchedulableAtOnceWhenRouted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	st := &domain.Step{State: domain.StepWaiting, WaitReason: domain.WaitCLILimit, WaitUntil: &until}
	if schedulable(st, now, false) || !schedulable(st, until, false) || !schedulable(st, now, true) {
		t.Fatal("cli_limit hold scheduling wrong")
	}
}
