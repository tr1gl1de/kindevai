package httpapi

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

func authStack(t *testing.T, burst int) http.Handler {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	hasher := password.New(password.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 4})
	hash, _ := hasher.Hash(t.Context(), "correct horse battery")
	if _, err := d.ExecContext(t.Context(), "INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', ?, 'owner', '2026-11-24T00:00:00.000000000Z')", hash); err != nil {
		t.Fatal(err)
	}
	svc := &app.AuthService{Users: sqlite.NewUserRepository(d), Sessions: sqlite.NewSessionRepository(d),
		Passwords: hasher, Now: time.Now, Rand: rand.Reader}
	lim := func() *ratelimit.Limiter {
		return ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Minute, MaxKeys: 100}, time.Now)
	}
	h := &AuthHandler{Service: svc, IPLimiter: lim(), LoginLimiter: lim()}
	mux := http.NewServeMux()
	h.Register(mux)
	mux.Handle("/api/health", httpx.Allow([]string{"GET"}, func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("/api/private", func(w http.ResponseWriter, r *http.Request) {
		p, _ := auth.FromContext(r.Context())
		httpx.WriteJSON(w, 200, map[string]any{"login": p.Login, "owner": p.Owner})
	})
	return h.Middleware(mux)
}

type call struct {
	method, target, body, cookie, remote string
	headers                              map[string]string
}

func doCall(t *testing.T, h http.Handler, c call) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), c.method, c.target, strings.NewReader(c.body))
	if c.body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: c.cookie})
	}
	if c.remote != "" {
		r.RemoteAddr = c.remote
	}
	for k, v := range c.headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	return nil
}

const goodLogin = `{"login":"owner","password":"correct horse battery"}`

func TestLoginMeLogout(t *testing.T) {
	t.Parallel()
	h := authStack(t, 100)
	rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: goodLogin})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"login":"owner"`) {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	c := sessionCookie(t, rec)
	if c == nil || c.Value == "" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure {
		t.Fatalf("cookie = %+v", c)
	}
	if strings.Contains(rec.Body.String(), c.Value) {
		t.Fatal("session token in response body")
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/auth/me", cookie: c.Value}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"owner"`) {
		t.Fatalf("me = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/private", cookie: c.Value}); !strings.Contains(rec.Body.String(), `"owner":true`) {
		t.Fatalf("principal not in context: %s", rec.Body)
	}
	rec = doCall(t, h, call{method: "POST", target: "/api/auth/logout", cookie: c.Value})
	if rec.Code != 204 {
		t.Fatalf("logout = %d", rec.Code)
	}
	if cleared := sessionCookie(t, rec); cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/auth/me", cookie: c.Value}); rec.Code != 401 {
		t.Fatalf("me after logout = %d", rec.Code)
	}
}

func TestSecureCookieBehindHTTPS(t *testing.T) {
	t.Parallel()
	h := authStack(t, 100)
	rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: goodLogin, headers: map[string]string{"X-Forwarded-Proto": "https"}})
	if c := sessionCookie(t, rec); c == nil || !c.Secure {
		t.Fatalf("cookie over HTTPS proxy not Secure: %+v", c)
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	t.Parallel()
	h := authStack(t, 100)
	var bodies []string
	for _, b := range []string{
		`{"login":"owner","password":"wrong password!"}`,
		`{"login":"nobody","password":"correct horse battery"}`,
		`{"login":"x","password":"y"}`,
	} {
		rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: b})
		if rec.Code != 401 || sessionCookie(t, rec) != nil {
			t.Fatalf("%s → %d", b, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("failure responses differ: %q", bodies)
	}
}

func TestAPIRequiresSessionByDefault(t *testing.T) {
	t.Parallel()
	h := authStack(t, 100)
	tests := []struct {
		target string
		code   int
	}{
		{"/api/private", 401},
		{"/api/auth/me", 401},
		{"/api/unknown", 401},
		{"/api/health", 200},
	}
	for _, tt := range tests {
		if rec := doCall(t, h, call{method: "GET", target: tt.target, cookie: "forged"}); rec.Code != tt.code {
			t.Fatalf("%s = %d, want %d", tt.target, rec.Code, tt.code)
		}
	}
}

func TestCrossOriginStateChangeIsRejected(t *testing.T) {
	t.Parallel()
	h := authStack(t, 100)
	rec := doCall(t, h, call{method: "POST", target: "http://cp.local/api/auth/login", body: goodLogin,
		headers: map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}})
	if rec.Code != 403 || sessionCookie(t, rec) != nil {
		t.Fatalf("cross-site login = %d", rec.Code)
	}
	rec = doCall(t, h, call{method: "POST", target: "http://cp.local/api/auth/login", body: goodLogin,
		headers: map[string]string{"Origin": "http://cp.local", "Sec-Fetch-Site": "same-origin"}})
	if rec.Code != 200 {
		t.Fatalf("same-origin login = %d", rec.Code)
	}
}

func TestLoginRateLimits(t *testing.T) {
	t.Parallel()
	h := authStack(t, 2)
	bad := `{"login":"owner","password":"wrong password!"}`
	for i := range 2 {
		if rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: bad, remote: "203.0.113.1:1"}); rec.Code != 401 {
			t.Fatalf("attempt %d = %d", i, rec.Code)
		}
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: bad, remote: "203.0.113.1:1"}); rec.Code != 429 {
		t.Fatalf("per-IP limit: %d", rec.Code)
	}
	// Same login from another address is limited by the per-login bucket (owner already used 2 attempts).
	if rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: goodLogin, remote: "198.51.100.2:1"}); rec.Code != 429 {
		t.Fatalf("per-login limit: %d", rec.Code)
	}
}
