package app

import (
	"bytes"
	"context"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

type memRepo struct {
	mu      sync.Mutex
	tokens  map[string]domain.EnrollmentToken
	runners map[string]domain.Runner
}

func newMemRepo() *memRepo {
	return &memRepo{tokens: map[string]domain.EnrollmentToken{}, runners: map[string]domain.Runner{}}
}

func (r *memRepo) CreateToken(_ context.Context, t domain.EnrollmentToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[t.ID] = t
	return nil
}

func (r *memRepo) ListTokens(_ context.Context, _ int, _ time.Time) ([]domain.EnrollmentToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.EnrollmentToken
	for _, t := range r.tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (r *memRepo) TokenByID(_ context.Context, id string) (domain.EnrollmentToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[id]
	if !ok {
		return t, domain.ErrNotFound
	}
	return t, nil
}

func (r *memRepo) TokenByHash(_ context.Context, h []byte) (domain.EnrollmentToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tokens {
		if bytes.Equal(t.TokenHash, h) {
			return t, nil
		}
	}
	return domain.EnrollmentToken{}, domain.ErrNotFound
}

func (r *memRepo) ConsumeToken(_ context.Context, id, runnerID string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tokens[id]
	if t.State(now) != domain.TokenPending {
		return false, nil
	}
	t.ConsumedAt, t.RunnerID = &now, runnerID
	r.tokens[id] = t
	return true, nil
}

func (r *memRepo) RevokeToken(_ context.Context, id string, now time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tokens[id]
	if t.State(now) != domain.TokenPending {
		return false, nil
	}
	t.RevokedAt = &now
	r.tokens[id] = t
	return true, nil
}

func (r *memRepo) CreateRunner(_ context.Context, run domain.Runner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.runners {
		if bytes.Equal(x.PublicKey, run.PublicKey) {
			return domain.ErrKeyEnrolled
		}
	}
	r.runners[run.ID] = run
	return nil
}

func (r *memRepo) Runner(_ context.Context, id string) (domain.Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runners[id]
	if !ok {
		return run, domain.ErrNotFound
	}
	run.ProjectIDs = slices.Clone(run.ProjectIDs)
	return run, nil
}

func (r *memRepo) Runners(context.Context) ([]domain.Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Runner
	for _, run := range r.runners {
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *memRepo) SaveRunner(_ context.Context, run domain.Runner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runners[run.ID]; !ok {
		return domain.ErrNotFound
	}
	r.runners[run.ID] = run
	return nil
}

type fakeTx struct{}

func (fakeTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type memAudit struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (a *memAudit) Record(_ context.Context, e auditapp.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *memAudit) last() auditapp.Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entries[len(a.entries)-1]
}

type fakeProjects map[string]string

func (p fakeProjects) Lookup(_ context.Context, id string) (ProjectRef, error) {
	name, ok := p[id]
	if !ok {
		return ProjectRef{}, domain.ErrNotFound
	}
	return ProjectRef{ID: id, Name: name}, nil
}

// fixture is a service with fakes and a controllable clock.
type fixture struct {
	svc   *Service
	repo  *memRepo
	audit *memAudit
	now   time.Time
}

var (
	owner  = auth.Principal{UserID: "owner1", Login: "owner", Owner: true}
	member = auth.Principal{UserID: "u2", Login: "bob"}
)

const (
	keyA = "2hznQhRgvaYtn8K3hLubpx7uDIisg76parXO5pADfQA="
	keyB = "AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA="
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{repo: newMemRepo(), audit: &memAudit{}, now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	n := 0
	var mu sync.Mutex
	f.svc = &Service{
		Repo:     f.repo,
		Projects: fakeProjects{"p1": "Alpha", "p2": "Beta"},
		Tx:       fakeTx{},
		Audit:    f.audit,
		Rand:     &counterRand{},
		Now:      func() time.Time { return f.now },
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "id" + strconv.Itoa(n)
		},
	}
	return f
}

// counterRand yields different bytes on every read, so each token differs.
type counterRand struct {
	mu sync.Mutex
	n  byte
}

func (c *counterRand) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	for i := range b {
		b[i] = c.n + byte(i) //nolint:gosec // G115: wrap-around is fine for test bytes
	}
	return len(b), nil
}
