package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

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

type env struct {
	h   http.Handler
	db  *sql.DB
	now atomic.Pointer[time.Time]
}

// newEnv serves the runners API; X-Test-User stands in for the identity middleware.
func newEnv(t *testing.T, burst int) *env {
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
	e := &env{db: d}
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	e.now.Store(&start)
	clock := func() time.Time { return *e.now.Load() }
	var n atomic.Int64
	h := &Handler{
		Service: &app.Service{
			Repo: sqlite.New(d), Projects: projects{}, Tx: db.Transactor{DB: d},
			Audit: &auditapp.Service{Repo: auditdb.New(d), Now: clock}, Rand: rand.Reader, Now: clock,
			NewID: func() string { return "id" + string(rune('a'+n.Add(1))) },
		},
		Limiter: ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Hour, MaxKeys: 100}, time.Now),
	}
	mux := http.NewServeMux()
	h.Register(mux)
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(w, r)
	})
	return e
}

func (e *env) do(t *testing.T, user, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var s string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		s = string(b)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(s))
	req.RemoteAddr = "192.0.2.1:1234"
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

func newKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func (e *env) token(t *testing.T) string {
	t.Helper()
	w := e.do(t, "owner", "POST", "/api/runners/enrollment-tokens", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create token = %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the response with the token must not be cached")
	}
	return decode[CreateTokenResult](t, w).Token
}

func enrollBody(token, key string) EnrollRequest {
	return EnrollRequest{Token: token, PublicKey: key, Hostname: "build-box", OS: "linux", Arch: "amd64", Version: "0.1.0"}
}

func TestEnrollmentFlowOverHTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	tok := e.token(t)
	key := newKey(t)

	w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody(tok, key))
	if w.Code != http.StatusCreated {
		t.Fatalf("enroll = %d %s", w.Code, w.Body)
	}
	res := decode[EnrollResult](t, w)
	if res.Status != "pending" || !strings.HasPrefix(res.Fingerprint, "SHA256:") || res.RunnerID == "" {
		t.Fatalf("enroll result = %+v", res)
	}
	// The same request again (a retry after a lost response) is answered with the same registration.
	w = e.do(t, "", "POST", "/api/runners/enroll", enrollBody(tok, key))
	if w.Code != http.StatusOK || decode[EnrollResult](t, w) != res {
		t.Fatalf("retry = %d %s", w.Code, w.Body)
	}
	// Another machine with the used token gets nothing.
	if w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody(tok, newKey(t))); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "token_used") {
		t.Fatalf("reuse = %d %s", w.Code, w.Body)
	}

	list := decode[[]RunnerView](t, e.do(t, "owner", "GET", "/api/runners", nil))
	if len(list) != 1 || list[0].Status != "pending" || list[0].Fingerprint != res.Fingerprint || list[0].Hostname != "build-box" || list[0].PublicKey != key {
		t.Fatalf("list = %+v", list)
	}
	if w := e.do(t, "owner", "POST", "/api/runners/"+res.RunnerID+"/approve", ProjectsRequest{ProjectIDs: []string{"nope"}}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve with unknown project = %d %s", w.Code, w.Body)
	}
	w = e.do(t, "owner", "POST", "/api/runners/"+res.RunnerID+"/approve", ProjectsRequest{ProjectIDs: []string{"p1"}})
	if v := decode[RunnerView](t, w); w.Code != http.StatusOK || v.Status != "approved" || len(v.Projects) != 1 || v.Projects[0].Name != "Alpha" {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "POST", "/api/runners/"+res.RunnerID+"/approve", ProjectsRequest{ProjectIDs: []string{"p1"}}); w.Code != http.StatusConflict {
		t.Fatalf("approve twice = %d %s", w.Code, w.Body)
	}
	for _, step := range []struct{ path, status string }{{"disable", "disabled"}, {"enable", "approved"}, {"revoke", "revoked"}} {
		w := e.do(t, "owner", "POST", "/api/runners/"+res.RunnerID+"/"+step.path, nil)
		if v := decode[RunnerView](t, w); w.Code != http.StatusOK || v.Status != step.status {
			t.Fatalf("%s = %d %s", step.path, w.Code, w.Body)
		}
	}
	if w := e.do(t, "owner", "GET", "/api/runners/nope", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing runner = %d", w.Code)
	}
	var stored int
	if err := e.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE details LIKE ? OR subject LIKE ?", "%"+tok+"%", "%"+tok+"%").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("token found in %d audit events (%v)", stored, err)
	}
}

func TestEnrollRejectsExpiredInvalidAndRevokedTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	expired := e.token(t)
	later := e.now.Load().Add(11 * time.Minute)
	e.now.Store(&later)
	revoked := e.token(t)
	tokens := decode[[]TokenView](t, e.do(t, "owner", "GET", "/api/runners/enrollment-tokens", nil))
	if len(tokens) != 2 || tokens[0].State != "pending" || tokens[1].State != "expired" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if w := e.do(t, "owner", "POST", "/api/runners/enrollment-tokens/"+tokens[0].ID+"/revoke", nil); w.Code != http.StatusNoContent {
		t.Fatalf("revoke token = %d %s", w.Code, w.Body)
	}
	for _, tt := range []struct {
		token string
		code  int
		err   string
	}{
		{expired, http.StatusGone, "token_expired"},
		{revoked, http.StatusGone, "token_revoked"},
		{"wrong", http.StatusUnauthorized, "token_invalid"},
	} {
		w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody(tt.token, newKey(t)))
		if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.err) {
			t.Errorf("enroll with %s token = %d %s", tt.err, w.Code, w.Body)
		}
	}
	if w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody(e.token(t), "bad")); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "publicKey") {
		t.Fatalf("bad key = %d %s", w.Code, w.Body)
	}
	var n int
	if err := e.db.QueryRowContext(t.Context(), "SELECT count(*) FROM runners").Scan(&n); err != nil || n != 0 {
		t.Fatalf("runners = %d, %v", n, err)
	}
}

func TestEnrollIsRateLimitedPerClient(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 2)
	for range 2 {
		if w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody("wrong", newKey(t))); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt = %d", w.Code)
		}
	}
	w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody("wrong", newKey(t)))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over the limit = %d", w.Code)
	}
}

func TestOnlyTheOwnerManagesRunners(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/runners"},
		{"GET", "/api/runners/x"},
		{"POST", "/api/runners/x/approve"},
		{"PUT", "/api/runners/x/projects"},
		{"POST", "/api/runners/x/disable"},
		{"POST", "/api/runners/x/enable"},
		{"POST", "/api/runners/x/revoke"},
		{"GET", "/api/runners/enrollment-tokens"},
		{"POST", "/api/runners/enrollment-tokens"},
		{"POST", "/api/runners/enrollment-tokens/x/revoke"},
	} {
		var body any
		if rt.method != "GET" {
			body = ProjectsRequest{ProjectIDs: []string{"p1"}}
		}
		if rt.path == "/api/runners/enrollment-tokens" || strings.HasSuffix(rt.path, "/revoke") || strings.HasSuffix(rt.path, "able") {
			body = nil
		}
		if w := e.do(t, "bob", rt.method, rt.path, body); w.Code != http.StatusForbidden {
			t.Errorf("bob %s %s = %d, want 403", rt.method, rt.path, w.Code)
		}
		if w := e.do(t, "", rt.method, rt.path, body); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", rt.method, rt.path, w.Code)
		}
	}
}
