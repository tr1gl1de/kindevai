package domain

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

const ttl = time.Minute

// stepTable is the Step FSM (KIN-A-14 chain plus the KIN-21 decisions for WAITING, FAILED and CANCELLED).
var stepTable = map[StepState][]StepState{
	StepPending:   {StepReady, StepCancelled},
	StepReady:     {StepClaimed, StepWaiting, StepCancelled},
	StepClaimed:   {StepReady, StepRunning, StepWaiting, StepFailed, StepCancelled},
	StepRunning:   {StepSucceeded, StepWaiting, StepFailed, StepCancelled},
	StepWaiting:   {StepReady, StepRunning, StepSucceeded, StepFailed, StepCancelled},
	StepFailed:    {StepReady},
	StepSucceeded: nil,
	StepCancelled: nil,
}

func TestStepTransitionTable(t *testing.T) {
	t.Parallel()
	for _, from := range AllStepStates {
		for _, to := range AllStepStates {
			if got, want := StepTransitionAllowed(from, to), slices.Contains(stepTable[from], to); got != want {
				t.Errorf("%s -> %s allowed = %v, want %v", from, to, got, want)
			}
		}
	}
}

func newExecution(t *testing.T, maxParallel int) Execution {
	t.Helper()
	p := samplePlan()
	p.MaxParallel = maxParallel
	p.Steps = append(p.Steps, PlanStep{Key: "lint", Title: "Lint", Repository: "api"})
	rev, err := NewPlanRevision("t1", 1, p, RiskL1, repos, "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	return NewExecution("e1", rev, func() string { n++; return fmt.Sprintf("s%d", n) }, t0)
}

func stepState(t *testing.T, e *Execution, key string) StepState {
	t.Helper()
	s := e.StepByKey(key)
	if s == nil {
		t.Fatalf("no step %s", key)
	}
	return s.State
}

func TestNewExecutionMakesRootStepsReady(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	if e.State != ExecutionRunning || e.PlanRevision != 1 || e.PlanHash == "" || e.MaxParallel != 2 || len(e.Steps) != 4 {
		t.Fatalf("execution = %+v", e)
	}
	for key, want := range map[string]StepState{"backend": StepReady, "frontend": StepPending, "docs": StepPending, "lint": StepReady} {
		if got := stepState(t, &e, key); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
}

func TestClaimIssuesMonotonicFencingTokensAndLeases(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l1, err := e.Claim(id, "r1", t0, ttl)
	if err != nil || l1.Fence != 1 || l1.RunnerID != "r1" || !l1.ExpiresAt.Equal(t0.Add(ttl)) || l1.StepID != id {
		t.Fatalf("claim = %+v, %v", l1, err)
	}
	if _, err := e.Claim(id, "r2", t0, ttl); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second claim of a claimed step = %v", err)
	}
	// The lease runs out before the runner starts: the step returns to READY and the next claim gets a higher token.
	if expired := e.ExpireLeases(t0.Add(ttl)); !slices.Equal(expired, []string{id}) || stepState(t, &e, "backend") != StepReady {
		t.Fatalf("expire = %v, state %s", expired, stepState(t, &e, "backend"))
	}
	l2, err := e.Claim(id, "r2", t0.Add(ttl), ttl)
	if err != nil || l2.Fence != 2 {
		t.Fatalf("re-claim = %+v, %v", l2, err)
	}
	// The first runner's token is stale now.
	if err := e.Start(id, "r1", l1.Fence, t0.Add(ttl), ttl); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("start with a stale token = %v", err)
	}
	if err := e.Start(id, "r2", l1.Fence, t0.Add(ttl), ttl); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("start with the right runner but an old token = %v", err)
	}
	if err := e.Start(id, "r2", l2.Fence, t0.Add(ttl), ttl); err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() error{
		"renew":   func() error { _, err := e.Renew(id, "r1", l1.Fence, t0.Add(ttl), ttl); return err },
		"succeed": func() error { return e.Succeed(id, "r1", l1.Fence, t0.Add(ttl)) },
		"fail":    func() error { return e.Fail(id, "r1", l1.Fence, "boom", t0.Add(ttl), 2) },
		"future":  func() error { return e.Succeed(id, "r2", l2.Fence+1, t0.Add(ttl)) },
	} {
		if err := op(); !errors.Is(err, ErrStaleFence) {
			t.Errorf("%s with a stale token = %v", name, err)
		}
	}
	if s := e.StepByKey("backend"); s.State != StepRunning || len(s.Attempts) != 1 || s.Attempts[0].Fence != 2 {
		t.Fatalf("stale writes changed the step: %+v", s)
	}
}

func TestStartRequiresALiveLease(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	if err := e.Start(id, "r1", l.Fence, t0.Add(ttl), ttl); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("start after expiry = %v", err)
	}
	if err := e.Start(id, "r1", l.Fence, t0.Add(ttl-time.Second), ttl); err != nil {
		t.Fatal(err)
	}
	s := e.StepByKey("backend")
	if s.State != StepRunning || !s.LeaseExpiresAt.Equal(t0.Add(2*ttl-time.Second)) || len(s.Attempts) != 1 ||
		s.Attempts[0] != (Attempt{Number: 1, RunnerID: "r1", Fence: 1, State: AttemptRunning, StartedAt: t0.Add(ttl - time.Second)}) {
		t.Fatalf("started step = %+v", s)
	}
	if err := e.Start(id, "r1", l.Fence, t0.Add(ttl), ttl); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second start = %v", err)
	}
}

func TestRenewExtendsTheLease(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	_ = e.Start(id, "r1", l.Fence, t0, ttl)
	exp, err := e.Renew(id, "r1", l.Fence, t0.Add(50*time.Second), ttl)
	if err != nil || !exp.Equal(t0.Add(110*time.Second)) {
		t.Fatalf("renew = %v, %v", exp, err)
	}
	if got := e.ExpireLeases(t0.Add(100 * time.Second)); len(got) != 0 {
		t.Fatalf("a renewed lease expired: %v", got)
	}
	if _, err := e.Renew(id, "r1", l.Fence, t0.Add(110*time.Second), ttl); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("renewing an expired lease = %v", err)
	}
}

func TestExpiredLeaseOfARunningStepWaitsForReconciliation(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	_ = e.Start(id, "r1", l.Fence, t0, ttl)
	if got := e.ExpireLeases(t0.Add(ttl)); !slices.Equal(got, []string{id}) {
		t.Fatalf("expired = %v", got)
	}
	s := e.StepByKey("backend")
	if s.State != StepWaiting || s.WaitReason != WaitRunner || s.Attempts[0].State != AttemptUnknown || s.LeaseExpiresAt != nil {
		t.Fatalf("step = %+v", s)
	}
	// No second process: the step cannot be claimed or resumed until the unknown attempt is reconciled.
	if _, err := e.Claim(id, "r2", t0.Add(ttl), ttl); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("claim of a waiting step = %v", err)
	}
	if err := e.ResumeStep(id, t0.Add(ttl)); !errors.Is(err, ErrAttemptUnknown) {
		t.Fatalf("resume with an unknown attempt = %v", err)
	}
	// The runner comes back with the still-current token and reports the result.
	if err := e.Succeed(id, "r1", l.Fence, t0.Add(2*ttl)); err != nil {
		t.Fatal(err)
	}
	if s := e.StepByKey("backend"); s.State != StepSucceeded || s.Attempts[0].State != AttemptSucceeded || s.WaitReason != "" {
		t.Fatalf("late result = %+v", s)
	}
}

func TestSuccessPromotesDependentsAndFinishesTheExecution(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 4)
	run := func(key string) {
		t.Helper()
		id := e.StepByKey(key).ID
		l, err := e.Claim(id, "r1", t0, ttl)
		if err != nil {
			t.Fatalf("claim %s: %v", key, err)
		}
		if err := e.Start(id, "r1", l.Fence, t0, ttl); err != nil {
			t.Fatal(err)
		}
		if err := e.Succeed(id, "r1", l.Fence, t0); err != nil {
			t.Fatal(err)
		}
	}
	run("backend")
	if stepState(t, &e, "frontend") != StepReady || stepState(t, &e, "docs") != StepPending {
		t.Fatal("frontend depends only on backend; docs also waits for frontend")
	}
	run("frontend")
	run("lint")
	if e.State != ExecutionRunning || stepState(t, &e, "docs") != StepReady {
		t.Fatalf("execution %s, docs %s", e.State, stepState(t, &e, "docs"))
	}
	run("docs")
	if e.State != ExecutionSucceeded || e.FinishedAt == nil {
		t.Fatalf("execution = %+v", e)
	}
	if _, err := e.Claim(e.StepByKey("docs").ID, "r1", t0, ttl); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("finished execution accepted a claim")
	}
}

func TestClaimRespectsTheParallelismLimit(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 1)
	b, l := e.StepByKey("backend").ID, e.StepByKey("lint").ID
	lease, err := e.Claim(b, "r1", t0, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Claim(l, "r2", t0, ttl); !errors.Is(err, ErrParallelismLimit) {
		t.Fatalf("second concurrent claim = %v", err)
	}
	_ = e.Start(b, "r1", lease.Fence, t0, ttl)
	_ = e.Succeed(b, "r1", lease.Fence, t0)
	if _, err := e.Claim(l, "r2", t0, ttl); err != nil {
		t.Fatalf("claim after the slot freed = %v", err)
	}
}

func TestFailRetriesThenFailsTheExecution(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	attempt := func(at time.Time) Lease {
		t.Helper()
		l, err := e.Claim(id, "r1", at, ttl)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Start(id, "r1", l.Fence, at, ttl); err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := attempt(t0)
	if err := e.Fail(id, "r1", l.Fence, "tests failed", t0, 2); err != nil {
		t.Fatal(err)
	}
	if s := e.StepByKey("backend"); s.State != StepReady || s.Attempts[0].State != AttemptFailed || s.Attempts[0].Error != "tests failed" {
		t.Fatalf("after first failure = %+v", s)
	}
	l = attempt(t0.Add(time.Second))
	if err := e.Fail(id, "r1", l.Fence, "tests failed again", t0.Add(time.Second), 2); err != nil {
		t.Fatal(err)
	}
	s := e.StepByKey("backend")
	if s.State != StepFailed || len(s.Attempts) != 2 || s.Attempts[1].Number != 2 || s.Attempts[1].Fence != 2 {
		t.Fatalf("after the retry budget = %+v", s)
	}
	if e.State != ExecutionFailed || e.Reason == "" {
		t.Fatalf("execution = %s %q", e.State, e.Reason)
	}
	for _, key := range []string{"frontend", "docs", "lint"} {
		if stepState(t, &e, key) != StepCancelled {
			t.Errorf("%s = %s, want CANCELLED", key, stepState(t, &e, key))
		}
	}
}

func TestWaitAndResumeAReadyStep(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	if err := e.WaitStep(id, "weather", t0); err == nil {
		t.Fatal("unknown wait reason accepted")
	}
	if err := e.WaitStep(id, WaitNetwork, t0); err != nil || stepState(t, &e, "backend") != StepWaiting ||
		e.StepByKey("backend").WaitReason != WaitNetwork {
		t.Fatalf("wait = %v", err)
	}
	if !e.StepByKey("backend").WaitingForNetwork() {
		t.Fatal("WAITING(network) is what the UI shows as WAITING_FOR_NETWORK")
	}
	if _, err := e.Claim(id, "r1", t0, ttl); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("a waiting step cannot be claimed")
	}
	if err := e.ResumeStep(id, t0); err != nil || stepState(t, &e, "backend") != StepReady || e.StepByKey("backend").WaitReason != "" {
		t.Fatalf("resume = %v", err)
	}
	if err := e.WaitStep(e.StepByKey("docs").ID, WaitRunner, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("a pending step cannot wait")
	}
}

func TestCancelStopsEverything(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	_ = e.Start(id, "r1", l.Fence, t0, ttl)
	if err := e.Cancel("", t0); err == nil {
		t.Fatal("cancel needs a reason")
	}
	if err := e.Cancel("owner cancelled", t0); err != nil {
		t.Fatal(err)
	}
	if e.State != ExecutionCancelled || e.Reason != "owner cancelled" {
		t.Fatalf("execution = %+v", e)
	}
	for _, s := range e.Steps {
		if s.State != StepCancelled || s.LeaseExpiresAt != nil {
			t.Errorf("%s = %s", s.Key, s.State)
		}
	}
	if e.StepByKey("backend").Attempts[0].State != AttemptCancelled {
		t.Fatal("running attempt not cancelled")
	}
	if err := e.Succeed(id, "r1", l.Fence, t0); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("result after cancel = %v", err)
	}
	if err := e.Cancel("again", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second cancel = %v", err)
	}
}

func TestUnknownStep(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	if _, err := e.Claim("nope", "r1", t0, ttl); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim unknown = %v", err)
	}
	if _, err := e.Claim(e.StepByKey("backend").ID, "", t0, ttl); err == nil {
		t.Fatal("claim without a runner")
	}
}

func TestCloneIsDeep(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	_ = e.Start(id, "r1", l.Fence, t0, ttl)
	c := e.Clone()
	*c.Steps[0].LeaseExpiresAt = t0
	_ = c.Succeed(id, "r1", l.Fence, t0)
	c.Steps[1].DependsOn = append(c.Steps[1].DependsOn[:0], "x")
	if s := e.StepByKey("backend"); s.State != StepRunning || s.Attempts[0].State != AttemptRunning ||
		s.LeaseExpiresAt.Equal(t0) || e.StepByKey("frontend").DependsOn[0] != "backend" {
		t.Fatal("clone shares state with the original")
	}
}

// TestExecutionInvariantsUnderRandomOperations is the property test for the step machine: whatever runners do,
// fencing tokens never decrease, at most MaxParallel steps hold a slot, a step only runs after its dependencies
// succeeded and every state change follows the Step FSM table.
func TestExecutionInvariantsUnderRandomOperations(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 21)) //nolint:gosec // deterministic test input
	runners := []string{"r1", "r2", "r3"}
	for walk := range 300 {
		e := newExecution(t, 1+rng.IntN(3))
		now := t0
		fences := map[string]int64{}
		for range 60 {
			now = now.Add(time.Duration(rng.IntN(40)) * time.Second)
			before := map[string]StepState{}
			for _, s := range e.Steps {
				before[s.ID] = s.State
			}
			s := e.Steps[rng.IntN(len(e.Steps))]
			r := runners[rng.IntN(len(runners))]
			fence := s.Fence - int64(rng.IntN(2)) // sometimes stale
			switch rng.IntN(9) {
			case 0, 1:
				_, _ = e.Claim(s.ID, r, now, ttl)
			case 2:
				_ = e.Start(s.ID, s.RunnerID, fence, now, ttl)
			case 3:
				_ = e.Succeed(s.ID, s.RunnerID, fence, now)
			case 4:
				_ = e.Fail(s.ID, s.RunnerID, fence, "x", now, 2)
			case 5:
				_, _ = e.Renew(s.ID, s.RunnerID, fence, now, ttl)
			case 6:
				e.ExpireLeases(now)
			case 7:
				_ = e.ResumeStep(s.ID, now)
			case 8:
				_ = e.HoldForApproval(s.ID, s.RunnerID, fence, "scope", now)
			}
			active := 0
			for _, st := range e.Steps {
				if st.Fence < fences[st.ID] {
					t.Fatalf("walk %d: fence of %s went down", walk, st.Key)
				}
				fences[st.ID] = st.Fence
				from := before[st.ID]
				retried := StepTransitionAllowed(from, StepFailed) && st.State == StepReady // RUNNING -> FAILED -> READY in one call
				if from != st.State && !StepTransitionAllowed(from, st.State) && !retried {
					t.Fatalf("walk %d: %s moved %s -> %s", walk, st.Key, from, st.State)
				}
				if st.State == StepClaimed || st.State == StepRunning {
					active++
					for _, dep := range st.DependsOn {
						if e.StepByKey(dep).State != StepSucceeded {
							t.Fatalf("walk %d: %s runs before %s succeeded", walk, st.Key, dep)
						}
					}
				}
			}
			if active > e.MaxParallel {
				t.Fatalf("walk %d: %d active steps, limit %d", walk, active, e.MaxParallel)
			}
		}
	}
}

// unknownStep leaves the backend step WAITING(runner) on an UNKNOWN attempt.
func unknownStep(t *testing.T, maxParallel int) (Execution, string, Lease) {
	t.Helper()
	e := newExecution(t, maxParallel)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	if err := e.Start(id, "r1", l.Fence, t0, ttl); err != nil {
		t.Fatal(err)
	}
	e.ExpireLeases(t0.Add(ttl))
	return e, id, l
}

func TestAStepWithAnUnknownAttemptKeepsItsParallelismSlot(t *testing.T) {
	t.Parallel()
	e, _, _ := unknownStep(t, 1)
	if _, err := e.Claim(e.StepByKey("lint").ID, "r2", t0.Add(ttl), ttl); !errors.Is(err, ErrParallelismLimit) {
		t.Fatalf("claim next to a possibly live process = %v", err)
	}
}

func TestReconcileAnUnknownAttempt(t *testing.T) {
	t.Parallel()
	now := t0.Add(2 * ttl)
	// The process is alive: back to RUNNING under the same token with a fresh lease.
	e, id, l := unknownStep(t, 2)
	if err := e.Reconcile(id, ProcessAlive, now, ttl, 2); err != nil {
		t.Fatal(err)
	}
	s := e.StepByKey("backend")
	if s.State != StepRunning || s.Attempts[0].State != AttemptRunning || !s.LeaseExpiresAt.Equal(now.Add(ttl)) || s.Fence != l.Fence {
		t.Fatalf("alive = %+v", s)
	}
	if err := e.Reconcile(id, ProcessAlive, now, ttl, 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reconcile without an unknown attempt = %v", err)
	}
	// The process is gone: the attempt failed and the step is retried with a new token.
	e, id, l = unknownStep(t, 2)
	if err := e.Reconcile(id, ProcessGone, now, ttl, 2); err != nil {
		t.Fatal(err)
	}
	s = e.StepByKey("backend")
	if s.State != StepReady || s.Attempts[0].State != AttemptFailed || s.RunnerID != "" {
		t.Fatalf("gone = %+v", s)
	}
	if l2, err := e.Claim(id, "r2", now, ttl); err != nil || l2.Fence != l.Fence+1 {
		t.Fatalf("re-claim = %+v, %v", l2, err)
	}
	// Gone without retry budget fails the execution.
	e, id, _ = unknownStep(t, 2)
	if err := e.Reconcile(id, ProcessGone, now, ttl, 1); err != nil || e.State != ExecutionFailed {
		t.Fatalf("gone, no budget = %v, %s", err, e.State)
	}
	if err := e.Reconcile(id, "maybe", now, ttl, 1); err == nil {
		t.Fatal("unknown outcome accepted")
	}
}

func TestReportsToAClosedExecutionAreStale(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, _ := e.Claim(id, "r1", t0, ttl)
	_ = e.Start(id, "r1", l.Fence, t0, ttl)
	_ = e.Cancel("stop", t0)
	if _, err := e.Renew(id, "r1", l.Fence, t0, ttl); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("renew after cancel = %v (the runner must stop its process)", err)
	}
}

func TestHoldForApprovalStopsTheStepWithoutUsingTheRetryBudget(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	l, err := e.Claim(id, "r1", t0, ttl)
	if err != nil {
		t.Fatal(err)
	}
	// A claimed step that never started may be held too.
	if err := e.HoldForApproval(id, "r1", l.Fence, "changed files outside the approved scope", t0); err != nil {
		t.Fatalf("hold a claimed step = %v", err)
	}
	if err := e.ResumeStep(id, t0); err != nil {
		t.Fatal(err)
	}
	l, _ = e.Claim(id, "r1", t0, ttl)
	if err := e.Start(id, "r1", l.Fence, t0, ttl); err != nil {
		t.Fatal(err)
	}
	if err := e.HoldForApproval(id, "r2", l.Fence, "x", t0); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("hold by a runner without the claim = %v", err)
	}
	if err := e.HoldForApproval(id, "r1", l.Fence, "changed files outside the approved scope", t0); err != nil {
		t.Fatal(err)
	}
	s := e.StepByKey("backend")
	last := s.Attempts[len(s.Attempts)-1]
	if s.State != StepWaiting || s.WaitReason != WaitApproval || s.RunnerID != "" || s.LeaseExpiresAt != nil ||
		last.State != AttemptCancelled {
		t.Fatalf("held step = %+v", s)
	}
	if err := e.Succeed(id, "r1", l.Fence, t0); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("a held step's old process must not report success: %v", err)
	}
	if _, err := e.Claim(id, "r1", t0, ttl); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("a held step cannot be claimed before the approval")
	}
	if err := e.ResumeStep(id, t0); err != nil || stepState(t, &e, "backend") != StepReady {
		t.Fatalf("resume after approval = %v", err)
	}
	// Cancelled attempts do not count: with a budget of 2 the step still gets a retry after one real failure.
	l, _ = e.Claim(id, "r1", t0, ttl)
	_ = e.Start(id, "r1", l.Fence, t0, ttl)
	if err := e.Fail(id, "r1", l.Fence, "boom", t0, 2); err != nil || stepState(t, &e, "backend") != StepReady {
		t.Fatalf("first real failure must be retried: %v, %s", err, stepState(t, &e, "backend"))
	}
}
