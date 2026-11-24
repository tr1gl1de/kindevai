package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

// store is an in-memory app.Store and app.SecretStore.
type store struct {
	mu       sync.Mutex
	secrets  map[domain.SecretName]storedSecret
	yt       *domain.YouTrackConnection
	bindings map[string]string // project -> short name
	repos    map[string]domain.Repository
}

type storedSecret struct {
	ciphertext []byte
	by         string
	at         time.Time
}

func newStore() *store {
	return &store{secrets: map[domain.SecretName]storedSecret{}, bindings: map[string]string{}, repos: map[string]domain.Repository{}}
}

func (s *store) PutSecret(_ context.Context, name domain.SecretName, ct []byte, by string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[name] = storedSecret{slices.Clone(ct), by, at}
	return nil
}

func (s *store) SecretCiphertext(_ context.Context, name domain.SecretName) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.secrets[name]
	if !ok {
		return nil, domain.ErrSecretNotSet
	}
	return slices.Clone(v.ciphertext), nil
}

func (s *store) SecretMeta(_ context.Context, name domain.SecretName) (domain.SecretMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.secrets[name]
	if !ok {
		return domain.SecretMeta{Name: name}, nil
	}
	return domain.SecretMeta{Name: name, Set: true, UpdatedAt: v.at, UpdatedBy: v.by}, nil
}

func (s *store) DeleteSecret(_ context.Context, name domain.SecretName) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.secrets, name)
	return nil
}

func (s *store) YouTrack(context.Context) (domain.YouTrackConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.yt == nil {
		return domain.YouTrackConnection{}, domain.ErrNotConfigured
	}
	return *s.yt, nil
}

func (s *store) SaveYouTrack(_ context.Context, c domain.YouTrackConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.yt = &c
	return nil
}

func (s *store) DeleteYouTrack(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.yt = nil
	return nil
}

func (s *store) ProjectYouTrack(_ context.Context, projectID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindings[projectID], nil
}

func (s *store) SetProjectYouTrack(_ context.Context, projectID, short string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if short == "" {
		delete(s.bindings, projectID)
		return nil
	}
	for p, sn := range s.bindings {
		if sn == short && p != projectID {
			return domain.ErrShortNameTaken
		}
	}
	s.bindings[projectID] = short
	return nil
}

func (s *store) Repositories(_ context.Context, projectID string) ([]domain.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Repository
	for _, id := range slices.Sorted(maps.Keys(s.repos)) {
		if r := s.repos[id]; r.ProjectID == projectID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *store) Repository(_ context.Context, projectID, id string) (domain.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[id]
	if !ok || r.ProjectID != projectID {
		return domain.Repository{}, domain.ErrNotFound
	}
	return r, nil
}

func (s *store) CreateRepository(_ context.Context, r domain.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.repos {
		if strings.EqualFold(o.FullName(), r.FullName()) {
			return domain.ErrRepositoryExists
		}
	}
	s.repos[r.ID] = r
	return nil
}

func (s *store) SaveRepository(_ context.Context, r domain.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[r.ID] = r
	return nil
}

func (s *store) DeleteRepository(_ context.Context, projectID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.repos[id]; !ok || r.ProjectID != projectID {
		return domain.ErrNotFound
	}
	delete(s.repos, id)
	return nil
}

type tx struct{}

func (tx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

type auditLog struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (a *auditLog) Record(_ context.Context, e auditapp.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditLog) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Kind)
	}
	return out
}

func (a *auditLog) dump() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(a.entries)
	return string(b)
}

type outbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (o *outbox) Enqueue(_ context.Context, m messaging.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = append(o.msgs, m)
	return nil
}

type projects struct{}

func (projects) Lookup(_ context.Context, id string) (app.ProjectRef, error) {
	switch id {
	case "p1":
		return app.ProjectRef{ID: "p1", Name: "Alpha"}, nil
	case "p2":
		return app.ProjectRef{ID: "p2", Name: "Beta"}, nil
	}
	return app.ProjectRef{}, domain.ErrNotFound
}

type probe struct {
	mu     sync.Mutex
	calls  []string // base URL + token seen
	result domain.CheckResult
}

func (p *probe) Probe(_ context.Context, baseURL string, token domain.Secret) domain.CheckResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, baseURL+" "+token.Reveal())
	return p.result
}

var (
	owner = auth.Principal{UserID: "owner1", Login: "owner", Owner: true}
	bob   = auth.Principal{UserID: "bob1", Login: "bob"}
	t0    = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
)

type env struct {
	svc   *app.Service
	store *store
	audit *auditLog
	out   *outbox
	probe *probe
	logs  *bytes.Buffer
	now   time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var k secrets.Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(k)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: newStore(), audit: &auditLog{}, out: &outbox{}, probe: &probe{}, logs: &bytes.Buffer{}, now: t0}
	var n int
	var mu sync.Mutex
	clock := func() time.Time { return e.now }
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.svc = &app.Service{
		Vault:    &app.Vault{Store: e.store, Cipher: box, Now: clock},
		Store:    e.store,
		Projects: projects{},
		Tx:       tx{},
		Audit:    e.audit,
		Outbox:   e.out,
		Probe:    e.probe,
		Now:      clock,
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "id" + string(rune('0'+n))
		},
		Logger: logger,
	}
	return e
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
