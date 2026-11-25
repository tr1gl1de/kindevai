package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

var (
	owner  = auth.Principal{UserID: "u-owner", Login: "owner", Owner: true}
	admin  = auth.Principal{UserID: "u-admin", Login: "alice"}
	member = auth.Principal{UserID: "u-member", Login: "bob"}
)

type env struct {
	svc    *Service
	repo   *memRepo
	wf     *fakeWorkflow
	audit  *fakeAudit
	outbox *fakeOutbox
}

func newEnv() env {
	var n atomic.Int64
	e := env{repo: newMemRepo(), wf: newFakeWorkflow(), audit: &fakeAudit{}, outbox: &fakeOutbox{}}
	e.svc = &Service{Repo: e.repo, Workflow: e.wf, Tx: &memTx{repo: e.repo}, Audit: e.audit, Outbox: e.outbox,
		Projects: fakeProjects{
			"p1": {"u-admin": RoleProjectAdmin, "u-member": RoleMember},
			"p2": {"u-member": RoleProjectAdmin},
		},
		Now: func() time.Time { return t0 }, NewID: func() string { return fmt.Sprintf("a%d", n.Add(1)) }}
	return e
}

func hashOf(n int) string { return strings.Repeat(fmt.Sprint(n%10), 64) }

func snapshot(risk domain.Risk, scope ...string) domain.PlanSnapshot {
	return domain.PlanSnapshot{Summary: "plan", Risk: risk, MaxParallel: 1, Steps: []domain.StepSnapshot{
		{Key: "backend", Title: "API", Repository: "api", Scope: scope}}}
}

// submit records revision n of task t1 in project p1 at the effective risk computed by the service, the way the
// workflow context does: EffectiveRisk, then PlanAccepted in the same transaction.
func (e env) submit(t *testing.T, n int, plan domain.PlanSnapshot) domain.Risk {
	t.Helper()
	ctx := t.Context()
	risk, err := e.svc.EffectiveRisk(ctx, "p1", plan)
	if err != nil {
		t.Fatal(err)
	}
	state := "READY"
	if risk.RequiresApproval() {
		state = TaskAwaitingApproval
	}
	e.wf.mu.Lock()
	e.wf.tasks["t1"] = &TaskRef{ID: "t1", ProjectID: "p1", Title: "Login", State: state, PlanRevision: n}
	e.wf.revisions["t1"] = append(e.wf.revisions["t1"], RevisionRef{Number: n, Hash: hashOf(n), Risk: risk, Plan: plan})
	e.wf.mu.Unlock()
	if err := e.svc.PlanAccepted(ctx, PlanEvent{TaskID: "t1", ProjectID: "p1", TaskTitle: "Login", Revision: n,
		PlanHash: hashOf(n), Risk: risk, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	return risk
}

func (e env) pending(t *testing.T) []domain.Approval {
	t.Helper()
	all, _ := e.repo.ForTask(t.Context(), "t1")
	return slices.DeleteFunc(all, func(a domain.Approval) bool { return a.State != domain.StatePending })
}

func find(t *testing.T, as []domain.Approval, scope domain.Scope) domain.Approval {
	t.Helper()
	for _, a := range as {
		if a.Scope == scope {
			return a
		}
	}
	t.Fatalf("no %s approval in %+v", scope, as)
	return domain.Approval{}
}

// AC: the Policy Evaluator only raises the AI's risk; rules are configured per project.
func TestEffectiveRiskUsesTheProjectPolicyAndOnlyRaises(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if r, _ := e.svc.EffectiveRisk(ctx, "p1", snapshot(domain.RiskL0, "db/migrations")); r != domain.RiskL2 {
		t.Fatalf("default policy: migrations = %s, want L2", r)
	}
	if r, _ := e.svc.EffectiveRisk(ctx, "p1", snapshot(domain.RiskL3, "docs")); r != domain.RiskL3 {
		t.Fatalf("L3 proposal lowered to %s", r)
	}
	if _, err := e.svc.SetPolicy(ctx, owner, "p1", []domain.Rule{
		{Category: domain.CategoryPaths, Risk: domain.RiskL3, Patterns: []string{"billing/**"}},
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.svc.EffectiveRisk(ctx, "p1", snapshot(domain.RiskL1, "billing/invoice.go")); r != domain.RiskL3 {
		t.Fatalf("project rule: billing = %s, want L3", r)
	}
	if r, _ := e.svc.EffectiveRisk(ctx, "p1", snapshot(domain.RiskL1, "db/migrations")); r != domain.RiskL1 {
		t.Fatalf("project policy replaced the default: migrations = %s, want L1", r)
	}
}

func TestOnlyTheOwnerChangesThePolicy(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	rules := []domain.Rule{{Category: domain.CategoryPaths, Risk: domain.RiskL3, Patterns: []string{"x/**"}}}
	for _, p := range []auth.Principal{admin, member} {
		if _, err := e.svc.SetPolicy(ctx, p, "p1", rules); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("%s set policy = %v", p.Login, err)
		}
	}
	v, err := e.svc.Policy(ctx, admin, "p1")
	if err != nil || v.Custom || len(v.Policy.Rules) != len(domain.AllCategories) {
		t.Fatalf("admin reads the default policy = %+v, %v", v, err)
	}
	if _, err := e.svc.Policy(ctx, member, "p1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member reads policy = %v", err)
	}
	if _, err := e.svc.Policy(ctx, admin, "p9"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project = %v", err)
	}
	if _, err := e.svc.SetPolicy(ctx, owner, "p1", rules); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.audit.kinds(), KindPolicyUpdated) {
		t.Fatalf("audit = %v", e.audit.kinds())
	}
}

// AC: L2/L3 need an execute approval and a separate integrate_dev request; L0/L1 get integrate_dev from the policy.
func TestPlanAcceptedRequestsApprovalsByRisk(t *testing.T) {
	t.Parallel()
	t.Run("L2", func(t *testing.T) {
		t.Parallel()
		e := newEnv()
		e.submit(t, 1, snapshot(domain.RiskL0, "db/migrations"))
		p := e.pending(t)
		if len(p) != 2 {
			t.Fatalf("pending = %+v", p)
		}
		ex := find(t, p, domain.ScopeExecute)
		if ex.Risk != domain.RiskL2 || ex.ProposedRisk != domain.RiskL0 || ex.Revision != 1 || ex.PlanHash != hashOf(1) ||
			len(ex.Findings) == 0 || ex.TaskTitle != "Login" {
			t.Fatalf("execute approval = %+v", ex)
		}
		find(t, p, domain.ScopeIntegrateDev)
		if len(e.outbox.msgs) != 1 || e.outbox.msgs[0].Topic != TopicApprovalRequested ||
			!strings.Contains(string(e.outbox.msgs[0].Payload), ex.ID) {
			t.Fatalf("outbox = %+v", e.outbox.msgs)
		}
		if ok, _ := e.svc.IntegrateDevApproved(t.Context(), "t1"); ok {
			t.Fatal("L2 integrate_dev must wait for the Owner")
		}
	})
	t.Run("L1", func(t *testing.T) {
		t.Parallel()
		e := newEnv()
		e.submit(t, 1, snapshot(domain.RiskL1, "docs"))
		if p := e.pending(t); len(p) != 0 {
			t.Fatalf("L1 pending = %+v", p)
		}
		all, _ := e.repo.ForTask(t.Context(), "t1")
		g := find(t, all, domain.ScopeIntegrateDev)
		if g.State != domain.StateApproved || g.Approver != domain.PolicyApprover {
			t.Fatalf("policy grant = %+v", g)
		}
		if ok, _ := e.svc.IntegrateDevApproved(t.Context(), "t1"); !ok {
			t.Fatal("L1 integrates into dev by policy")
		}
		if len(e.outbox.msgs) != 0 {
			t.Fatalf("no notification for L1: %+v", e.outbox.msgs)
		}
	})
}

// AC: a new plan revision supersedes the pending approvals of older ones.
func TestNewRevisionSupersedesPendingApprovals(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	old := find(t, e.pending(t), domain.ScopeExecute)
	e.submit(t, 2, snapshot(domain.RiskL2, "docs", "web"))
	p := e.pending(t)
	if len(p) != 2 || find(t, p, domain.ScopeExecute).Revision != 2 {
		t.Fatalf("pending = %+v", p)
	}
	got, _ := e.repo.Get(t.Context(), old.ID)
	if got.State != domain.StateSuperseded {
		t.Fatalf("old approval = %s", got.State)
	}
	_, err := e.svc.Decide(t.Context(), owner, old.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)})
	if !errors.Is(err, domain.ErrStaleRevision) {
		t.Fatalf("approving the superseded revision = %v", err)
	}
}

// AC: only the Owner decides; Member and Project Admin get 403 and nothing changes.
func TestOnlyTheOwnerDecides(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL3, "docs"))
	ex := find(t, e.pending(t), domain.ScopeExecute)
	for _, p := range []auth.Principal{admin, member} {
		_, err := e.svc.Decide(t.Context(), p, ex.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("%s decide = %v", p.Login, err)
		}
	}
	if got, _ := e.repo.Get(t.Context(), ex.ID); got.State != domain.StatePending {
		t.Fatalf("state = %s", got.State)
	}
	a, err := e.svc.Decide(t.Context(), owner, ex.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1), Comment: "ok"})
	if err != nil || a.State != domain.StateApproved || a.Approver != "owner" || a.ApproverID != owner.UserID {
		t.Fatalf("owner approve = %+v, %v", a, err)
	}
	if task, _ := e.wf.Task(t.Context(), "t1"); task.State != "READY" {
		t.Fatalf("task = %s, want READY", task.State)
	}
	if !slices.Contains(e.audit.kinds(), KindApproved) {
		t.Fatalf("audit = %v", e.audit.kinds())
	}
	// execute ≠ integrate_dev
	if ok, _ := e.svc.IntegrateDevApproved(t.Context(), "t1"); ok {
		t.Fatal("approving execute must not approve integrate_dev")
	}
}

// AC: approve/reject take planRevision + planHash; a revision that is no longer current is 409 and nothing is
// approved.
func TestDecidingAStaleRevisionApprovesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	ex := find(t, e.pending(t), domain.ScopeExecute)
	for _, in := range []DecideInput{
		{Approve: true, Revision: 2, PlanHash: hashOf(1)},
		{Approve: true, Revision: 1, PlanHash: hashOf(2)},
		{Approve: false, Revision: 0, PlanHash: "", Comment: "no"},
	} {
		if _, err := e.svc.Decide(t.Context(), owner, ex.ID, in); !errors.Is(err, domain.ErrStaleRevision) {
			t.Fatalf("decide %+v = %v", in, err)
		}
	}
	// The workflow moved on (e.g. the plan chat created revision 2) but the event has not arrived yet: the
	// workflow's own check refuses and the approval stays pending.
	e.wf.mu.Lock()
	e.wf.tasks["t1"].PlanRevision = 2
	e.wf.revisions["t1"] = append(e.wf.revisions["t1"], RevisionRef{Number: 2, Hash: hashOf(2), Risk: domain.RiskL2})
	e.wf.mu.Unlock()
	if _, err := e.svc.Decide(t.Context(), owner, ex.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)}); !errors.Is(err, domain.ErrStaleRevision) {
		t.Fatalf("decide behind the workflow = %v", err)
	}
	all, _ := e.repo.ForTask(t.Context(), "t1")
	for _, a := range all {
		if a.State == domain.StateApproved {
			t.Fatalf("an approval was created: %+v", a)
		}
	}
}

func TestRejectNeedsACommentAndReturnsTheTaskToAnalysis(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	ex := find(t, e.pending(t), domain.ScopeExecute)
	var ve *domain.ValidationError
	if _, err := e.svc.Decide(t.Context(), owner, ex.ID, DecideInput{Revision: 1, PlanHash: hashOf(1)}); !errors.As(err, &ve) {
		t.Fatalf("reject without comment = %v", err)
	}
	a, err := e.svc.Decide(t.Context(), owner, ex.ID, DecideInput{Revision: 1, PlanHash: hashOf(1), Comment: "split it"})
	if err != nil || a.State != domain.StateRejected || a.Comment != "split it" {
		t.Fatalf("reject = %+v, %v", a, err)
	}
	if task, _ := e.wf.Task(t.Context(), "t1"); task.State != "ANALYZING" {
		t.Fatalf("task = %s", task.State)
	}
}

// AC: integrate_dev for L2/L3 is a separate Owner decision, possible once the task is verified.
func TestIntegrateDevIsDecidedAfterVerification(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	in := find(t, e.pending(t), domain.ScopeIntegrateDev)
	dec := DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)}
	if _, err := e.svc.Decide(t.Context(), owner, in.ID, dec); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("integrate_dev before verification = %v", err)
	}
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = TaskVerifying
	e.wf.mu.Unlock()
	if _, err := e.svc.Decide(t.Context(), owner, in.ID, dec); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.IntegrateDevApproved(t.Context(), "t1"); !ok {
		t.Fatal("integrate_dev approved")
	}
}

func TestClosedTaskSupersedesPendingApprovals(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	if err := e.svc.TaskClosed(t.Context(), "t1"); err != nil {
		t.Fatal(err)
	}
	if p := e.pending(t); len(p) != 0 {
		t.Fatalf("pending after close = %+v", p)
	}
}

// AC: a step changing files outside its approved scope is stopped and needs a new approval.
func TestChangesBeyondTheScopeStopTheStepUntilApproved(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	e.submit(t, 1, snapshot(domain.RiskL1, "internal/api"))
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = "RUNNING"
	e.wf.steps["s1"] = StepRef{StepID: "s1", TaskID: "t1", Key: "backend", Repository: "api", Revision: 1,
		PlanHash: hashOf(1), Risk: domain.RiskL1, Scope: []string{"internal/api"}}
	e.wf.mu.Unlock()

	ch := StepChanges{StepID: "s1", RunnerID: "r1", Fence: 3, Changed: []string{"internal/api/h.go"}}
	if v, err := e.svc.CheckChanges(ctx, ch); err != nil || !v.Empty() || len(e.wf.held) != 0 {
		t.Fatalf("in-scope changes = %+v, %v", v, err)
	}
	ch.Changed = []string{"internal/api/h.go", "web/app.ts", "internal/api/auth/token.go"}
	v, err := e.svc.CheckChanges(ctx, ch)
	if err != nil || v.Empty() || !slices.Equal(e.wf.held, []string{"s1"}) {
		t.Fatalf("violation = %+v, %v, held %v", v, err, e.wf.held)
	}
	req := find(t, e.pending(t), domain.ScopeExecute)
	if req.Reason != domain.ReasonScopeExceeded || req.StepKey != "backend" ||
		!slices.Equal(req.Paths, []string{"internal/api/auth/token.go", "web/app.ts"}) {
		t.Fatalf("scope request = %+v", req)
	}
	if len(e.outbox.msgs) != 1 {
		t.Fatalf("notification = %+v", e.outbox.msgs)
	}
	// KIN-51: the audit entry carries the diff summary (the files beyond the approval).
	if a := e.audit.entries[len(e.audit.entries)-1]; a.Kind != KindRequested || a.Details["reason"] != "scope_exceeded" ||
		a.Details["paths"] != "internal/api/auth/token.go, web/app.ts" {
		t.Fatalf("audit = %+v", a)
	}
	if _, err := e.svc.Decide(ctx, owner, req.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.wf.resumed, []string{"s1"}) {
		t.Fatalf("resumed = %v", e.wf.resumed)
	}
	// The approved files are now part of the step's scope.
	if v, err := e.svc.CheckChanges(ctx, ch); err != nil || !v.Empty() {
		t.Fatalf("after approval = %+v, %v", v, err)
	}
}

func TestRejectingAScopeExtensionCancelsTheTask(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	e.submit(t, 1, snapshot(domain.RiskL1, "internal/api"))
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = "RUNNING"
	e.wf.steps["s1"] = StepRef{StepID: "s1", TaskID: "t1", Key: "backend", Repository: "api", Revision: 1,
		PlanHash: hashOf(1), Risk: domain.RiskL1, Scope: []string{"internal/api"}}
	e.wf.mu.Unlock()
	if _, err := e.svc.CheckChanges(ctx, StepChanges{StepID: "s1", RunnerID: "r1", Fence: 1, Changed: []string{"Makefile"}}); err != nil {
		t.Fatal(err)
	}
	req := find(t, e.pending(t), domain.ScopeExecute)
	if _, err := e.svc.Decide(ctx, owner, req.ID, DecideInput{Revision: 1, PlanHash: hashOf(1), Comment: "no"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.wf.cancelled, []string{"t1"}) {
		t.Fatalf("cancelled = %v", e.wf.cancelled)
	}
}

func TestListAndDetailsFollowProjectRoles(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	e.submit(t, 2, snapshot(domain.RiskL3, "docs", "internal/auth"))

	all, err := e.svc.List(ctx, owner, ListQuery{})
	if err != nil || len(all) != 4 || all[0].ProjectName != "Project p1" {
		t.Fatalf("owner list = %+v, %v", all, err)
	}
	pend, _ := e.svc.List(ctx, owner, ListQuery{States: []domain.State{domain.StatePending}})
	if len(pend) != 2 {
		t.Fatalf("pending list = %d", len(pend))
	}
	if got, _ := e.svc.List(ctx, admin, ListQuery{}); len(got) != 4 {
		t.Fatalf("project admin of p1 sees %d", len(got))
	}
	if got, _ := e.svc.List(ctx, member, ListQuery{}); len(got) != 0 {
		t.Fatalf("a member of p1 (admin of p2) sees %d", len(got))
	}

	ex := find(t, e.pending(t), domain.ScopeExecute)
	d, err := e.svc.Get(ctx, admin, ex.ID)
	if err != nil || d.CanDecide || d.Previous == nil || d.Revision.Number != 2 || len(d.Changes) == 0 || len(d.Related) != 4 {
		t.Fatalf("admin detail = %+v, %v", d, err)
	}
	if d, _ := e.svc.Get(ctx, owner, ex.ID); !d.CanDecide {
		t.Fatal("the Owner can decide a pending approval")
	}
	if _, err := e.svc.Get(ctx, member, ex.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("member detail = %v", err)
	}
}

// Review KIN-24: rejecting a revision makes its merge-into-dev request moot.
func TestRejectingExecuteSupersedesItsMergeRequest(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL2, "docs"))
	ex := find(t, e.pending(t), domain.ScopeExecute)
	if _, err := e.svc.Decide(t.Context(), owner, ex.ID, DecideInput{Revision: 1, PlanHash: hashOf(1), Comment: "no"}); err != nil {
		t.Fatal(err)
	}
	if p := e.pending(t); len(p) != 0 {
		t.Fatalf("pending after reject = %+v", p)
	}
}

func (e env) heldStep(t *testing.T) domain.Approval {
	t.Helper()
	e.submit(t, 1, snapshot(domain.RiskL1, "internal/api"))
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = TaskRunning
	e.wf.steps["s1"] = StepRef{StepID: "s1", TaskID: "t1", Key: "backend", Repository: "api", Revision: 1,
		PlanHash: hashOf(1), Risk: domain.RiskL1, Scope: []string{"internal/api"}}
	e.wf.mu.Unlock()
	if _, err := e.svc.CheckChanges(t.Context(), StepChanges{StepID: "s1", RunnerID: "r1", Fence: 1,
		Changed: []string{"../outside", "web/a.ts"}}); err != nil {
		t.Fatal(err)
	}
	return find(t, e.pending(t), domain.ScopeExecute)
}

// Review KIN-24: a scope approval applies only while the task runs that revision.
func TestScopeApprovalNeedsTheRunningTask(t *testing.T) {
	t.Parallel()
	e := newEnv()
	req := e.heldStep(t)
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = "BLOCKED"
	e.wf.mu.Unlock()
	if _, err := e.svc.Decide(t.Context(), owner, req.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("approve while not running = %v", err)
	}
	if len(e.wf.resumed) != 0 {
		t.Fatalf("resumed = %v", e.wf.resumed)
	}
}

// Review KIN-24: invalid paths are stored quoted and an approval never makes them acceptable.
func TestInvalidChangedPathsNeverBecomeInScope(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	req := e.heldStep(t)
	if !slices.Equal(req.Paths, []string{`"../outside"`, "web/a.ts"}) {
		t.Fatalf("paths = %q", req.Paths)
	}
	if _, err := e.svc.Decide(ctx, owner, req.ID, DecideInput{Approve: true, Revision: 1, PlanHash: hashOf(1)}); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.CheckChanges(ctx, StepChanges{StepID: "s1", RunnerID: "r1", Fence: 2, Changed: []string{"../outside", "web/a.ts"}})
	if err != nil || !slices.Equal(v.Outside, []string{`"../outside"`}) {
		t.Fatalf("after approval = %+v, %v", v, err)
	}
}

func TestOverlongReportsStopTheStep(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL1, "internal/api"))
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = TaskRunning
	e.wf.steps["s1"] = StepRef{StepID: "s1", TaskID: "t1", Key: "backend", Repository: "api", Revision: 1,
		PlanHash: hashOf(1), Risk: domain.RiskL1, Scope: []string{"internal/api"}}
	e.wf.mu.Unlock()
	files := make([]string, MaxChangedFiles+3)
	for i := range files {
		files[i] = fmt.Sprintf("internal/api/f%d.go", i)
	}
	v, err := e.svc.CheckChanges(t.Context(), StepChanges{StepID: "s1", RunnerID: "r1", Fence: 1, Changed: files})
	if err != nil || v.Empty() || len(e.wf.held) != 1 {
		t.Fatalf("overlong report = %+v, %v", v.Outside, err)
	}
}

// KIN-51: a report that leaves files out (truncated by the runner, or unreadable) stops the step even when every
// listed file is in scope: the unlisted files were never checked.
func TestUnreportedFilesStopTheStep(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL1, "internal/api"))
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = TaskRunning
	e.wf.steps["s1"] = StepRef{StepID: "s1", TaskID: "t1", Key: "backend", Repository: "api", Revision: 1,
		PlanHash: hashOf(1), Risk: domain.RiskL1, Scope: []string{"internal/api"}}
	e.wf.mu.Unlock()
	v, err := e.svc.CheckChanges(t.Context(), StepChanges{StepID: "s1", RunnerID: "r1", Fence: 1,
		Changed: []string{"internal/api/a.go"}, Unreported: 7})
	if err != nil || !slices.Equal(v.Outside, []string{"(+7 unreported)"}) || len(e.wf.held) != 1 {
		t.Fatalf("truncated report = %+v, %v (held %v)", v.Outside, err, e.wf.held)
	}
}

// KIN-78: an attempt that worked in a worktree and completed without a final changes report stops the step like a
// truncated report: a scope_exceeded request names the missing report, and the attempt's reason says why.
func TestMissingChangesReportStopsTheStep(t *testing.T) {
	t.Parallel()
	e := newEnv()
	e.submit(t, 1, snapshot(domain.RiskL1, "internal/api"))
	e.wf.mu.Lock()
	e.wf.tasks["t1"].State = TaskRunning
	e.wf.steps["s1"] = StepRef{StepID: "s1", TaskID: "t1", Key: "backend", Repository: "api", Revision: 1,
		PlanHash: hashOf(1), Risk: domain.RiskL1, Scope: []string{"internal/api"}}
	e.wf.mu.Unlock()
	v, err := e.svc.CheckChanges(t.Context(), StepChanges{StepID: "s1", RunnerID: "r1", Fence: 1, Missing: true})
	if err != nil || !slices.Equal(v.Outside, []string{MissingChangesReport}) || len(e.wf.held) != 1 ||
		e.wf.holdMsgs[0] != "no changes report from the attempt" {
		t.Fatalf("missing report = %+v, %v (held %v %q)", v.Outside, err, e.wf.held, e.wf.holdMsgs)
	}
	all, _ := e.repo.ForTask(t.Context(), "t1")
	var paths []string
	for _, a := range all {
		if a.Reason == domain.ReasonScopeExceeded {
			paths = a.Paths
		}
	}
	if !slices.Equal(paths, []string{MissingChangesReport}) {
		t.Fatalf("scope_exceeded request paths %v", paths)
	}
}

// KIN-97: any revision of a task with its diff, for everyone who sees the task (Members included, unlike approvals);
// another project's task is not found.
func TestRevisionDiffFollowsTaskAccess(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	e.submit(t, 1, snapshot(domain.RiskL0, "docs"))
	e.submit(t, 2, snapshot(domain.RiskL0, "docs", "web"))
	e.submit(t, 3, snapshot(domain.RiskL1, "web"))

	for _, p := range []auth.Principal{owner, admin, member} {
		d, err := e.svc.RevisionDiff(ctx, p, "t1", 3, 0)
		if err != nil || d.Task.ID != "t1" || d.Revision.Number != 3 || d.Previous == nil || d.Previous.Number != 2 {
			t.Fatalf("%s: diff = %+v, %v", p.Login, d, err)
		}
		if len(d.Changes) != 2 { // risk L0 -> L1, scope -docs
			t.Fatalf("%s: changes = %+v", p.Login, d.Changes)
		}
	}
	// Any two revisions: 3 against 1.
	d, err := e.svc.RevisionDiff(ctx, owner, "t1", 3, 1)
	if err != nil || d.Previous.Number != 1 {
		t.Fatalf("diff against 1 = %+v, %v", d, err)
	}
	// The first revision has nothing to compare with.
	d, err = e.svc.RevisionDiff(ctx, member, "t1", 1, 0)
	if err != nil || d.Previous != nil || len(d.Changes) != 0 || d.Revision.Number != 1 {
		t.Fatalf("first revision = %+v, %v", d, err)
	}

	stranger := auth.Principal{UserID: "u-x", Login: "x"}
	for name, call := range map[string]func() error{
		"stranger":     func() error { _, err := e.svc.RevisionDiff(ctx, stranger, "t1", 2, 0); return err },
		"unknown task": func() error { _, err := e.svc.RevisionDiff(ctx, owner, "nope", 1, 0); return err },
		"revision 0":   func() error { _, err := e.svc.RevisionDiff(ctx, owner, "t1", 0, 0); return err },
		"revision 4":   func() error { _, err := e.svc.RevisionDiff(ctx, owner, "t1", 4, 0); return err },
		"base 9":       func() error { _, err := e.svc.RevisionDiff(ctx, owner, "t1", 2, 9); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s: err = %v, want not found", name, err)
		}
	}
	var ve *domain.ValidationError
	if _, err := e.svc.RevisionDiff(ctx, owner, "t1", 2, 2); !errors.As(err, &ve) || ve.Field != "base" {
		t.Fatalf("base = revision: err = %v", err)
	}
}
