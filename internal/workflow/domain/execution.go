package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// StepState is a state of the WorkflowStep FSM (KIN-A-14).
type StepState string

const (
	StepPending   StepState = "PENDING"
	StepReady     StepState = "READY"
	StepClaimed   StepState = "CLAIMED"
	StepRunning   StepState = "RUNNING"
	StepSucceeded StepState = "SUCCEEDED"
	StepWaiting   StepState = "WAITING"
	StepFailed    StepState = "FAILED"
	StepCancelled StepState = "CANCELLED"
)

// AllStepStates lists every step state.
var AllStepStates = []StepState{StepPending, StepReady, StepClaimed, StepRunning, StepSucceeded, StepWaiting,
	StepFailed, StepCancelled}

// stepTransitions is the Step FSM. CLAIMED -> READY: the lease ran out before the runner started. WAITING ->
// RUNNING/SUCCEEDED/FAILED: reconciliation found the process alive or its result. FAILED -> READY: retry within
// the attempt budget.
var stepTransitions = map[StepState][]StepState{
	StepPending: {StepReady, StepCancelled},
	StepReady:   {StepClaimed, StepWaiting, StepCancelled},
	StepClaimed: {StepReady, StepRunning, StepWaiting, StepFailed, StepCancelled},
	StepRunning: {StepSucceeded, StepWaiting, StepFailed, StepCancelled},
	StepWaiting: {StepReady, StepRunning, StepSucceeded, StepFailed, StepCancelled},
	StepFailed:  {StepReady},
}

// StepTransitionAllowed reports whether the Step FSM has the edge from -> to.
func StepTransitionAllowed(from, to StepState) bool {
	return slices.Contains(stepTransitions[from], to)
}

// ExecutionState is the state of one run of a plan revision.
type ExecutionState string

const (
	ExecutionRunning   ExecutionState = "RUNNING"
	ExecutionSucceeded ExecutionState = "SUCCEEDED"
	ExecutionFailed    ExecutionState = "FAILED"
	ExecutionCancelled ExecutionState = "CANCELLED"
)

// AttemptState is the outcome of one runner launch of a step.
type AttemptState string

const (
	AttemptRunning   AttemptState = "RUNNING"
	AttemptSucceeded AttemptState = "SUCCEEDED"
	AttemptFailed    AttemptState = "FAILED"
	AttemptCancelled AttemptState = "CANCELLED"
	// AttemptUnknown: the lease ran out while running; the process may still be alive until reconciliation.
	AttemptUnknown AttemptState = "UNKNOWN"
)

// Attempt (StepAttempt) is one launch of a step on a runner under one fencing token.
type Attempt struct {
	Number     int
	RunnerID   string
	Fence      int64
	State      AttemptState
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Lease is a runner's claim on a step. Every later report must carry Fence; a newer claim makes it stale.
type Lease struct {
	StepID    string
	RunnerID  string
	Fence     int64
	ExpiresAt time.Time
}

// Step (WorkflowStep) is one node of the plan DAG inside an execution.
type Step struct {
	ID         string
	Key        string
	Title      string
	Repository string
	DependsOn  []string
	State      StepState
	WaitReason WaitReason // set only while WAITING
	Reason     string     // why the step failed or was cancelled
	// Fence is the last fencing token issued for the step. It only grows, across attempts and runners.
	Fence          int64
	RunnerID       string     // holder of the current claim (CLAIMED, RUNNING, or WAITING on an unknown attempt)
	LeaseExpiresAt *time.Time // set while CLAIMED or RUNNING
	Attempts       []Attempt
}

// WaitingForNetwork is what the UI shows as WAITING_FOR_NETWORK.
func (s *Step) WaitingForNetwork() bool { return s.State == StepWaiting && s.WaitReason == WaitNetwork }

func (s *Step) unknownAttempt() bool {
	a := s.lastAttempt()
	return s.State == StepWaiting && a != nil && a.State == AttemptUnknown
}

func (s *Step) lastAttempt() *Attempt {
	if len(s.Attempts) == 0 {
		return nil
	}
	return &s.Attempts[len(s.Attempts)-1]
}

// move sets the state; callers check the FSM edge first (the property test verifies every change follows it).
func (s *Step) move(to StepState) {
	s.State = to
	if to != StepWaiting {
		s.WaitReason = ""
	}
	if to != StepClaimed && to != StepRunning {
		s.LeaseExpiresAt = nil
	}
}

func (s *Step) finishAttempt(state AttemptState, msg string, now time.Time) {
	if a := s.lastAttempt(); a != nil && (a.State == AttemptRunning || a.State == AttemptUnknown) {
		a.State, a.Error, a.FinishedAt = state, msg, &now
	}
}

// Execution is the aggregate for one run of a plan revision: it owns the steps and their attempts, so claims,
// the parallelism limit and dependency promotion change atomically.
type Execution struct {
	ID           string
	TaskID       string
	PlanRevision int
	PlanHash     string
	State        ExecutionState
	MaxParallel  int
	Reason       string
	CreatedAt    time.Time
	FinishedAt   *time.Time
	Steps        []Step
	Version      int64 // optimistic concurrency, maintained by the repository
}

// NewExecution starts a run of rev: one PENDING step per plan step, roots promoted to READY.
func NewExecution(id string, rev PlanRevision, newID func() string, now time.Time) Execution {
	p := rev.Plan()
	e := Execution{ID: id, TaskID: rev.TaskID(), PlanRevision: rev.Number(), PlanHash: rev.Hash(),
		State: ExecutionRunning, MaxParallel: p.MaxParallel, CreatedAt: now}
	for _, ps := range p.Steps {
		e.Steps = append(e.Steps, Step{ID: newID(), Key: ps.Key, Title: ps.Title, Repository: ps.Repository,
			DependsOn: ps.DependsOn, State: StepPending})
	}
	e.promote()
	return e
}

// Step returns the step with id, or nil.
func (e *Execution) Step(id string) *Step {
	for i := range e.Steps {
		if e.Steps[i].ID == id {
			return &e.Steps[i]
		}
	}
	return nil
}

// StepByKey returns the step with the plan key, or nil.
func (e *Execution) StepByKey(key string) *Step {
	for i := range e.Steps {
		if e.Steps[i].Key == key {
			return &e.Steps[i]
		}
	}
	return nil
}

// Clone returns a deep copy.
func (e Execution) Clone() Execution {
	c := e
	if e.FinishedAt != nil {
		f := *e.FinishedAt
		c.FinishedAt = &f
	}
	c.Steps = make([]Step, len(e.Steps))
	for i, s := range e.Steps {
		s.DependsOn = slices.Clone(s.DependsOn)
		if s.LeaseExpiresAt != nil {
			l := *s.LeaseExpiresAt
			s.LeaseExpiresAt = &l
		}
		s.Attempts = slices.Clone(s.Attempts)
		for j, a := range s.Attempts {
			if a.FinishedAt != nil {
				f := *a.FinishedAt
				s.Attempts[j].FinishedAt = &f
			}
		}
		c.Steps[i] = s
	}
	return c
}

func (e *Execution) step(id string, op string) (*Step, error) {
	if e.State != ExecutionRunning {
		return nil, fmt.Errorf("execution %s is %s: %s: %w", e.ID, e.State, op, ErrInvalidTransition)
	}
	s := e.Step(id)
	if s == nil {
		return nil, fmt.Errorf("step %s: %w", id, ErrNotFound)
	}
	return s, nil
}

func invalidStep(s *Step, op string) error {
	return fmt.Errorf("step %s: %s in %s: %w", s.Key, op, s.State, ErrInvalidTransition)
}

// promote moves PENDING steps whose dependencies all succeeded to READY and finishes the execution once every step
// succeeded.
func (e *Execution) promote() {
	done := true
	for i := range e.Steps {
		s := &e.Steps[i]
		if s.State == StepPending && !slices.ContainsFunc(s.DependsOn, func(k string) bool {
			d := e.StepByKey(k)
			return d == nil || d.State != StepSucceeded
		}) {
			s.move(StepReady)
		}
		done = done && s.State == StepSucceeded
	}
	if done && e.State == ExecutionRunning {
		e.State = ExecutionSucceeded
	}
}

func (e *Execution) active() int {
	n := 0
	for _, s := range e.Steps {
		if s.State == StepClaimed || s.State == StepRunning || s.unknownAttempt() {
			n++ // an unknown attempt may still have a live process
		}
	}
	return n
}

// Claim gives runnerID the READY step under a new fencing token and a lease of ttl.
func (e *Execution) Claim(stepID, runnerID string, now time.Time, ttl time.Duration) (Lease, error) {
	if runnerID == "" {
		return Lease{}, invalid("runnerId", "runner_required")
	}
	s, err := e.step(stepID, "claim")
	if err != nil {
		return Lease{}, err
	}
	if s.State != StepReady {
		return Lease{}, invalidStep(s, "claim")
	}
	if e.active() >= e.MaxParallel {
		return Lease{}, ErrParallelismLimit
	}
	exp := now.Add(ttl)
	s.move(StepClaimed)
	s.Fence++
	s.RunnerID, s.LeaseExpiresAt = runnerID, &exp
	return Lease{StepID: s.ID, RunnerID: runnerID, Fence: s.Fence, ExpiresAt: exp}, nil
}

// holder returns the step if runnerID with fence holds its current claim.
// A finished or cancelled execution has no current claims, so every runner report to it is stale: the runner
// must stop its process.
func (e *Execution) holder(stepID, runnerID string, fence int64, op string) (*Step, error) {
	if e.State != ExecutionRunning && e.Step(stepID) != nil {
		return nil, fmt.Errorf("execution %s is %s: %s: %w", e.ID, e.State, op, ErrStaleFence)
	}
	s, err := e.step(stepID, op)
	if err != nil {
		return nil, err
	}
	if s.RunnerID == "" || s.RunnerID != runnerID || s.Fence != fence {
		return nil, fmt.Errorf("step %s: %s with token %d: %w", s.Key, op, fence, ErrStaleFence)
	}
	return s, nil
}

func leaseLive(s *Step, now time.Time) bool {
	return s.LeaseExpiresAt != nil && now.Before(*s.LeaseExpiresAt)
}

// Start records that the runner launched the claimed step: CLAIMED -> RUNNING with a new attempt and a fresh lease.
func (e *Execution) Start(stepID, runnerID string, fence int64, now time.Time, ttl time.Duration) error {
	s, err := e.holder(stepID, runnerID, fence, "start")
	if err != nil {
		return err
	}
	if s.State != StepClaimed {
		return invalidStep(s, "start")
	}
	if !leaseLive(s, now) {
		return ErrLeaseExpired
	}
	exp := now.Add(ttl)
	s.move(StepRunning)
	s.LeaseExpiresAt = &exp
	s.Attempts = append(s.Attempts, Attempt{Number: len(s.Attempts) + 1, RunnerID: runnerID, Fence: fence,
		State: AttemptRunning, StartedAt: now})
	return nil
}

// Renew extends the lease of a CLAIMED or RUNNING step (heartbeat) and returns the new expiry.
func (e *Execution) Renew(stepID, runnerID string, fence int64, now time.Time, ttl time.Duration) (time.Time, error) {
	s, err := e.holder(stepID, runnerID, fence, "renew")
	if err != nil {
		return time.Time{}, err
	}
	if s.State != StepClaimed && s.State != StepRunning {
		return time.Time{}, invalidStep(s, "renew")
	}
	if !leaseLive(s, now) {
		return time.Time{}, ErrLeaseExpired
	}
	exp := now.Add(ttl)
	s.LeaseExpiresAt = &exp
	return exp, nil
}

// reportable: a result comes from a RUNNING step, or from a step WAITING on its unknown attempt (the runner came
// back after its lease ran out; its token is still the current one, so the result is genuine).
func reportable(s *Step) bool { return s.State == StepRunning || s.unknownAttempt() }

// Lease vs fencing: the token is the safety rule, the lease only liveness. Start and Renew need a live lease, but a
// result (Succeed/Fail) is accepted while the token is current even if the lease ran out and the sweep has not run
// yet: nobody else can hold the step until ExpireLeases releases it, and then the old token is rejected.

// Succeed records the step's success, promotes its dependents and finishes the execution when all steps succeeded.
func (e *Execution) Succeed(stepID, runnerID string, fence int64, now time.Time) error {
	s, err := e.holder(stepID, runnerID, fence, "succeed")
	if err != nil {
		return err
	}
	if !reportable(s) {
		return invalidStep(s, "succeed")
	}
	s.finishAttempt(AttemptSucceeded, "", now)
	s.move(StepSucceeded)
	s.RunnerID = ""
	e.promote()
	if e.State == ExecutionSucceeded {
		e.FinishedAt = &now
	}
	return nil
}

// Fail records a failed attempt. Within maxAttempts the step is retried (FAILED -> READY, the next claim gets a
// new token); otherwise the step stays FAILED, the execution fails and its unfinished steps are cancelled.
func (e *Execution) Fail(stepID, runnerID string, fence int64, msg string, now time.Time, maxAttempts int) error {
	s, err := e.holder(stepID, runnerID, fence, "fail")
	if err != nil {
		return err
	}
	if !reportable(s) && s.State != StepClaimed {
		return invalidStep(s, "fail")
	}
	msg = truncate(strings.TrimSpace(msg), MaxReasonLen)
	if msg == "" {
		msg = "failed"
	}
	if s.State == StepClaimed { // the launch itself failed: it still counts as an attempt
		s.Attempts = append(s.Attempts, Attempt{Number: len(s.Attempts) + 1, RunnerID: runnerID, Fence: fence,
			State: AttemptRunning, StartedAt: now})
	}
	e.failAttempt(s, msg, now, maxAttempts)
	return nil
}

// failAttempt ends the step's attempt as failed and retries the step within maxAttempts, else fails the execution.
func (e *Execution) failAttempt(s *Step, msg string, now time.Time, maxAttempts int) {
	s.finishAttempt(AttemptFailed, msg, now)
	s.move(StepFailed)
	s.RunnerID, s.Reason = "", msg
	if s.countedAttempts() < maxAttempts {
		s.move(StepReady)
		s.Reason = ""
		return
	}
	e.finish(ExecutionFailed, fmt.Sprintf("step %s failed: %s", s.Key, msg), now)
}

// ProcessState is what reconciliation with the runner found for an UNKNOWN attempt (KIN-22/23). A terminal result
// the runner kept arrives as Succeed/Fail with the still-current token instead.
type ProcessState string

const (
	ProcessAlive ProcessState = "alive" // the process still runs: back to RUNNING under the same token
	ProcessGone  ProcessState = "gone"  // no process and no result: the attempt failed (retry within the budget)
)

// Reconcile resolves the UNKNOWN attempt of a WAITING step. Until it is resolved no second process may start.
func (e *Execution) Reconcile(stepID string, p ProcessState, now time.Time, ttl time.Duration, maxAttempts int) error {
	if p != ProcessAlive && p != ProcessGone {
		return invalid("process", "process_state")
	}
	s, err := e.step(stepID, "reconcile")
	if err != nil {
		return err
	}
	if !s.unknownAttempt() {
		return invalidStep(s, "reconcile")
	}
	if p == ProcessGone {
		e.failAttempt(s, "process lost", now, maxAttempts)
		return nil
	}
	exp := now.Add(ttl)
	s.move(StepRunning)
	s.LeaseExpiresAt = &exp
	s.lastAttempt().State = AttemptRunning
	return nil
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ExpireLeases handles leases that ran out at now and returns the affected step IDs. A CLAIMED step never started,
// so it returns to READY. A RUNNING step's process may still be alive: it goes to WAITING(runner) with its attempt
// UNKNOWN and is not run again until reconciliation.
func (e *Execution) ExpireLeases(now time.Time) []string {
	if e.State != ExecutionRunning {
		return nil
	}
	var ids []string
	for i := range e.Steps {
		s := &e.Steps[i]
		if s.LeaseExpiresAt == nil || now.Before(*s.LeaseExpiresAt) {
			continue
		}
		switch s.State {
		case StepClaimed:
			s.move(StepReady)
			s.RunnerID = ""
		case StepRunning:
			s.move(StepWaiting)
			s.WaitReason = WaitRunner
			if a := s.lastAttempt(); a != nil {
				a.State = AttemptUnknown
			}
		default:
			continue
		}
		ids = append(ids, s.ID)
	}
	return ids
}

// HoldForApproval stops a CLAIMED or RUNNING step whose runner reported changes beyond the approved scope: the
// attempt is cancelled (it does not use the retry budget), the claim is released so the old process's later
// reports are stale, and the step WAITs with reason approval until ResumeStep after a new approval.
func (e *Execution) HoldForApproval(stepID, runnerID string, fence int64, msg string, now time.Time) error {
	s, err := e.holder(stepID, runnerID, fence, "hold")
	if err != nil {
		return err
	}
	if !reportable(s) && s.State != StepClaimed {
		return invalidStep(s, "hold")
	}
	msg = truncate(strings.TrimSpace(msg), MaxReasonLen)
	if s.State == StepClaimed {
		s.Attempts = append(s.Attempts, Attempt{Number: len(s.Attempts) + 1, RunnerID: runnerID, Fence: fence,
			State: AttemptRunning, StartedAt: now})
	}
	s.finishAttempt(AttemptCancelled, msg, now)
	s.move(StepWaiting)
	s.WaitReason, s.RunnerID = WaitApproval, ""
	return nil
}

// countedAttempts are the attempts that use the retry budget: cancelled ones (held for approval) do not.
func (s *Step) countedAttempts() int {
	n := 0
	for _, a := range s.Attempts {
		if a.State != AttemptCancelled {
			n++
		}
	}
	return n
}

// WaitStep parks a READY step that cannot be scheduled (no runner, network, CLI limit, approval).
func (e *Execution) WaitStep(stepID string, reason WaitReason, _ time.Time) error {
	if !reason.Valid() {
		return invalid("reason", "wait_reason")
	}
	s, err := e.step(stepID, "wait")
	if err != nil {
		return err
	}
	if s.State != StepReady {
		return invalidStep(s, "wait")
	}
	s.move(StepWaiting)
	s.WaitReason = reason
	return nil
}

// ResumeStep returns a WAITING step to READY, unless its last attempt is still unknown.
func (e *Execution) ResumeStep(stepID string, _ time.Time) error {
	s, err := e.step(stepID, "resume")
	if err != nil {
		return err
	}
	if s.State != StepWaiting {
		return invalidStep(s, "resume")
	}
	if a := s.lastAttempt(); a != nil && a.State == AttemptUnknown {
		return ErrAttemptUnknown
	}
	s.move(StepReady)
	s.RunnerID = ""
	return nil
}

// Cancel stops the execution: every unfinished step and running attempt is cancelled.
func (e *Execution) Cancel(reason string, now time.Time) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || !printable(reason, MaxReasonLen) {
		return invalid("reason", "reason_required")
	}
	if e.State != ExecutionRunning {
		return fmt.Errorf("execution %s is %s: cancel: %w", e.ID, e.State, ErrInvalidTransition)
	}
	e.finish(ExecutionCancelled, reason, now)
	return nil
}

func (e *Execution) finish(state ExecutionState, reason string, now time.Time) {
	for i := range e.Steps {
		s := &e.Steps[i]
		if StepTransitionAllowed(s.State, StepCancelled) {
			s.finishAttempt(AttemptCancelled, "", now)
			s.move(StepCancelled)
			s.RunnerID, s.Reason = "", reason
		}
	}
	e.State, e.Reason, e.FinishedAt = state, reason, &now
}
