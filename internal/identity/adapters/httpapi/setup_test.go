package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

type plainHasher struct{}

func (plainHasher) Hash(_ context.Context, pw string) (string, error) { return "h:" + pw, nil }

type memChannel struct {
	mu  sync.Mutex
	tok string
}

func (c *memChannel) Publish(p string) error { c.mu.Lock(); c.tok = p; c.mu.Unlock(); return nil }
func (c *memChannel) Clear() error           { c.mu.Lock(); c.tok = ""; c.mu.Unlock(); return nil }
func (c *memChannel) get() string            { c.mu.Lock(); defer c.mu.Unlock(); return c.tok }

func setupStack(t *testing.T, burst int) (http.Handler, *memChannel) {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	ch := &memChannel{}
	svc := &app.SetupService{Repo: sqlite.NewSetupRepository(d), Hasher: plainHasher{}, Channel: ch, Now: time.Now,
		Rand: rand.Reader, NewID: func() string { return "u1" }}
	if _, err := svc.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	lim := ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Minute, MaxKeys: 100}, time.Now)
	mux := http.NewServeMux()
	(&SetupHandler{Service: svc, Limiter: lim}).Register(mux)
	return mux, ch
}

type req struct {
	method, target, body string
	headers              map[string]string
	remote               string
}

func send(t *testing.T, h http.Handler, r req) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	hr := httptest.NewRequestWithContext(t.Context(), r.method, r.target, strings.NewReader(r.body))
	hr.Header.Set("Content-Type", "application/json")
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func setupBody(token string) string {
	b, _ := json.Marshal(map[string]string{"token": token, "login": "owner", "password": "correct horse battery", "platformName": "Acme"})
	return string(b)
}

func TestSetupFlow(t *testing.T) {
	t.Parallel()
	h, ch := setupStack(t, 100)
	rec, body := send(t, h, req{method: "GET", target: "/api/setup/status"})
	if rec.Code != 200 || body["setupRequired"] != true {
		t.Fatalf("status = %d %v", rec.Code, body)
	}
	if strings.Contains(rec.Body.String(), ch.get()) {
		t.Fatal("status response leaks the setup token")
	}
	rec, body = send(t, h, req{method: "POST", target: "/api/setup", body: setupBody("wrong")})
	if rec.Code != 401 || body["error"] != "invalid_setup_token" {
		t.Fatalf("wrong token = %d %v", rec.Code, body)
	}
	tok := ch.get()
	rec, body = send(t, h, req{method: "POST", target: "/api/setup", body: setupBody(tok)})
	if rec.Code != 201 || body["login"] != "owner" {
		t.Fatalf("setup = %d %v", rec.Code, body)
	}
	if strings.Contains(rec.Body.String(), "correct horse") || strings.Contains(rec.Body.String(), "h:") {
		t.Fatal("response leaks the password or its hash")
	}
	rec, body = send(t, h, req{method: "GET", target: "/api/setup/status"})
	if body["setupRequired"] != false {
		t.Fatalf("status after setup = %v", body)
	}
	rec, body = send(t, h, req{method: "POST", target: "/api/setup", body: setupBody(tok)})
	if rec.Code != 409 || body["error"] != "setup_completed" {
		t.Fatalf("repeat = %d %v", rec.Code, body)
	}
}

func TestSetupIgnoresUntrustedContext(t *testing.T) {
	t.Parallel()
	h, ch := setupStack(t, 100)
	spoofed := map[string]string{
		"X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1", "Forwarded": "for=127.0.0.1;host=localhost",
		"Host": "localhost",
	}
	// No token in the body: neither spoofed "local" headers nor a token in the URL may stand in for it.
	body := `{"token":"","login":"owner","password":"correct horse battery","platformName":"Acme"}`
	rec, _ := send(t, h, req{method: "POST", target: "/api/setup?token=" + ch.get(), body: body, headers: spoofed, remote: "127.0.0.1:5555"})
	if rec.Code != 401 {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestSetupValidationAndBadRequests(t *testing.T) {
	t.Parallel()
	h, ch := setupStack(t, 100)
	tests := []struct {
		name      string
		r         req
		wantCode  int
		wantError string
		wantField string
	}{
		{"short password", req{method: "POST", target: "/api/setup",
			body: `{"token":"` + ch.get() + `","login":"owner","password":"short","platformName":"Acme"}`}, 422, "validation", "password"},
		{"unknown field", req{method: "POST", target: "/api/setup", body: `{"token":"x","role":"owner"}`}, 400, "invalid_json", ""},
		{"not json", req{method: "POST", target: "/api/setup", body: `token=x`, headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}, 415, "unsupported_media_type", ""},
		{"huge body", req{method: "POST", target: "/api/setup", body: `{"token":"` + strings.Repeat("a", 20000) + `"}`}, 413, "body_too_large", ""},
		{"wrong method", req{method: "GET", target: "/api/setup"}, 405, "method_not_allowed", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec, body := send(t, h, tt.r)
			if rec.Code != tt.wantCode || body["error"] != tt.wantError {
				t.Fatalf("= %d %v, want %d %s", rec.Code, body, tt.wantCode, tt.wantError)
			}
			if tt.wantField != "" && body["field"] != tt.wantField {
				t.Fatalf("field = %v", body["field"])
			}
		})
	}
}

func TestSetupIsRateLimitedByPeerAddress(t *testing.T) {
	t.Parallel()
	h, _ := setupStack(t, 3)
	for i := range 3 {
		rec, _ := send(t, h, req{method: "POST", target: "/api/setup", body: setupBody("bad"), remote: "203.0.113.7:1000",
			headers: map[string]string{"X-Forwarded-For": "10.0.0." + string(rune('1'+i))}})
		if rec.Code != 401 {
			t.Fatalf("attempt %d = %d", i, rec.Code)
		}
	}
	rec, body := send(t, h, req{method: "POST", target: "/api/setup", body: setupBody("bad"), remote: "203.0.113.7:2000",
		headers: map[string]string{"X-Forwarded-For": "10.9.9.9"}})
	if rec.Code != 429 || body["error"] != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("4th attempt = %d %v", rec.Code, body)
	}
	if rec, _ := send(t, h, req{method: "POST", target: "/api/setup", body: setupBody("bad"), remote: "198.51.100.1:1"}); rec.Code != 401 {
		t.Fatalf("other peer limited: %d", rec.Code)
	}
}
