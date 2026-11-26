package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

const token = "perm:b3duZXI=.s3cr3tT0ken.AbCdEf123456"

type projects struct{}

func (projects) Lookup(_ context.Context, id string) (app.ProjectRef, error) {
	if id == "p1" {
		return app.ProjectRef{ID: "p1", Name: "Alpha"}, nil
	}
	return app.ProjectRef{}, domain.ErrNotFound
}

var principals = map[string]auth.Principal{
	"owner": {UserID: "owner1", Login: "owner", Owner: true},
	"bob":   {UserID: "bob1", Login: "bob"},
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	h      http.Handler
	db     *sql.DB
	outbox *messaging.Outbox
	svc    *app.Service
	gh     *app.GitHubService
	inbox  *messaging.Inbox
	logs   *syncBuf
}

// newEnv serves the integrations API; X-Test-User stands in for the identity middleware.
func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner1', 'owner', 'h', 'owner', 'x'), ('bob1', 'bob', 'h', 'user', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	var k secrets.Key
	_, _ = rand.Read(k[:])
	box, _ := secrets.NewBox(k)
	logs := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	repo := sqlite.New(d)
	ob := messaging.NewOutbox(d, messaging.Config{Logger: logger})
	svc := &app.Service{
		Vault: &app.Vault{Store: repo, Cipher: box, Now: time.Now}, Store: repo, Projects: projects{},
		Tx: db.Transactor{DB: d}, Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Outbox: ob,
		Now: time.Now, NewID: rand.Text, Logger: logger,
	}
	inbox := messaging.NewInbox(d, messaging.Config{Logger: logger})
	gh := &app.GitHubService{Vault: svc.Vault, Store: repo, Projects: projects{}, Tx: svc.Tx, Audit: svc.Audit, Outbox: ob,
		Inbox: inbox, Now: time.Now, NewID: rand.Text, Logger: logger}
	svc.RepositoryAdded = gh.RepositoryAdded
	h := &Handler{Service: svc, GitHub: gh, Logger: logger}
	mux := http.NewServeMux()
	h.Register(mux)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(w, r)
	})
	return &env{h: wrapped, db: d, outbox: ob, svc: svc, gh: gh, inbox: inbox, logs: logs}
}

func (e *env) do(t *testing.T, user, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	return v
}

func TestYouTrackConnectionIsWriteOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do(t, "", "GET", "/api/integrations", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w := e.do(t, "bob", "GET", "/api/integrations", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", w.Code)
	}
	if w := e.do(t, "bob", "PUT", "/api/integrations/youtrack", YouTrackRequest{BaseURL: "https://a.youtrack.cloud", Token: token}); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner put: %d", w.Code)
	}
	w := e.do(t, "owner", "GET", "/api/integrations", nil)
	if w.Code != http.StatusOK || decode[Integrations](t, w).YouTrack != nil {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	w = e.do(t, "owner", "PUT", "/api/integrations/youtrack", YouTrackRequest{BaseURL: "ftp://x", Token: token})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"field":"baseUrl"`) {
		t.Fatalf("invalid url: %d %s", w.Code, w.Body)
	}
	w = e.do(t, "owner", "PUT", "/api/integrations/youtrack", YouTrackRequest{BaseURL: "https://a.youtrack.cloud/", Token: token})
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	yt := decode[YouTrack](t, w)
	if yt.BaseURL != "https://a.youtrack.cloud" || !yt.Token.Set || yt.Token.UpdatedAt == nil || yt.Check.Status != "none" {
		t.Fatalf("view %+v", yt)
	}
	if w := e.do(t, "owner", "GET", "/api/integrations", nil); strings.Contains(w.Body.String(), "s3cr3t") {
		t.Fatalf("GET leaks the token: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), "s3cr3t") {
		t.Fatal("PUT response leaks the token")
	}
	// Token omitted: kept for the same URL, required for a new one.
	if w := e.do(t, "owner", "PUT", "/api/integrations/youtrack", YouTrackRequest{BaseURL: "https://a.youtrack.cloud"}); w.Code != http.StatusOK {
		t.Fatalf("save without token: %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "PUT", "/api/integrations/youtrack", YouTrackRequest{BaseURL: "https://b.youtrack.cloud"}); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "token_required_for_url") {
		t.Fatalf("new url without token: %d %s", w.Code, w.Body)
	}
	if s, err := e.svc.YouTrackSettings(t.Context()); err != nil || s.Token.Reveal() != token {
		t.Fatalf("token lost: %v", err)
	}
	var events int
	_ = e.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE kind = ? AND details LIKE '%s3cr3t%'", app.KindYouTrackConfigured).Scan(&events)
	if events != 0 {
		t.Fatal("audit details leak the token")
	}
	if strings.Contains(e.logs.String(), "s3cr3t") {
		t.Fatalf("logs leak the token: %s", e.logs.String())
	}

	if w := e.do(t, "owner", "DELETE", "/api/integrations/youtrack", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := e.do(t, "owner", "DELETE", "/api/integrations/youtrack", nil); w.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", w.Code)
	}
}

func TestCheckConnectionIsQueued(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do(t, "owner", "POST", "/api/integrations/youtrack/check", nil); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "not_configured") {
		t.Fatalf("unconfigured: %d %s", w.Code, w.Body)
	}
	e.do(t, "owner", "PUT", "/api/integrations/youtrack", YouTrackRequest{BaseURL: "https://a.youtrack.cloud", Token: token})
	if w := e.do(t, "bob", "POST", "/api/integrations/youtrack/check", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", w.Code)
	}
	w := e.do(t, "owner", "POST", "/api/integrations/youtrack/check", nil)
	if w.Code != http.StatusAccepted || decode[YouTrack](t, w).Check.Status != "pending" {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	stats, err := e.outbox.Stats(t.Context())
	if err != nil || stats[app.AdapterYouTrack].Pending != 1 {
		t.Fatalf("outbox %v %v", stats, err)
	}
}

func TestProjectIntegrationsAPI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do(t, "bob", "GET", "/api/projects/p1/integrations", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", w.Code)
	}
	if w := e.do(t, "owner", "GET", "/api/projects/nope/integrations", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", w.Code)
	}
	w := e.do(t, "owner", "PUT", "/api/projects/p1/integrations/youtrack", ShortNameRequest{ShortName: "kin"})
	if w.Code != http.StatusOK || decode[ProjectIntegrations](t, w).YouTrackProject != "KIN" {
		t.Fatalf("bind: %d %s", w.Code, w.Body)
	}
	w = e.do(t, "owner", "POST", "/api/projects/p1/repositories", RepositoryRequest{Owner: "acme", Name: "web"})
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	repo := decode[Repository](t, w)
	if repo.FullName != "acme/web" || repo.IntegrationBranch != "dev" || len(repo.ProtectedBranches) != 2 {
		t.Fatalf("repo %+v", repo)
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/repositories", RepositoryRequest{Owner: "ACME", Name: "Web"}); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "repository_exists") {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/repositories", RepositoryRequest{Owner: "acme", Name: "api", IntegrationBranch: "main"}); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "branch_protected") {
		t.Fatalf("protected: %d %s", w.Code, w.Body)
	}
	w = e.do(t, "owner", "PUT", "/api/projects/p1/repositories/"+repo.ID, BranchRequest{IntegrationBranch: "next"})
	if w.Code != http.StatusOK || decode[Repository](t, w).IntegrationBranch != "next" {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	pi := decode[ProjectIntegrations](t, e.do(t, "owner", "GET", "/api/projects/p1/integrations", nil))
	if pi.YouTrackProject != "KIN" || len(pi.Repositories) != 1 || pi.Repositories[0].IntegrationBranch != "next" {
		t.Fatalf("get %+v", pi)
	}
	if w := e.do(t, "owner", "DELETE", "/api/projects/p1/repositories/"+repo.ID, nil); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d", w.Code)
	}
	if w := e.do(t, "owner", "DELETE", "/api/projects/p1/repositories/"+repo.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("remove again: %d", w.Code)
	}
	if w := e.do(t, "owner", "PUT", "/api/projects/p1/integrations/youtrack", ShortNameRequest{}); w.Code != http.StatusOK ||
		decode[ProjectIntegrations](t, w).YouTrackProject != "" {
		t.Fatalf("unbind: %d %s", w.Code, w.Body)
	}
}
