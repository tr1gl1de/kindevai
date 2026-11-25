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
// the attempt budget. RUNNING/WAITING -> READY: a phase of the review loop ended and the next one (review, fix round)
// is due (KIN-35). SUCCEEDED -> READY: CI failed on the step's published branch and a CI fix round starts (KIN-37).
var stepTransitions = map[StepState][]StepState{
	StepPending:   {StepReady, StepCancelled},
	StepReady:     {StepClaimed, StepWaiting, StepCancelled},
	StepClaimed:   {StepReady, StepRunning, StepWaiting, StepFailed, StepCancelled},
	StepRunning:   {StepReady, StepSucceeded, StepWaiting, StepFailed, StepCancelled},
	StepWaiting:   {StepReady, StepRunning, StepSucceeded, StepFailed, StepCancelled},
	StepFailed:    {StepReady},
	StepSucceeded: {StepReady},
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
	// Agent time (KIN-25): the CLI adapter that ran the attempt and its process's work period on the runner's
	// clock; WorkIncomplete when a time could not be recorded.
	Agent          string
	WorkStart      *time.Time
	WorkEnd        *time.Time
	WorkIncomplete bool
	// Report is what the AI CLI reported (KIN-79): native session, model, CLI version and usage.
	Report AgentReport
	// The review loop (KIN-35): the attempt's role (implementer, reviewer; "" outside the loop) and fix round, the
	// test command it was sent and the runner's test evidence, and its final workspace report (base, head, files).
	Role        string
	Round       int
	TestCommand string
	Tests       *TestRun
	BaseSHA     string
	HeadSHA     string
	Files       []string
	FilesTotal  int
	// PushedBranch is the branch a publish attempt reported it pushed HeadSHA to (KIN-36).
	PushedBranch string
}

// AgentReport is an AI CLI run's report on one attempt (KIN-79, KB KIN-A-16).
type AgentReport struct {
	Session string
	Model   string
	Version string
	Usage   AgentUsage
	// Result is the CLI's final answer (untrusted; at most MaxAgentResult bytes): the implementer's handoff or the
	// reviewer's verdict (KIN-35).
	Result string
}

// AgentUsage is the token usage and cost of an attempt's CLI run.
type AgentUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	Turns            int
}

// Add accumulates o into u.
func (u *AgentUsage) Add(o AgentUsage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CostUSD += o.CostUSD
	u.Turns += o.Turns
}

// RecordAgentReport stores the CLI's report with the attempt of runnerID under fence, whatever the step's state
// (a cancelled or superseded attempt still used tokens). It reports whether the attempt changed.
func (e *Execution) RecordAgentReport(stepID, runnerID string, fence int64, r AgentReport) bool {
	a := e.attemptFor(stepID, runnerID, fence)
	if a == nil || a.Report == r {
		return false
	}
	r.Session, r.Model, r.Version = cutUTF8(r.Session, 128), cutUTF8(r.Model, 128), cutUTF8(r.Version, 64)
	r.Result = cutUTF8(r.Result, MaxAgentResult)
	if a.Report == r {
		return false
	}
	a.Report = r
	return true
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
	// WaitUntil is when a step WAITING for its CLI's limit is retried (KIN-79); nil otherwise.
	WaitUntil *time.Time
	// NetworkHolds counts the step's network holds in a row on the same runner (KIN-81): each doubles the retry
	// delay and from NetworkAlertAfter on the Owner is alerted. An attempt that ends otherwise resets it.
	NetworkHolds int
	Reason       string // why the step failed or was cancelled
	// Fence is the last fencing token issued for the step. It only grows, across attempts and runners.
	Fence          int64
	RunnerID       string     // holder of the current claim (CLAIMED, RUNNING, or WAITING on an unknown attempt)
	LeaseExpiresAt *time.Time // set while CLAIMED or RUNNING
	Attempts       []Attempt
	// Agent and Model are the AI CLI and model of the latest claim (KIN-33): "mock" for mock plans, "" before the
	// first claim. They stay after the attempt ends, so a review step can tell which CLI authored the work.
	Agent string
	Model string
	// The review loop (KIN-35, review.go): Review is on for a routed implementer step on an AI CLI; Phase is the
	// current phase, Round the fix round (0 for the first implementation), PinnedRunner the runner that holds the
	// step's work branch after its first implementation and TestCommand the test command of the current claim.
	Review       bool
	Phase        string
	Round        int
	PinnedRunner string
	TestCommand  string
	// AttemptBase is the number of attempts before the loop last started over (its runner was released): only later
	// attempts count for the loop's budgets and evidence.
	AttemptBase int
	// CIRound counts the CI fix rounds of the step's published branch (KIN-37, ci.go); CIFeedback is why CI failed,
	// told to the implementer of the current CI fix round.
	CIRound    int
	CIFeedback string
}

// WaitingForNetwork is what the UI shows as WAITING_FOR_NETWORK.
func (s *Step) WaitingForNetwork() bool { return s.State == StepWaiting && s.WaitReason == WaitNetwork }

// AttemptUnknown reports a step WAITING on an attempt whose outcome is unknown (its runner was lost): it must be
// reconciled before the step may run again.
func (s *Step) AttemptUnknown() bool { return s.unknownAttempt() }

// Holding reports a step whose runner may have a process for it: CLAIMED, RUNNING or an unknown attempt.
func (s *Step) Holding() bool {
	return s.RunnerID != "" && (s.State == StepClaimed || s.State == StepRunning || s.unknownAttempt())
}

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
		s.WaitReason, s.WaitUntil = "", nil
	}
	if to != StepClaimed && to != StepRunning {
		s.LeaseExpiresAt = nil
	}
}

func (s *Step) finishAttempt(state AttemptState, msg string, now time.Time) {
	s.NetworkHolds = 0 // HoldForNetwork sets it again
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
		s.WaitUntil = clonePtr(s.WaitUntil)
		s.Attempts = slices.Clone(s.Attempts)
		for j, a := range s.Attempts {
			s.Attempts[j].Tests = clonePtr(a.Tests)
			s.Attempts[j].Files = slices.Clone(a.Files)
			s.Attempts[j].FinishedAt = clonePtr(a.FinishedAt)
			s.Attempts[j].WorkStart, s.Attempts[j].WorkEnd = clonePtr(a.WorkStart), clonePtr(a.WorkEnd)
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

// Claim gives runnerID the READY step under a new fencing token and a lease of ttl (no AI CLI recorded).
func (e *Execution) Claim(stepID, runnerID string, now time.Time, ttl time.Duration) (Lease, error) {
	return e.ClaimWith(stepID, runnerID, AgentChoice{}, now, ttl)
}

// ClaimWith is Claim for the AI CLI and model routing chose (KIN-33); they are recorded on the step and its attempt.
func (e *Execution) ClaimWith(stepID, runnerID string, agent AgentChoice, now time.Time, ttl time.Duration) (Lease, error) {
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
	s.Agent, s.Model = agent.Agent, agent.Model
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
	s.Attempts = append(s.Attempts, s.newAttempt(runnerID, fence, now))
	return nil
}

// newAttempt is the step's next attempt under fence, with the phase's role, round and test command.
func (s *Step) newAttempt(runnerID string, fence int64, now time.Time) Attempt {
	return Attempt{Number: len(s.Attempts) + 1, RunnerID: runnerID, Fence: fence, State: AttemptRunning, StartedAt: now,
		Agent: s.Agent, Role: s.attemptRole(), Round: s.Round, TestCommand: s.TestCommand}
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
	return e.SucceedWithin(stepID, runnerID, fence, now, DefaultMaxAttempts)
}

// SucceedWithin is Succeed with the retry budget a review-loop attempt that did not pass (failed tests, an invalid
// verdict, KIN-35) is retried within.
func (e *Execution) SucceedWithin(stepID, runnerID string, fence int64, now time.Time, maxAttempts int) error {
	s, err := e.holder(stepID, runnerID, fence, "succeed")
	if err != nil {
		return err
	}
	if !reportable(s) {
		return invalidStep(s, "succeed")
	}
	if s.Review && !e.finishPhase(s, now, maxAttempts) {
		return nil
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
	msg = reasonText(msg)
	if msg == "" {
		msg = "failed"
	}
	if s.State == StepClaimed { // the launch itself failed: it still counts as an attempt
		s.Attempts = append(s.Attempts, s.newAttempt(runnerID, fence, now))
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

// reasonText trims a reason and bounds it to MaxReasonLen runes.
func reasonText(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxReasonLen {
		return string(r[:MaxReasonLen])
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

// Released is an attempt ReleaseRunner took from its runner: the runner must stop the process with this token.
type Released struct {
	StepID string
	Fence  int64
}

// ReleaseRunner takes every step runnerID holds away from it at once (KIN-63: the Owner disabled or revoked the
// runner). A RUNNING step or an UNKNOWN attempt fails with msg and is retried on another runner within
// maxAttempts; a CLAIMED step never started, so it returns to READY without using the budget. The tokens are
// released, so any later report of these attempts is stale. It returns the released attempts in step order.
func (e *Execution) ReleaseRunner(runnerID, msg string, now time.Time, maxAttempts int) []Released {
	if e.State != ExecutionRunning || runnerID == "" {
		return nil
	}
	msg = reasonText(msg)
	var out []Released
	for i := range e.Steps {
		s := &e.Steps[i]
		if s.RunnerID != runnerID || !s.Holding() {
			continue
		}
		out = append(out, Released{StepID: s.ID, Fence: s.Fence})
		if s.State == StepClaimed {
			s.move(StepReady)
			s.RunnerID = ""
			continue
		}
		// A step that uses up its budget fails the execution, which cancels the remaining steps (no longer held).
		e.failAttempt(s, msg, now, maxAttempts)
	}
	// A review loop whose work branch lives on this runner starts over elsewhere (KIN-35): nothing else can reach it.
	for i := range e.Steps {
		if s := &e.Steps[i]; s.PinnedRunner == runnerID && !s.Holding() && e.State == ExecutionRunning &&
			(s.State == StepReady || s.State == StepWaiting) {
			s.restartReview()
		}
	}
	return out
}

// AgentReleased is an attempt ReleaseAgent took away: its runner must stop the process with this token.
type AgentReleased struct {
	StepID   string
	RunnerID string
	Fence    int64
}

// ReleaseAgent takes every step held with agent away at once (KIN-33: the Owner's emergency stop of a CLI). A
// CLAIMED step returns to READY; a RUNNING step or an UNKNOWN attempt is cancelled with msg — not the step's fault,
// so it does not use the retry budget — and the step WAITs with reason agent, which the scheduler routes again
// (another CLI of its chain, or it keeps waiting). The tokens are released, so later reports of these attempts are stale.
func (e *Execution) ReleaseAgent(agent, msg string, now time.Time) []AgentReleased {
	if e.State != ExecutionRunning || agent == "" {
		return nil
	}
	msg = reasonText(msg)
	var out []AgentReleased
	for i := range e.Steps {
		s := &e.Steps[i]
		if s.Agent != agent || !s.Holding() {
			continue
		}
		out = append(out, AgentReleased{StepID: s.ID, RunnerID: s.RunnerID, Fence: s.Fence})
		s.RunnerID = ""
		if s.State == StepClaimed {
			s.move(StepReady)
			continue
		}
		s.finishAttempt(AttemptCancelled, msg, now)
		s.move(StepWaiting)
		s.WaitReason = WaitAgent
	}
	return out
}

// HoldForApproval stops a CLAIMED or RUNNING step whose runner reported changes beyond the approved scope: the
// attempt is cancelled (it does not use the retry budget), the claim is released so the old process's later
// reports are stale, and the step WAITs with reason approval until ResumeStep after a new approval.
func (e *Execution) HoldForApproval(stepID, runnerID string, fence int64, msg string, now time.Time) error {
	return e.hold(stepID, runnerID, fence, msg, WaitApproval, now)
}

// HoldForNetwork parks a CLAIMED or RUNNING step whose runner could not build or verify the strict egress sandbox
// (KIN-31, fail-closed: the attempt never ran unconfined). Like a hold for approval the attempt is cancelled (no
// retry budget used) and the claim released; the step WAITs with reason network until the scheduler retries it
// (NetworkRetryDelayAfter its holds in a row, after the attempt ended; KIN-81).
func (e *Execution) HoldForNetwork(stepID, runnerID string, fence int64, msg string, now time.Time) error {
	s := e.Step(stepID)
	if s == nil {
		return e.hold(stepID, runnerID, fence, msg, WaitNetwork, now)
	}
	holds := s.NetworkHolds // the hold's finishAttempt resets it
	if err := e.hold(stepID, runnerID, fence, msg, WaitNetwork, now); err != nil {
		return err
	}
	if n := len(s.Attempts); n < 2 || s.Attempts[n-2].RunnerID != runnerID {
		holds = 0 // the streak is per runner
	}
	s.NetworkHolds = holds + 1
	return nil
}

// NetworkRetryDelay is how long a step held for the network once waits before the scheduler assigns it again;
// every further hold in a row doubles it (KIN-81), up to MaxNetworkRetryDelay. From NetworkAlertAfter holds in a
// row on one runner the Owner is alerted (its strict egress keeps failing).
const (
	NetworkRetryDelay    = 2 * time.Minute
	MaxNetworkRetryDelay = 30 * time.Minute
	NetworkAlertAfter    = 3
)

// NetworkRetryDelayAfter is the retry delay after holds network holds in a row.
func NetworkRetryDelayAfter(holds int) time.Duration {
	d := NetworkRetryDelay
	for i := 1; i < holds && d < MaxNetworkRetryDelay; i++ {
		d *= 2
	}
	return min(d, MaxNetworkRetryDelay)
}

// NetworkRetryDue reports a step WAITING for the network whose last attempt ended at least its backed-off retry
// delay ago.
func (s *Step) NetworkRetryDue(now time.Time) bool {
	if !s.WaitingForNetwork() {
		return false
	}
	a := s.lastAttempt()
	return a == nil || a.FinishedAt == nil || !now.Before(a.FinishedAt.Add(NetworkRetryDelayAfter(s.NetworkHolds)))
}

// HoldForCLILimit parks a CLAIMED or RUNNING step whose AI CLI hit its subscription/rate limit or is logged out
// (KIN-79): like a hold for the network the attempt is cancelled (no retry budget used) and the claim released; the
// step WAITs with reason cli_limit until resetsAt (CLILimitRetryDelay from now when the CLI did not say, or said a
// time already past), when the scheduler assigns it again.
func (e *Execution) HoldForCLILimit(stepID, runnerID string, fence int64, msg string, resetsAt, now time.Time) error {
	if err := e.hold(stepID, runnerID, fence, msg, WaitCLILimit, now); err != nil {
		return err
	}
	until := resetsAt.UTC()
	if resetsAt.IsZero() || !resetsAt.After(now) {
		until = now.Add(CLILimitRetryDelay).UTC()
	}
	if limit := now.Add(MaxCLILimitWait); until.After(limit) {
		until = limit.UTC()
	}
	e.Step(stepID).WaitUntil = &until
	return nil
}

// RetryCLILimitAt sets when a step WAITING for its CLI's limit is retried (the hold routing decided, KIN-83).
func (e *Execution) RetryCLILimitAt(stepID string, until time.Time) error {
	s, err := e.step(stepID, "wait")
	if err != nil {
		return err
	}
	if s.State != StepWaiting || s.WaitReason != WaitCLILimit {
		return invalidStep(s, "wait")
	}
	until = until.UTC()
	s.WaitUntil = &until
	return nil
}

// CLILimitRetryDelay is how long a step held for its CLI's limit waits when the CLI named no reset time;
// MaxCLILimitWait bounds a reset time the CLI named.
const (
	CLILimitRetryDelay = 30 * time.Minute
	MaxCLILimitWait    = 7 * 24 * time.Hour
)

// CLILimitRetryDue reports a step WAITING for its CLI's limit whose wait is over.
func (s *Step) CLILimitRetryDue(now time.Time) bool {
	return s.State == StepWaiting && s.WaitReason == WaitCLILimit && (s.WaitUntil == nil || !now.Before(*s.WaitUntil))
}

func (e *Execution) hold(stepID, runnerID string, fence int64, msg string, reason WaitReason, now time.Time) error {
	s, err := e.holder(stepID, runnerID, fence, "hold")
	if err != nil {
		return err
	}
	if !reportable(s) && s.State != StepClaimed {
		return invalidStep(s, "hold")
	}
	msg = reasonText(msg)
	if s.State == StepClaimed {
		s.Attempts = append(s.Attempts, s.newAttempt(runnerID, fence, now))
	}
	s.finishAttempt(AttemptCancelled, msg, now)
	s.move(StepWaiting)
	s.WaitReason, s.RunnerID = reason, ""
	return nil
}

// countedAttempts are the attempts that use the retry budget: cancelled ones (held for approval) do not, nor do
// those of another phase or fix round of the review loop (each has its own budget, KIN-35).
func (s *Step) countedAttempts() int {
	n := 0
	role := s.attemptRole()
	for _, a := range s.Attempts[min(s.AttemptBase, len(s.Attempts)):] {
		if a.State != AttemptCancelled && a.Role == role && a.Round == s.Round {
			n++
		}
	}
	return n
}

// WaitStep parks a READY step that cannot be scheduled (no runner, network, CLI limit, approval, agent), or changes
// the reason of a WAITING one.
func (e *Execution) WaitStep(stepID string, reason WaitReason, _ time.Time) error {
	if !reason.Valid() || reason == WaitCI { // ci blocks a task, never a step
		return invalid("reason", "wait_reason")
	}
	s, err := e.step(stepID, "wait")
	if err != nil {
		return err
	}
	// A step waiting without an unknown attempt may change what it waits for (the scheduler's routing, KIN-33).
	if s.State != StepReady && (s.State != StepWaiting || s.unknownAttempt()) {
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

// DefaultMaxAttempts is the retry budget of a step (one retry after a confirmed failure, KIN-23), per phase and
// fix round of the review loop.
const DefaultMaxAttempts = 2
