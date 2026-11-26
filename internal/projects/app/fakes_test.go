package app

import (
	"context"
	"sort"
	"sync"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

type memRepo struct {
	mu       sync.Mutex
	projects map[string]domain.Project
	members  map[[2]string]domain.Membership
}

func newMemRepo() *memRepo {
	return &memRepo{projects: map[string]domain.Project{}, members: map[[2]string]domain.Membership{}}
}

func (r *memRepo) Create(_ context.Context, p domain.Project) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.projects {
		if q.Slug == p.Slug {
			return domain.ErrSlugTaken
		}
	}
	r.projects[p.ID] = p
	return nil
}

func (r *memRepo) ByID(_ context.Context, id string) (domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.projects[id]
	if !ok {
		return domain.Project{}, domain.ErrNotFound
	}
	return p, nil
}

func (r *memRepo) sorted(keep func(domain.Project) bool) []domain.Project {
	var out []domain.Project
	for _, p := range r.projects {
		if keep(p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *memRepo) All(context.Context) ([]domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sorted(func(domain.Project) bool { return true }), nil
}

func (r *memRepo) ForUser(_ context.Context, uid string) ([]domain.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sorted(func(p domain.Project) bool { _, ok := r.members[[2]string{p.ID, uid}]; return ok }), nil
}

func (r *memRepo) Membership(_ context.Context, pid, uid string) (domain.Membership, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.members[[2]string{pid, uid}]
	return m, ok, nil
}

func (r *memRepo) Members(_ context.Context, pid string) ([]domain.Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Membership
	for k, m := range r.members {
		if k[0] == pid {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

func (r *memRepo) SetMember(_ context.Context, m domain.Membership) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members[[2]string{m.ProjectID, m.UserID}] = m
	return nil
}

func (r *memRepo) RemoveMember(_ context.Context, pid, uid string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.members[[2]string{pid, uid}]
	delete(r.members, [2]string{pid, uid})
	return ok, nil
}

type memUsers map[string]UserRef // by login

func (u memUsers) ByLogin(_ context.Context, login string) (UserRef, error) {
	r, ok := u[login]
	if !ok {
		return UserRef{}, domain.ErrUserNotFound
	}
	return r, nil
}

func (u memUsers) ByIDs(_ context.Context, ids []string) (map[string]UserRef, error) {
	out := map[string]UserRef{}
	for _, r := range u {
		for _, id := range ids {
			if r.ID == id {
				out[id] = r
			}
		}
	}
	return out, nil
}

// directTx runs fn without a transaction: the in-memory fakes have none (rollback is covered on SQLite).
type directTx struct{}

func (directTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

type memAudit struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (m *memAudit) Record(_ context.Context, e auditapp.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}
