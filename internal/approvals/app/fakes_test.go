package app

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type memRepo struct {
	mu        sync.Mutex
	policies  map[string]domain.Policy
	approvals map[string]domain.Approval
	order     []string
}

func newMemRepo() *memRepo {
	return &memRepo{policies: map[string]domain.Policy{}, approvals: map[string]domain.Approval{}}
}

func (r *memRepo) Policy(_ context.Context, projectID string) (domain.Policy, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.policies[projectID]
	return p, ok, nil
}

func (r *memRepo) SavePolicy(_ context.Context, projectID string, p domain.Policy, _ string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[projectID] = p
	return nil
}

func (r *memRepo) Create(_ context.Context, a domain.Approval) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.approvals {
		if o.State == domain.StatePending && a.State == domain.StatePending && o.TaskID == a.TaskID &&
			o.Scope == a.Scope && o.StepKey == a.StepKey {
			return ErrDuplicatePending
		}
	}
	r.approvals[a.ID] = a
	r.order = append(r.order, a.ID)
	return nil
}

func (r *memRepo) Get(_ context.Context, id string) (domain.Approval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.approvals[id]
	if !ok {
		return a, domain.ErrNotFound
	}
	return a, nil
}

func (r *memRepo) Save(_ context.Context, a domain.Approval) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.approvals[a.ID]
	if !ok || old.State != domain.StatePending {
		return ErrConflict
	}
	r.approvals[a.ID] = a
	return nil
}

func (r *memRepo) ForTask(_ context.Context, taskID string) ([]domain.Approval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Approval
	for _, id := range r.order {
		if a := r.approvals[id]; a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *memRepo) List(_ context.Context, f Filter) ([]domain.Approval, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Approval
	for i := len(r.order) - 1; i >= 0; i-- {
		a := r.approvals[r.order[i]]
		if (f.AllProjects || slices.Contains(f.ProjectIDs, a.ProjectID)) && (len(f.States) == 0 || slices.Contains(f.States, a.State)) {
			out = append(out, a)
		}
	}
	return out, nil
}

// memTx rolls the in-memory repository back when fn fails, like the SQLite transaction.
type memTx struct {
	mu   sync.Mutex
	repo *memRepo
}

func (t *memTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.repo.mu.Lock()
	approvals, order := maps.Clone(t.repo.approvals), slices.Clone(t.repo.order)
	t.repo.mu.Unlock()
	err := fn(ctx)
	if err != nil {
		t.repo.mu.Lock()
		t.repo.approvals, t.repo.order = approvals, order
		t.repo.mu.Unlock()
	}
	return err
}

// fakeWorkflow models the workflow context's task states as the approvals context needs them.
type fakeWorkflow struct {
	mu        sync.Mutex
	tasks     map[string]*TaskRef
	revisions map[string][]RevisionRef // by task, index = number-1
	steps     map[string]StepRef
	held      []string
	resumed   []string
	cancelled []string
}

func newFakeWorkflow() *fakeWorkflow {
	return &fakeWorkflow{tasks: map[string]*TaskRef{}, revisions: map[string][]RevisionRef{}, steps: map[string]StepRef{}}
}

func (w *fakeWorkflow) Task(_ context.Context, id string) (TaskRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.tasks[id]
	if !ok {
		return TaskRef{}, domain.ErrNotFound
	}
	return *t, nil
}

func (w *fakeWorkflow) Revision(_ context.Context, taskID string, n int) (RevisionRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	revs := w.revisions[taskID]
	if n < 1 || n > len(revs) {
		return RevisionRef{}, domain.ErrNotFound
	}
	return revs[n-1], nil
}

func (w *fakeWorkflow) decide(taskID string, n int, hash, to string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	t := w.tasks[taskID]
	if t.PlanRevision != n || w.revisions[taskID][n-1].Hash != hash {
		return domain.ErrStaleRevision
	}
	if t.State != TaskAwaitingApproval {
		return domain.ErrNotReady
	}
	t.State = to
	return nil
}

func (w *fakeWorkflow) ApprovePlan(_ context.Context, taskID string, n int, hash string) error {
	return w.decide(taskID, n, hash, "READY")
}

func (w *fakeWorkflow) RejectPlan(_ context.Context, taskID string, n int, hash string) error {
	return w.decide(taskID, n, hash, "ANALYZING")
}

func (w *fakeWorkflow) Step(_ context.Context, stepID string) (StepRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.steps[stepID]
	if !ok {
		return StepRef{}, domain.ErrNotFound
	}
	return s, nil
}

func (w *fakeWorkflow) HoldStep(_ context.Context, stepID, _ string, _ int64, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held = append(w.held, stepID)
	return nil
}

func (w *fakeWorkflow) ResumeStep(_ context.Context, stepID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.resumed = append(w.resumed, stepID)
	return nil
}

func (w *fakeWorkflow) CancelTask(_ context.Context, taskID, _ string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = append(w.cancelled, taskID)
	w.tasks[taskID].State = "CANCELLED"
	return nil
}

type fakeProjects map[string]map[string]string // project -> user -> role

func (f fakeProjects) Visible(_ context.Context, p auth.Principal) ([]ProjectRef, error) {
	var out []ProjectRef
	for id, members := range f {
		switch {
		case p.Owner:
			out = append(out, ProjectRef{ID: id, Name: "Project " + id, Role: RoleOwner})
		case members[p.UserID] != "":
			out = append(out, ProjectRef{ID: id, Name: "Project " + id, Role: members[p.UserID]})
		}
	}
	slices.SortFunc(out, func(a, b ProjectRef) int { return compare(a.ID, b.ID) })
	return out, nil
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (f *fakeAudit) Record(_ context.Context, e auditapp.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.entries {
		out = append(out, e.Kind)
	}
	return out
}

type fakeOutbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (f *fakeOutbox) Enqueue(_ context.Context, m messaging.Message) error {
	if !json.Valid(m.Payload) {
		return errors.New("invalid payload")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return nil
}
