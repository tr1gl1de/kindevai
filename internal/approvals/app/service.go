// Package app holds the approvals use cases: the Policy Evaluator the workflow asks for a plan's effective risk,
// approval requests created when a plan revision is accepted, the Owner's decisions in the Approval Center, the
// approved-scope check of running steps and the project risk policy (KIN-24; KIN-A-12, KIN-A-14).
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// Audit event kinds recorded by the approvals context.
const (
	KindRequested       = "approval.requested"
	KindGrantedByPolicy = "approval.granted_by_policy"
	KindApproved        = "approval.approved"
	KindRejected        = "approval.rejected"
	KindSuperseded      = "approval.superseded"
	KindPolicyUpdated   = "risk_policy.updated"
)

// Outbox: the Owner is notified of a new pending approval. The Telegram adapter (KIN-40) registers the handler;
// until then the message waits in the dead-letter list and can be retried from there.
const (
	AdapterTelegram        = "telegram"
	TopicApprovalRequested = "telegram.approval_requested"
)

// Workflow task states the approvals context reacts to.
const (
	TaskAwaitingApproval = "AWAITING_APPROVAL"
	TaskRunning          = "RUNNING"
	TaskVerifying        = "VERIFYING"
)

// Project roles as the projects context reports them.
const (
	RoleOwner        = "owner"
	RoleProjectAdmin = "project_admin"
	RoleMember       = "member"
)

var (
	// ErrDuplicatePending: the task already has a pending approval of this scope (and step).
	ErrDuplicatePending = errors.New("a pending approval already exists")
	// ErrConflict: the approval changed since it was loaded (it is no longer pending).
	ErrConflict = errors.New("concurrent modification")
)

// Filter selects approvals for the list.
type Filter struct {
	AllProjects bool
	ProjectIDs  []string
	States      []domain.State // empty = all
	TaskID      string         // empty = all
	Limit       int
}

// Repository stores approvals and project policies; every method runs on the caller's transaction (db.Conn).
type Repository interface {
	Policy(ctx context.Context, projectID string) (domain.Policy, bool, error)
	SavePolicy(ctx context.Context, projectID string, p domain.Policy, updatedBy string, now time.Time) error
	Create(ctx context.Context, a domain.Approval) error // ErrDuplicatePending
	Get(ctx context.Context, id string) (domain.Approval, error)
	Save(ctx context.Context, a domain.Approval) error // only a pending approval changes; else ErrConflict
	ForTask(ctx context.Context, taskID string) ([]domain.Approval, error)
	List(ctx context.Context, f Filter) ([]domain.Approval, error) // newest first
}

// TaskRef is a workflow task as the approvals context sees it.
type TaskRef struct {
	ID           string
	ProjectID    string
	Title        string
	State        string
	PlanRevision int
}

// RevisionRef is a stored plan revision.
type RevisionRef struct {
	Number    int
	Hash      string
	Risk      domain.Risk // effective
	Plan      domain.PlanSnapshot
	CreatedBy string
	CreatedAt time.Time
}

// StepRef is an execution step with its approved scope.
type StepRef struct {
	StepID     string
	TaskID     string
	Key        string
	Repository string
	Revision   int
	PlanHash   string
	Risk       domain.Risk // the revision's effective (approved) risk
	Scope      []string
}

// Workflow is the workflow context's app API as approvals needs it. Errors: domain.ErrNotFound,
// domain.ErrStaleRevision (not the current revision/hash), domain.ErrNotReady (the task's state forbids it).
type Workflow interface {
	Task(ctx context.Context, id string) (TaskRef, error)
	Revision(ctx context.Context, taskID string, number int) (RevisionRef, error)
	ApprovePlan(ctx context.Context, taskID string, revision int, hash string) error
	RejectPlan(ctx context.Context, taskID string, revision int, hash string) error
	Step(ctx context.Context, stepID string) (StepRef, error)
	HoldStep(ctx context.Context, stepID, runnerID string, fence int64, msg string) error
	ResumeStep(ctx context.Context, stepID string) error // only a step held for approval
	CancelTask(ctx context.Context, taskID, reason string) error
}

// ProjectRef is a project visible to a user, with their role ("owner" for the platform Owner).
type ProjectRef struct {
	ID   string
	Name string
	Role string
}

// Projects is the projects context's app API as approvals needs it.
type Projects interface {
	Visible(ctx context.Context, p auth.Principal) ([]ProjectRef, error)
}

// AuditLog records audit events in the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Outbox stores outgoing messages in the caller's transaction.
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// TxRunner runs fn in one database transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Service implements the approvals use cases.
type Service struct {
	Repo     Repository
	Workflow Workflow
	Projects Projects
	Tx       TxRunner
	Audit    AuditLog
	Outbox   Outbox
	Now      func() time.Time
	NewID    func() string
}

func (s *Service) now() time.Time { return s.Now().UTC() }

func (s *Service) policy(ctx context.Context, projectID string) (domain.Policy, bool, error) {
	p, ok, err := s.Repo.Policy(ctx, projectID)
	if err != nil {
		return domain.Policy{}, false, fmt.Errorf("loading risk policy: %w", err)
	}
	if !ok {
		return domain.DefaultPolicy(), false, nil
	}
	return p, true, nil
}

// EffectiveRisk is the Policy Evaluator: the plan's proposed risk raised by the project's rules, never lowered.
func (s *Service) EffectiveRisk(ctx context.Context, projectID string, plan domain.PlanSnapshot) (domain.Risk, error) {
	pol, _, err := s.policy(ctx, projectID)
	if err != nil {
		return "", err
	}
	return pol.Evaluate(plan.Risk, plan.Targets()).Risk, nil
}

// PlanEvent is the workflow's "plan revision accepted" event.
type PlanEvent struct {
	TaskID    string
	ProjectID string
	TaskTitle string
	Revision  int
	PlanHash  string
	Risk      domain.Risk // effective risk stored with the revision
	Plan      domain.PlanSnapshot
}

func (s *Service) record(ctx context.Context, actor auditapp.Actor, kind string, a domain.Approval, extra map[string]string) error {
	d := map[string]string{"taskId": a.TaskID, "planRevision": strconv.Itoa(a.Revision), "planHash": a.PlanHash,
		"scope": string(a.Scope), "risk": string(a.Risk)}
	if a.StepKey != "" {
		d["stepKey"] = a.StepKey
	}
	for k, v := range extra {
		d[k] = v
	}
	return s.Audit.Record(ctx, auditapp.Entry{Actor: actor, Kind: kind, ProjectID: a.ProjectID,
		Subject: "approval:" + a.ID, Details: d})
}

// notify enqueues the Owner notification for a new pending approval (ids only: the handler loads the rest).
func (s *Service) notify(ctx context.Context, a domain.Approval) error {
	payload, err := json.Marshal(map[string]any{"approvalId": a.ID, "projectId": a.ProjectID, "taskId": a.TaskID,
		"planRevision": a.Revision, "scope": a.Scope, "reason": a.Reason, "risk": a.Risk})
	if err != nil {
		return err
	}
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterTelegram, Topic: TopicApprovalRequested,
		Key: "approval/" + a.ID + "/requested", Payload: payload}); err != nil {
		return fmt.Errorf("enqueueing approval notification: %w", err)
	}
	return nil
}

func (s *Service) request(ctx context.Context, r domain.Request, now time.Time, notify bool) error {
	a, err := domain.NewRequest(r, now)
	if err != nil {
		return err
	}
	if err := s.Repo.Create(ctx, a); err != nil {
		return fmt.Errorf("creating approval: %w", err)
	}
	if err := s.record(ctx, auditapp.System(), KindRequested, a, map[string]string{"reason": string(a.Reason)}); err != nil {
		return err
	}
	if notify {
		return s.notify(ctx, a)
	}
	return nil
}

// supersede retires the task's pending approvals that keep(a) selects.
func (s *Service) supersede(ctx context.Context, taskID string, now time.Time, keep func(domain.Approval) bool) error {
	all, err := s.Repo.ForTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("loading approvals: %w", err)
	}
	for _, a := range all {
		if a.State != domain.StatePending || !keep(a) {
			continue
		}
		a.Supersede(now)
		if err := s.Repo.Save(ctx, a); err != nil {
			return fmt.Errorf("superseding approval %s: %w", a.ID, err)
		}
		if err := s.record(ctx, auditapp.System(), KindSuperseded, a, nil); err != nil {
			return err
		}
	}
	return nil
}

// PlanAccepted runs in the workflow's transaction when a plan revision becomes current: pending approvals of older
// revisions are superseded; an L2/L3 revision gets a pending execute approval (the Owner is notified) and a
// separate pending integrate_dev request; an L0/L1 revision gets integrate_dev granted by the policy.
func (s *Service) PlanAccepted(ctx context.Context, ev PlanEvent) error {
	now := s.now()
	if err := s.supersede(ctx, ev.TaskID, now, func(a domain.Approval) bool { return a.Revision < ev.Revision }); err != nil {
		return err
	}
	pol, _, err := s.policy(ctx, ev.ProjectID)
	if err != nil {
		return err
	}
	assessment := pol.Evaluate(ev.Plan.Risk, ev.Plan.Targets())
	base := domain.Request{ProjectID: ev.ProjectID, TaskID: ev.TaskID, TaskTitle: ev.TaskTitle, Revision: ev.Revision,
		PlanHash: ev.PlanHash, Reason: domain.ReasonPlan, Risk: ev.Risk, ProposedRisk: ev.Plan.Risk,
		Findings: assessment.Findings}
	if !ev.Risk.RequiresApproval() {
		base.ID, base.Scope = s.NewID(), domain.ScopeIntegrateDev
		a, err := domain.GrantByPolicy(base, now)
		if err != nil {
			return err
		}
		if err := s.Repo.Create(ctx, a); err != nil {
			return fmt.Errorf("creating approval: %w", err)
		}
		return s.record(ctx, auditapp.System(), KindGrantedByPolicy, a, nil)
	}
	exec := base
	exec.ID, exec.Scope = s.NewID(), domain.ScopeExecute
	if err := s.request(ctx, exec, now, true); err != nil {
		return err
	}
	integ := base
	integ.ID, integ.Scope = s.NewID(), domain.ScopeIntegrateDev
	return s.request(ctx, integ, now, false) // actionable only after verification (KIN-37 notifies then)
}

// TaskClosed runs in the workflow's transaction when a task is cancelled or failed: its pending approvals are moot.
func (s *Service) TaskClosed(ctx context.Context, taskID string) error {
	return s.supersede(ctx, taskID, s.now(), func(domain.Approval) bool { return true })
}

// IntegrateDevApproved reports whether the task's current plan revision may be merged into dev: granted by the
// policy (L0/L1) or approved by the Owner (L2/L3). For the merge gate (KIN-37).
func (s *Service) IntegrateDevApproved(ctx context.Context, taskID string) (bool, error) {
	t, err := s.Workflow.Task(ctx, taskID)
	if err != nil {
		return false, err
	}
	rev, err := s.Workflow.Revision(ctx, taskID, t.PlanRevision)
	if err != nil {
		return false, err
	}
	all, err := s.Repo.ForTask(ctx, taskID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(all, func(a domain.Approval) bool {
		return a.Scope == domain.ScopeIntegrateDev && a.State == domain.StateApproved && a.Revision == rev.Number &&
			a.PlanHash == rev.Hash
	}), nil
}

// StepChanges is a runner's report of the files a step changed (from the authenticated runner channel).
type StepChanges struct {
	StepID   string
	RunnerID string
	Fence    int64
	Changed  []string
}

// MaxChangedFiles bounds the files one report is checked against; a longer report stops the step anyway.
const MaxChangedFiles = 2000

// approvedExtensions lists the files approved beyond the plan scope for the step of this revision.
func approvedExtensions(all []domain.Approval, revision int, stepKey string) []string {
	var out []string
	for _, a := range all {
		if a.Reason == domain.ReasonScopeExceeded && a.State == domain.StateApproved && a.Revision == revision &&
			a.StepKey == stepKey {
			out = append(out, a.Paths...)
		}
	}
	return out
}

// CheckChanges compares a step's changed files with its approved scope (plan scope plus approved extensions) and
// the revision's approved risk. On a violation the step is stopped (WAITING approval) and a scope_exceeded execute
// approval is requested from the Owner, in one transaction.
func (s *Service) CheckChanges(ctx context.Context, ch StepChanges) (domain.Violation, error) {
	var v domain.Violation
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		st, err := s.Workflow.Step(ctx, ch.StepID)
		if err != nil {
			return err
		}
		t, err := s.Workflow.Task(ctx, st.TaskID)
		if err != nil {
			return err
		}
		pol, _, err := s.policy(ctx, t.ProjectID)
		if err != nil {
			return err
		}
		all, err := s.Repo.ForTask(ctx, st.TaskID)
		if err != nil {
			return err
		}
		// Files the Owner already approved for this step are covered, whatever the policy rates them; only valid,
		// cleaned paths can match an approved extension.
		ext := approvedExtensions(all, st.Revision, st.Key)
		changed := ch.Changed
		more := len(changed) - MaxChangedFiles
		if more > 0 {
			changed = changed[:MaxChangedFiles]
		}
		changed = slices.DeleteFunc(slices.Clone(changed), func(f string) bool {
			return domain.ValidChangedPath(f) && slices.Contains(ext, path.Clean(f))
		})
		v = domain.CheckChanges(st.Scope, st.Risk, pol, st.Repository, changed)
		if more > 0 { // the rest is checked again after the approval; the step must not run on unchecked files
			v.Outside = append(v.Outside, fmt.Sprintf("(+%d)", more))
		}
		if v.Empty() {
			return nil
		}
		if err := s.Workflow.HoldStep(ctx, ch.StepID, ch.RunnerID, ch.Fence, "changes beyond the approved scope"); err != nil {
			return err
		}
		return s.request(ctx, domain.Request{ID: s.NewID(), ProjectID: t.ProjectID, TaskID: t.ID, TaskTitle: t.Title,
			Revision: st.Revision, PlanHash: st.PlanHash, Scope: domain.ScopeExecute, Reason: domain.ReasonScopeExceeded,
			StepKey: st.Key, StepID: st.StepID, Paths: v.Paths(), Risk: st.Risk, Findings: v.Sensitive}, s.now(), true)
	})
	return v, err
}

// visible returns the projects whose approvals p may see: every project for the Owner, the projects p
// administers otherwise (Members do not see approvals).
func (s *Service) visible(ctx context.Context, p auth.Principal) (map[string]ProjectRef, error) {
	ps, err := s.Projects.Visible(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ProjectRef, len(ps))
	for _, pr := range ps {
		if p.Owner || pr.Role == RoleProjectAdmin {
			out[pr.ID] = pr
		}
	}
	return out, nil
}

// ListQuery filters the Approval Center list.
type ListQuery struct {
	States []domain.State
	TaskID string // one task's approvals (KIN-25); empty = all
}

// ListItem is an approval with its project's name.
type ListItem struct {
	domain.Approval
	ProjectName string
}

// MaxList bounds one list response.
const MaxList = 200

// List returns the approvals p may see, newest first.
func (s *Service) List(ctx context.Context, p auth.Principal, q ListQuery) ([]ListItem, error) {
	vis, err := s.visible(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(vis) == 0 {
		return []ListItem{}, nil
	}
	f := Filter{AllProjects: p.Owner, States: q.States, TaskID: q.TaskID, Limit: MaxList}
	for id := range vis {
		f.ProjectIDs = append(f.ProjectIDs, id)
	}
	slices.Sort(f.ProjectIDs)
	as, err := s.Repo.List(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("listing approvals: %w", err)
	}
	out := make([]ListItem, 0, len(as))
	for _, a := range as {
		out = append(out, ListItem{Approval: a, ProjectName: vis[a.ProjectID].Name})
	}
	return out, nil
}

// Detail is one approval in the Approval Center: the task, the revision it binds to, the previous revision and
// what changed, and the task's other approvals.
type Detail struct {
	Approval    domain.Approval
	ProjectName string
	Task        TaskRef
	Revision    RevisionRef
	Previous    *RevisionRef
	Changes     []domain.Change
	Related     []domain.Approval
	CanDecide   bool
}

// Get returns an approval p may see (domain.ErrNotFound otherwise).
func (s *Service) Get(ctx context.Context, p auth.Principal, id string) (Detail, error) {
	vis, err := s.visible(ctx, p)
	if err != nil {
		return Detail{}, err
	}
	a, err := s.Repo.Get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	pr, ok := vis[a.ProjectID]
	if !ok {
		return Detail{}, domain.ErrNotFound
	}
	t, err := s.Workflow.Task(ctx, a.TaskID)
	if err != nil {
		return Detail{}, err
	}
	rev, err := s.Workflow.Revision(ctx, a.TaskID, a.Revision)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Approval: a, ProjectName: pr.Name, Task: t, Revision: rev,
		CanDecide: domain.CanDecide(p.Owner) && a.State == domain.StatePending}
	if a.Revision > 1 {
		prev, err := s.Workflow.Revision(ctx, a.TaskID, a.Revision-1)
		if err != nil {
			return Detail{}, err
		}
		d.Previous = &prev
		d.Changes = domain.DiffPlans(prev.Plan, rev.Plan)
	}
	if d.Related, err = s.Repo.ForTask(ctx, a.TaskID); err != nil {
		return Detail{}, err
	}
	return d, nil
}

// DecideInput is the Owner's decision on the revision and hash they reviewed.
type DecideInput struct {
	Approve  bool
	Revision int
	PlanHash string
	Comment  string
}

// Decide approves or rejects an approval (Owner only). The decision and its effect on the workflow commit
// together: execute approves/rejects the plan revision in the workflow (stale -> domain.ErrStaleRevision, nothing
// is approved); integrate_dev needs the task VERIFYING on that revision; scope_exceeded resumes the held step or,
// rejected, cancels the task.
func (s *Service) Decide(ctx context.Context, p auth.Principal, id string, in DecideInput) (domain.Approval, error) {
	if !domain.CanDecide(p.Owner) {
		return domain.Approval{}, domain.ErrForbidden
	}
	var a domain.Approval
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if a, err = s.Repo.Get(ctx, id); err != nil {
			return err
		}
		if err := a.Decide(domain.Decision{Approver: domain.Approver{ID: p.UserID, Login: p.Login}, Approve: in.Approve,
			Revision: in.Revision, PlanHash: in.PlanHash, Comment: in.Comment}, s.now()); err != nil {
			return err
		}
		if err := s.Repo.Save(ctx, a); err != nil {
			return err
		}
		if err := s.apply(ctx, a, in.Approve); err != nil {
			return err
		}
		kind := KindRejected
		if in.Approve {
			kind = KindApproved
		}
		return s.record(ctx, auditapp.UserActor(p), kind, a, map[string]string{"reason": string(a.Reason),
			"comment": a.Comment})
	})
	if err != nil {
		return domain.Approval{}, err
	}
	return a, nil
}

// apply carries a decision over to the workflow.
func (s *Service) apply(ctx context.Context, a domain.Approval, approve bool) error {
	switch {
	case a.Reason == domain.ReasonScopeExceeded:
		// The held step belongs to the running execution of this revision; anything else is stale.
		t, err := s.Workflow.Task(ctx, a.TaskID)
		if err != nil {
			return err
		}
		if t.PlanRevision != a.Revision {
			return domain.ErrStaleRevision
		}
		if t.State != TaskRunning {
			return domain.ErrNotReady
		}
		if approve {
			return s.Workflow.ResumeStep(ctx, a.StepID)
		}
		return s.Workflow.CancelTask(ctx, a.TaskID, "scope extension rejected: "+a.Comment)
	case a.Scope == domain.ScopeExecute:
		if approve {
			return s.Workflow.ApprovePlan(ctx, a.TaskID, a.Revision, a.PlanHash)
		}
		if err := s.Workflow.RejectPlan(ctx, a.TaskID, a.Revision, a.PlanHash); err != nil {
			return err
		}
		// The rejected revision will never run: its merge-into-dev request is moot.
		return s.supersede(ctx, a.TaskID, s.now(), func(o domain.Approval) bool {
			return o.Revision == a.Revision && o.Scope == domain.ScopeIntegrateDev
		})
	default: // integrate_dev: the merge gate (KIN-37) reads the decision; the task must be verified on this revision
		t, err := s.Workflow.Task(ctx, a.TaskID)
		if err != nil {
			return err
		}
		if t.PlanRevision != a.Revision {
			return domain.ErrStaleRevision
		}
		if approve && t.State != TaskVerifying {
			return domain.ErrNotReady
		}
		return nil
	}
}

// PolicyView is a project's risk policy; Custom is false while the default applies.
type PolicyView struct {
	Policy domain.Policy
	Custom bool
}

func (s *Service) project(ctx context.Context, p auth.Principal, projectID string) (ProjectRef, error) {
	ps, err := s.Projects.Visible(ctx, p)
	if err != nil {
		return ProjectRef{}, err
	}
	i := slices.IndexFunc(ps, func(pr ProjectRef) bool { return pr.ID == projectID })
	if i < 0 {
		return ProjectRef{}, domain.ErrNotFound
	}
	return ps[i], nil
}

// Policy returns the project's risk policy (Owner and the project's admins).
func (s *Service) Policy(ctx context.Context, p auth.Principal, projectID string) (PolicyView, error) {
	pr, err := s.project(ctx, p, projectID)
	if err != nil {
		return PolicyView{}, err
	}
	if !p.Owner && pr.Role != RoleProjectAdmin {
		return PolicyView{}, domain.ErrForbidden
	}
	pol, custom, err := s.policy(ctx, projectID)
	if err != nil {
		return PolicyView{}, err
	}
	return PolicyView{Policy: pol, Custom: custom}, nil
}

// SetPolicy replaces the project's risk policy (Owner only, KIN-A-21).
func (s *Service) SetPolicy(ctx context.Context, p auth.Principal, projectID string, rules []domain.Rule) (PolicyView, error) {
	if !p.Owner {
		return PolicyView{}, domain.ErrForbidden
	}
	if _, err := s.project(ctx, p, projectID); err != nil {
		return PolicyView{}, err
	}
	pol, err := domain.NewPolicy(rules)
	if err != nil {
		return PolicyView{}, err
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.SavePolicy(ctx, projectID, pol, p.UserID, s.now()); err != nil {
			return fmt.Errorf("saving risk policy: %w", err)
		}
		d := map[string]string{}
		for _, r := range pol.Rules {
			d[string(r.Category)] = fmt.Sprintf("%s:%d", r.Risk, len(r.Patterns))
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindPolicyUpdated,
			ProjectID: projectID, Subject: "project:" + projectID, Details: d})
	})
	if err != nil {
		return PolicyView{}, err
	}
	return PolicyView{Policy: pol, Custom: true}, nil
}
