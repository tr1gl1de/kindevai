package app

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// memRepo is an in-memory Repository with the same optimistic-concurrency contract as the SQLite one.
type memRepo struct {
	mu        sync.Mutex
	tasks     map[string]domain.Task
	revisions map[string]domain.PlanRevision // taskID/number
	execs     map[string]domain.Execution
}

func newMemRepo() *memRepo {
	return &memRepo{tasks: map[string]domain.Task{}, revisions: map[string]domain.PlanRevision{},
		execs: map[string]domain.Execution{}}
}

func revKey(taskID string, n int) string { return fmt.Sprintf("%s/%d", taskID, n) }

func (r *memRepo) CreateTask(_ context.Context, t *domain.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.tasks {
		if t.YouTrackIssue != "" && o.ProjectID == t.ProjectID && o.YouTrackIssue == t.YouTrackIssue {
			return ErrDuplicateIssue
		}
	}
	t.Version = 1
	r.tasks[t.ID] = *t
	return nil
}

func (r *memRepo) TaskByIssue(_ context.Context, projectID, issue string) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if t.ProjectID == projectID && t.YouTrackIssue == issue {
			return t, nil
		}
	}
	return domain.Task{}, domain.ErrNotFound
}

func (r *memRepo) Task(_ context.Context, id string) (domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return t, domain.ErrNotFound
	}
	return t, nil
}

func (r *memRepo) SaveTask(_ context.Context, t *domain.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.tasks[t.ID]
	if !ok || old.Version != t.Version {
		return domain.ErrConflict
	}
	t.Version++
	r.tasks[t.ID] = *t
	return nil
}

func (r *memRepo) CreateRevision(_ context.Context, rev domain.PlanRevision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := revKey(rev.TaskID(), rev.Number())
	if _, ok := r.revisions[k]; ok {
		return domain.ErrConflict
	}
	r.revisions[k] = rev
	return nil
}

func (r *memRepo) Revision(_ context.Context, taskID string, n int) (domain.PlanRevision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rev, ok := r.revisions[revKey(taskID, n)]
	if !ok {
		return rev, domain.ErrNotFound
	}
	return rev, nil
}

func (r *memRepo) CreateExecution(_ context.Context, e *domain.Execution) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.execs {
		if o.TaskID == e.TaskID && o.State == domain.ExecutionRunning {
			return domain.ErrConflict
		}
	}
	e.Version = 1
	r.execs[e.ID] = e.Clone()
	return nil
}

func (r *memRepo) Execution(_ context.Context, id string) (domain.Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.execs[id]
	if !ok {
		return e, domain.ErrNotFound
	}
	return e.Clone(), nil
}

func (r *memRepo) ExecutionByStep(_ context.Context, stepID string) (domain.Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.execs {
		if e.Step(stepID) != nil {
			return e.Clone(), nil
		}
	}
	return domain.Execution{}, domain.ErrNotFound
}

func (r *memRepo) RunningExecution(_ context.Context, taskID string) (domain.Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.execs {
		if e.TaskID == taskID && e.State == domain.ExecutionRunning {
			return e.Clone(), nil
		}
	}
	return domain.Execution{}, domain.ErrNotFound
}

func (r *memRepo) SaveExecution(_ context.Context, e *domain.Execution) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.execs[e.ID]
	if !ok || old.Version != e.Version {
		return domain.ErrConflict
	}
	e.Version++
	r.execs[e.ID] = e.Clone()
	return nil
}

func (r *memRepo) ExpiredLeaseExecutions(_ context.Context, now time.Time) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, e := range r.execs {
		for _, s := range e.Steps {
			if e.State == domain.ExecutionRunning && s.LeaseExpiresAt != nil && !now.Before(*s.LeaseExpiresAt) {
				ids = append(ids, e.ID)
				break
			}
		}
	}
	return ids, nil
}

// memTx serializes use cases like the single SQLite writer does. It does not roll back: tests that need atomicity
// run against SQLite (adapters/sqlite).
// A nested call joins the outer transaction, like db.Transactor.
type memTx struct{ mu sync.Mutex }

type memTxKey struct{}

func (t *memTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx.Value(memTxKey{}) != nil {
		return fn(ctx)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return fn(context.WithValue(ctx, memTxKey{}, true))
}

type fakeProjects map[string][]string

func (f fakeProjects) Repositories(_ context.Context, projectID string) ([]string, error) {
	r, ok := f[projectID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return r, nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (r *memRepo) RunningExecutions(_ context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, e := range r.execs {
		if e.State == domain.ExecutionRunning {
			ids = append(ids, e.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (r *memRepo) TasksInState(_ context.Context, s domain.TaskState) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, t := range r.tasks {
		if t.State == s {
			ids = append(ids, t.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (r *memRepo) ExecutionsHeldBy(_ context.Context, runnerID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, e := range r.execs {
		if e.State == domain.ExecutionRunning && slices.ContainsFunc(e.Steps, func(s domain.Step) bool { return s.RunnerID == runnerID }) {
			ids = append(ids, e.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (r *memRepo) HeldSteps(context.Context) (map[string]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, e := range r.execs {
		for i := range e.Steps {
			if e.State == domain.ExecutionRunning && e.Steps[i].Holding() {
				out[e.Steps[i].RunnerID]++
			}
		}
	}
	return out, nil
}

func (f TaskFilter) matches(t domain.Task) bool {
	return (len(f.ProjectIDs) > 0 || len(f.TaskIDs) > 0) &&
		(len(f.ProjectIDs) == 0 || slices.Contains(f.ProjectIDs, t.ProjectID)) &&
		(len(f.States) == 0 || slices.Contains(f.States, t.State)) &&
		(len(f.TaskIDs) == 0 || slices.Contains(f.TaskIDs, t.ID))
}

func (r *memRepo) ListTasks(_ context.Context, f TaskFilter, limit int) ([]domain.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Task
	for _, t := range r.tasks {
		if f.matches(t) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b domain.Task) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memRepo) EachAttempt(_ context.Context, f TaskFilter, fn func(string, domain.Attempt) error) error {
	r.mu.Lock()
	var all []struct {
		task string
		a    domain.Attempt
	}
	for _, e := range r.execs {
		if !f.matches(r.tasks[e.TaskID]) {
			continue
		}
		for _, s := range e.Clone().Steps {
			for _, a := range s.Attempts {
				all = append(all, struct {
					task string
					a    domain.Attempt
				}{e.TaskID, a})
			}
		}
	}
	r.mu.Unlock()
	for _, x := range all {
		if err := fn(x.task, x.a); err != nil {
			return err
		}
	}
	return nil
}

func (r *memRepo) ExecutionSpans(_ context.Context, taskIDs []string) (map[string]ExecutionSpan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]ExecutionSpan{}
	running := map[string]bool{}
	for _, e := range r.execs {
		if !slices.Contains(taskIDs, e.TaskID) {
			continue
		}
		sp, ok := out[e.TaskID]
		if !ok || e.CreatedAt.Before(sp.Start) {
			sp.Start = e.CreatedAt
		}
		if e.FinishedAt == nil {
			running[e.TaskID] = true
		} else if sp.End == nil || e.FinishedAt.After(*sp.End) {
			f := *e.FinishedAt
			sp.End = &f
		}
		out[e.TaskID] = sp
	}
	for id := range running {
		sp := out[id]
		sp.End = nil
		out[id] = sp
	}
	return out, nil
}

func (r *memRepo) LatestExecution(_ context.Context, taskID string) (domain.Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *domain.Execution
	for _, e := range r.execs {
		if e.TaskID == taskID && (best == nil || e.CreatedAt.After(best.CreatedAt) || e.CreatedAt.Equal(best.CreatedAt) && e.ID > best.ID) {
			c := e.Clone()
			best = &c
		}
	}
	if best == nil {
		return domain.Execution{}, domain.ErrNotFound
	}
	return *best, nil
}

// memPlanning is an in-memory PlanningRepository.
type memPlanning struct {
	mu   sync.Mutex
	runs map[string]domain.PlanningRun
	ord  []string // creation order
}

func newMemPlanning() *memPlanning { return &memPlanning{runs: map[string]domain.PlanningRun{}} }

func (m *memPlanning) CreatePlanningRun(_ context.Context, r *domain.PlanningRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.runs {
		if o.TaskID == r.TaskID && o.State.Active() {
			return domain.ErrConflict
		}
	}
	r.Version = 1
	m.runs[r.ID] = *r
	m.ord = append(m.ord, r.ID)
	return nil
}

func (m *memPlanning) PlanningRun(_ context.Context, id string) (domain.PlanningRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return r, domain.ErrNotFound
	}
	return r, nil
}

func (m *memPlanning) LatestPlanningRun(_ context.Context, taskID string) (domain.PlanningRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.ord) - 1; i >= 0; i-- {
		if r := m.runs[m.ord[i]]; r.TaskID == taskID {
			return r, nil
		}
	}
	return domain.PlanningRun{}, domain.ErrNotFound
}

func (m *memPlanning) LatestPlanRun(_ context.Context, taskID string) (domain.PlanningRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.ord) - 1; i >= 0; i-- {
		if r := m.runs[m.ord[i]]; r.TaskID == taskID && r.Kind != domain.PlanningKindDiscussion {
			return r, nil
		}
	}
	return domain.PlanningRun{}, domain.ErrNotFound
}

func (m *memPlanning) SavePlanningRun(_ context.Context, r *domain.PlanningRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.runs[r.ID]
	if !ok || old.Version != r.Version {
		return domain.ErrConflict
	}
	r.Version++
	m.runs[r.ID] = *r
	return nil
}

func (m *memPlanning) ActivePlanningRuns(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, id := range m.ord {
		if m.runs[id].State.Active() {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *memPlanning) PlanningRunsHeldBy(_ context.Context, runnerID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, id := range m.ord {
		if r := m.runs[id]; r.State == domain.PlanningRunning && r.RunnerID == runnerID {
			out = append(out, id)
		}
	}
	return out, nil
}
