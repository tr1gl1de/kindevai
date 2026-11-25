// Package domain is the workflow bounded context: tasks and their state machine, immutable plan revisions with
// a DAG of steps, and executions whose steps runners claim under a lease with a fencing token (KIN-A-14,
// KIN-A-15). Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotFound: no such task, plan revision, execution or step.
	ErrNotFound = errors.New("not found")
	// ErrInvalidTransition: the state machine does not allow the change from the current state.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrStaleRevision: the plan revision is not the task's current one (a newer revision replaced it).
	ErrStaleRevision = errors.New("plan revision is not current")
	// ErrPlanHashMismatch: a stored plan does not hash to its recorded planHash.
	ErrPlanHashMismatch = errors.New("plan hash does not match the plan")
	// ErrStaleFence: the fencing token or runner does not hold the step's current claim.
	ErrStaleFence = errors.New("stale fencing token")
	// ErrLeaseExpired: the claim's lease ran out.
	ErrLeaseExpired = errors.New("lease expired")
	// ErrParallelismLimit: the execution already runs MaxParallel steps.
	ErrParallelismLimit = errors.New("parallelism limit reached")
	// ErrAttemptUnknown: the outcome of the step's last attempt is unknown; it must be reconciled before the step
	// may run again (no second process, KIN-A-14).
	ErrAttemptUnknown = errors.New("last attempt must be reconciled first")
	// ErrConflict: the aggregate changed since it was loaded (optimistic concurrency).
	ErrConflict = errors.New("concurrent modification")
)

// ValidationError reports invalid input for one field with a stable code.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

func invalid(field, code string) error { return &ValidationError{Field: field, Code: code} }

// TaskState is a state of the Task FSM (KIN-A-14).
type TaskState string

const (
	TaskCreated          TaskState = "CREATED"
	TaskAnalyzing        TaskState = "ANALYZING"
	TaskReady            TaskState = "READY"
	TaskAwaitingApproval TaskState = "AWAITING_APPROVAL"
	TaskRunning          TaskState = "RUNNING"
	TaskVerifying        TaskState = "VERIFYING"
	TaskCompleted        TaskState = "COMPLETED"
	TaskBlocked          TaskState = "BLOCKED"
	TaskFailed           TaskState = "FAILED"
	TaskCancelled        TaskState = "CANCELLED"
)

// AllTaskStates lists every task state.
var AllTaskStates = []TaskState{TaskCreated, TaskAnalyzing, TaskReady, TaskAwaitingApproval, TaskRunning,
	TaskVerifying, TaskCompleted, TaskBlocked, TaskFailed, TaskCancelled}

// taskTransitions is the Task FSM. READY/AWAITING_APPROVAL go back to ANALYZING (or to each other) when a plan
// chat creates a new revision (KIN-43); VERIFYING goes back to RUNNING for a fix iteration. Leaving BLOCKED other
// than to FAILED/CANCELLED is Resume, which returns to the state the task was blocked in.
var taskTransitions = map[TaskState][]TaskState{
	TaskCreated:          {TaskAnalyzing, TaskBlocked, TaskCancelled},
	TaskAnalyzing:        {TaskReady, TaskAwaitingApproval, TaskBlocked, TaskFailed, TaskCancelled},
	TaskReady:            {TaskAnalyzing, TaskAwaitingApproval, TaskRunning, TaskBlocked, TaskCancelled},
	TaskAwaitingApproval: {TaskAnalyzing, TaskReady, TaskBlocked, TaskCancelled},
	TaskRunning:          {TaskVerifying, TaskBlocked, TaskFailed, TaskCancelled},
	TaskVerifying:        {TaskRunning, TaskCompleted, TaskBlocked, TaskFailed, TaskCancelled},
	TaskBlocked:          {TaskFailed, TaskCancelled},
	TaskFailed:           {TaskAnalyzing}, // retry (KIN-25): a new revision of the plan, checked by policy and approvals again
}

// TaskTransitionAllowed reports whether the Task FSM has the edge from -> to.
func TaskTransitionAllowed(from, to TaskState) bool {
	return slices.Contains(taskTransitions[from], to)
}

// Terminal reports a final task state (closed; only a FAILED task may be retried, which plans it again).
func (s TaskState) Terminal() bool {
	return s == TaskCompleted || s == TaskFailed || s == TaskCancelled
}

// Valid reports a known task state.
func (s TaskState) Valid() bool { return slices.Contains(AllTaskStates, s) }

// WaitReason says what a BLOCKED task or a WAITING step waits for (owner decision 24.11.2026: there is no
// WAITING_FOR_NETWORK state, it is WAITING with reason network).
type WaitReason string

const (
	WaitNetwork  WaitReason = "network"
	WaitRunner   WaitReason = "runner"
	WaitApproval WaitReason = "approval"
	WaitCLILimit WaitReason = "cli_limit"
	// WaitAgent: no AI CLI is permitted for the step's role in its project (disabled globally or in the project,
	// KIN-33).
	WaitAgent WaitReason = "agent"
	// WaitCI: CI keeps failing on the task's pull request after the last CI fix round, or its merge into the
	// integration branch failed (KIN-37); the Owner decides.
	WaitCI WaitReason = "ci"
)

// Valid reports a known wait reason.
func (r WaitReason) Valid() bool {
	return r == WaitNetwork || r == WaitRunner || r == WaitApproval || r == WaitCLILimit || r == WaitAgent || r == WaitCI
}

// MaxReasonLen bounds the free-text detail of a reason (runes).
const MaxReasonLen = 1000

// Reason explains BLOCKED (Code required, Detail optional) and FAILED/CANCELLED (Detail required, no Code).
// Other states carry no reason.
type Reason struct {
	Code   WaitReason
	Detail string
}

func checkReason(to TaskState, r Reason) (Reason, error) {
	r.Detail = strings.TrimSpace(r.Detail)
	if !printable(r.Detail, MaxReasonLen) {
		return r, invalid("reason", "reason_format")
	}
	switch to {
	case TaskBlocked:
		if !r.Code.Valid() {
			return r, invalid("reason", "wait_reason")
		}
	case TaskFailed, TaskCancelled:
		if r.Code != "" || r.Detail == "" {
			return r, invalid("reason", "reason_required")
		}
	default:
		if r != (Reason{}) {
			return r, invalid("reason", "reason_not_allowed")
		}
	}
	return r, nil
}

// printable reports text of at most maxLen runes without control characters other than newline and tab.
func printable(s string, maxLen int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' })
}

// MaxTitleLen bounds a task title (runes).
const MaxTitleLen = 200

var youtrackIssueRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,19}-[1-9][0-9]{0,9}$`)

// Task is the aggregate for one unit of work in a project, optionally linked to a YouTrack issue.
type Task struct {
	ID            string
	ProjectID     string
	Title         string
	YouTrackIssue string // e.g. "KIN-7"; "" when the task did not come from YouTrack
	State         TaskState
	Reason        Reason     // set only in BLOCKED, FAILED, CANCELLED
	BlockReason   WaitReason // == Reason.Code while BLOCKED
	BlockedFrom   TaskState  // the state Resume returns to; set only while BLOCKED
	PlanRevision  int        // current plan revision; 0 before the first plan
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Version       int64 // optimistic concurrency, maintained by the repository
}

// NewTask validates the input and returns a CREATED task.
func NewTask(id, projectID, title, youtrackIssue, createdBy string, now time.Time) (Task, error) {
	title = strings.TrimSpace(title)
	switch {
	case projectID == "":
		return Task{}, invalid("projectId", "project_required")
	case title == "" || strings.ContainsAny(title, "\n\t") || !printable(title, MaxTitleLen):
		return Task{}, invalid("title", "title_format")
	case youtrackIssue != "" && !youtrackIssueRe.MatchString(youtrackIssue):
		return Task{}, invalid("youtrackIssue", "issue_format")
	}
	return Task{ID: id, ProjectID: projectID, Title: title, YouTrackIssue: youtrackIssue, State: TaskCreated,
		CreatedBy: createdBy, CreatedAt: now, UpdatedAt: now}, nil
}

// Retitle replaces the title (e.g. the linked YouTrack issue's summary changed) and reports whether it changed. A
// finished task keeps its title.
func (t *Task) Retitle(title string, now time.Time) (bool, error) {
	title = strings.TrimSpace(title)
	if title == "" || strings.ContainsAny(title, "\n\t") || !printable(title, MaxTitleLen) {
		return false, invalid("title", "title_format")
	}
	if title == t.Title || t.State.Terminal() {
		return false, nil
	}
	t.Title, t.UpdatedAt = title, now
	return true, nil
}

// Transition moves the task along the FSM with the reason the target state needs.
func (t *Task) Transition(to TaskState, r Reason, now time.Time) error {
	if !TaskTransitionAllowed(t.State, to) {
		return fmt.Errorf("task %s: %s -> %s: %w", t.ID, t.State, to, ErrInvalidTransition)
	}
	r, err := checkReason(to, r)
	if err != nil {
		return err
	}
	t.BlockedFrom, t.BlockReason = "", ""
	if to == TaskBlocked {
		t.BlockedFrom, t.BlockReason = t.State, r.Code
	}
	t.State, t.Reason, t.UpdatedAt = to, r, now
	return nil
}

// Resume returns a BLOCKED task to the state it was blocked in.
func (t *Task) Resume(now time.Time) error {
	if t.State != TaskBlocked || !t.BlockedFrom.Valid() {
		return fmt.Errorf("task %s: resume from %s: %w", t.ID, t.State, ErrInvalidTransition)
	}
	t.State, t.BlockedFrom, t.BlockReason, t.Reason, t.UpdatedAt = t.BlockedFrom, "", "", Reason{}, now
	return nil
}

// AcceptPlan makes revision the current plan and routes the task by the effective risk: L0/L1 to READY, L2/L3 to
// AWAITING_APPROVAL. A newer revision (e.g. from the plan chat) replaces the current one while the task has not
// started running; any approval of an older revision no longer applies.
func (t *Task) AcceptPlan(revision int, risk Risk, now time.Time) error {
	if !risk.Valid() {
		return invalid("risk", "risk_level")
	}
	if err := t.CanAcceptPlan(); err != nil {
		return err
	}
	if revision <= t.PlanRevision {
		return ErrStaleRevision
	}
	to := TaskReady
	if risk.RequiresApproval() {
		to = TaskAwaitingApproval
	}
	if to != t.State {
		if err := t.Transition(to, Reason{}, now); err != nil {
			return err
		}
	}
	t.PlanRevision, t.UpdatedAt = revision, now
	return nil
}

// CanAcceptPlan reports whether a new plan revision may replace the current one (ANALYZING, READY or
// AWAITING_APPROVAL; a running task is cancelled first).
func (t *Task) CanAcceptPlan() error {
	if t.State != TaskAnalyzing && t.State != TaskReady && t.State != TaskAwaitingApproval {
		return fmt.Errorf("task %s: accept plan in %s: %w", t.ID, t.State, ErrInvalidTransition)
	}
	return nil
}

// Approve records the Owner's approval of the current plan revision: AWAITING_APPROVAL -> READY. The approval
// binds to the revision; an older one is ErrStaleRevision (KIN-A-11).
func (t *Task) Approve(revision int, now time.Time) error {
	if t.State != TaskAwaitingApproval {
		return fmt.Errorf("task %s: approve in %s: %w", t.ID, t.State, ErrInvalidTransition)
	}
	if revision != t.PlanRevision {
		return ErrStaleRevision
	}
	return t.Transition(TaskReady, Reason{}, now)
}

// RejectPlan records the Owner's rejection of the current plan revision: AWAITING_APPROVAL -> ANALYZING, where the
// planner or the plan chat (KIN-43) produces a new revision.
func (t *Task) RejectPlan(revision int, now time.Time) error {
	if t.State != TaskAwaitingApproval {
		return fmt.Errorf("task %s: reject plan in %s: %w", t.ID, t.State, ErrInvalidTransition)
	}
	if revision != t.PlanRevision {
		return ErrStaleRevision
	}
	return t.Transition(TaskAnalyzing, Reason{}, now)
}
