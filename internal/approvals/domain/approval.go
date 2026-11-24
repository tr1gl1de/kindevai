package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Scope is the boundary an approval opens (KIN-A-12 "Три независимые границы"). Release into main/master is
// always manual and outside the platform, so there is no scope for it.
type Scope string

const (
	ScopeExecute      Scope = "execute"       // develop the task by this plan revision
	ScopeIntegrateDev Scope = "integrate_dev" // merge/push the verified result into dev
)

// Valid reports a known scope.
func (s Scope) Valid() bool { return s == ScopeExecute || s == ScopeIntegrateDev }

// Reason says why an approval was requested.
type Reason string

const (
	ReasonPlan          Reason = "plan"           // a plan revision at L2/L3, or integrate_dev of one
	ReasonScopeExceeded Reason = "scope_exceeded" // a running step changed files beyond its approved scope
)

// State is an approval's lifecycle: pending until the Owner decides; superseded when a newer plan revision or
// the task's end makes it moot. Decided and superseded approvals never change again.
type State string

const (
	StatePending    State = "pending"
	StateApproved   State = "approved"
	StateRejected   State = "rejected"
	StateSuperseded State = "superseded"
)

// Valid reports a known state.
func (s State) Valid() bool {
	return s == StatePending || s == StateApproved || s == StateRejected || s == StateSuperseded
}

// PolicyApprover is the approver label of an approval granted by the policy (L0/L1 integrate_dev).
const PolicyApprover = "policy"

// Limits.
const (
	MaxCommentLen = 2000
	maxPaths      = 500
)

var planHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Request is the input for a new approval.
type Request struct {
	ID           string
	ProjectID    string
	TaskID       string
	TaskTitle    string // snapshot for the Approval Center list
	Revision     int
	PlanHash     string
	Scope        Scope
	Reason       Reason
	StepKey      string   // the step that exceeded its scope (ReasonScopeExceeded)
	StepID       string   // its execution step, resumed on approval
	Paths        []string // the files beyond the scope; approving adds them to the step's scope
	Risk         Risk     // effective risk of the revision
	ProposedRisk Risk     // what the planner proposed
	Findings     []Finding
}

// Approval binds a decision to one task, plan revision and planHash, scope and risk (KIN-A-14 "Approval").
type Approval struct {
	ID           string
	ProjectID    string
	TaskID       string
	TaskTitle    string
	Revision     int
	PlanHash     string
	Scope        Scope
	Reason       Reason
	StepKey      string
	StepID       string
	Paths        []string
	Risk         Risk
	ProposedRisk Risk
	Findings     []Finding
	State        State
	ApproverID   string // user id; "" for the policy
	Approver     string // login at decision time, or PolicyApprover
	Comment      string
	RequestedAt  time.Time
	DecidedAt    *time.Time
}

// NewRequest validates r and returns a pending approval.
func NewRequest(r Request, now time.Time) (Approval, error) {
	switch {
	case r.ID == "" || r.ProjectID == "" || r.TaskID == "":
		return Approval{}, invalid("taskId", "task_required")
	case r.Revision < 1:
		return Approval{}, invalid("planRevision", "revision_number")
	case !planHashRe.MatchString(r.PlanHash):
		return Approval{}, invalid("planHash", "hash_format")
	case !r.Scope.Valid():
		return Approval{}, invalid("scope", "scope_unknown")
	case !r.Risk.Valid() || (r.ProposedRisk != "" && !r.ProposedRisk.Valid()):
		return Approval{}, invalid("risk", "risk_level")
	case r.Reason != ReasonPlan && r.Reason != ReasonScopeExceeded:
		return Approval{}, invalid("reason", "reason_unknown")
	case r.Reason == ReasonScopeExceeded && (r.StepKey == "" || r.StepID == "" || r.Scope != ScopeExecute):
		return Approval{}, invalid("stepKey", "step_required")
	}
	paths := slices.Clone(r.Paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) > maxPaths {
		paths = paths[:maxPaths]
	}
	if paths == nil {
		paths = []string{}
	}
	findings := slices.Clone(r.Findings)
	if findings == nil {
		findings = []Finding{}
	}
	return Approval{ID: r.ID, ProjectID: r.ProjectID, TaskID: r.TaskID, TaskTitle: r.TaskTitle, Revision: r.Revision,
		PlanHash: r.PlanHash, Scope: r.Scope, Reason: r.Reason, StepKey: r.StepKey, StepID: r.StepID, Paths: paths, Risk: r.Risk,
		ProposedRisk: r.ProposedRisk, Findings: findings, State: StatePending, RequestedAt: now}, nil
}

// GrantByPolicy records integrate_dev approved by the policy for an L0/L1 revision (auto-merge into dev).
func GrantByPolicy(r Request, now time.Time) (Approval, error) {
	if r.Scope != ScopeIntegrateDev || !r.Risk.IntegrateDevAutoApproved() {
		return Approval{}, invalid("scope", "policy_grant_not_allowed")
	}
	a, err := NewRequest(r, now)
	if err != nil {
		return Approval{}, err
	}
	a.State, a.Approver, a.DecidedAt = StateApproved, PolicyApprover, &now
	return a, nil
}

// CanDecide: only the platform Owner approves or rejects (Member and Project Admin get 403, KIN-A-21).
func CanDecide(owner bool) bool { return owner }

// Approver is who decides.
type Approver struct {
	ID    string
	Login string
}

// Decision is the Owner's answer for the plan revision and hash they reviewed.
type Decision struct {
	Approver Approver
	Approve  bool
	Revision int
	PlanHash string
	Comment  string // required to reject
}

// Decide approves or rejects a pending approval. The decision must name the revision and hash the approval binds
// to; anything else (or a superseded approval) is ErrStaleRevision.
func (a *Approval) Decide(d Decision, now time.Time) error {
	switch a.State {
	case StatePending:
	case StateSuperseded:
		return fmt.Errorf("approval %s: %w", a.ID, ErrStaleRevision)
	default:
		return fmt.Errorf("approval %s is %s: %w", a.ID, a.State, ErrAlreadyDecided)
	}
	if d.Revision != a.Revision || d.PlanHash != a.PlanHash {
		return fmt.Errorf("approval %s binds revision %d: %w", a.ID, a.Revision, ErrStaleRevision)
	}
	comment := strings.TrimSpace(d.Comment)
	switch {
	case d.Approver.ID == "":
		return invalid("approver", "approver_required")
	case !printable(comment, MaxCommentLen):
		return invalid("comment", "comment_format")
	case !d.Approve && comment == "":
		return invalid("comment", "comment_required")
	}
	a.State = StateRejected
	if d.Approve {
		a.State = StateApproved
	}
	a.ApproverID, a.Approver, a.Comment, a.DecidedAt = d.Approver.ID, d.Approver.Login, comment, &now
	return nil
}

// Supersede retires a pending approval (a newer plan revision, or the task ended). Decided ones keep their record.
func (a *Approval) Supersede(now time.Time) {
	if a.State == StatePending {
		a.State, a.DecidedAt = StateSuperseded, &now
	}
}
