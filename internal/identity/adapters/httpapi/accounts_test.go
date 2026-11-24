package httpapi

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

func accountsStack(t *testing.T) http.Handler {
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
	users := sqlite.NewUserRepository(d)
	sessions := sqlite.NewSessionRepository(d)
	hash, _ := hasher.Hash(t.Context(), "correct horse battery")
	if err := users.Create(t.Context(), domain.User{ID: "u-owner", Login: "owner", PasswordHash: hash, Role: domain.RoleOwner, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	lim := func() *ratelimit.Limiter {
		return ratelimit.New(ratelimit.Config{Burst: 100, Every: time.Minute, MaxKeys: 100}, time.Now)
	}
	authn := &AuthHandler{
		Service:   &app.AuthService{Users: users, Sessions: sessions, Passwords: hasher, Now: time.Now, Rand: rand.Reader},
		IPLimiter: lim(), LoginLimiter: lim(),
	}
	accounts := &app.AccountsService{Accounts: users, Sessions: sessions, Hasher: hasher, Rand: rand.Reader, Now: time.Now, NewID: rand.Text}
	mux := http.NewServeMux()
	authn.Register(mux)
	(&AccountsHandler{Service: accounts, Limiter: ratelimit.New(ratelimit.Config{Burst: 5, Every: time.Minute, MaxKeys: 100}, time.Now)}).Register(mux)
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) { httpx.WriteJSON(w, 200, []string{}) })
	return authn.Middleware(mux)
}

func login(t *testing.T, h http.Handler, l, pw string) (string, map[string]any) {
	t.Helper()
	rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: `{"login":"` + l + `","password":"` + pw + `"}`})
	if rec.Code != 200 {
		t.Fatalf("login %s = %d %s", l, rec.Code, rec.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return sessionCookie(t, rec).Value, body
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

func TestAccountLifecycle(t *testing.T) {
	t.Parallel()
	h := accountsStack(t)
	owner, _ := login(t, h, "owner", "correct horse battery")

	rec := doCall(t, h, call{method: "POST", target: "/api/users", body: `{"login":"carol"}`, cookie: owner})
	if rec.Code != 201 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decode(t, rec)
	temp, _ := created["temporaryPassword"].(string)
	user, _ := created["user"].(map[string]any)
	if len(temp) < 12 || user["login"] != "carol" || user["mustChangePassword"] != true {
		t.Fatalf("created = %v", created)
	}
	id, _ := user["id"].(string)
	if rec := doCall(t, h, call{method: "POST", target: "/api/users", body: `{"login":"carol"}`, cookie: owner}); rec.Code != 409 {
		t.Fatalf("duplicate = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/users", cookie: owner}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"carol"`) || strings.Contains(rec.Body.String(), "argon2") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}

	// First sign-in with the temporary password: everything but me/password/logout is blocked.
	carol, body := login(t, h, "carol", temp)
	if u, _ := body["user"].(map[string]any); u["mustChangePassword"] != true {
		t.Fatalf("login body = %v", body)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/projects", cookie: carol}); rec.Code != 403 || decode(t, rec)["error"] != "password_change_required" {
		t.Fatalf("blocked API = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/auth/me", cookie: carol}); rec.Code != 200 {
		t.Fatalf("me = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/auth/password", cookie: carol,
		body: `{"currentPassword":"wrong","newPassword":"carol's own phrase"}`}); rec.Code != 401 {
		t.Fatalf("wrong current = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/auth/password", cookie: carol,
		body: `{"currentPassword":"` + temp + `","newPassword":"short"}`}); rec.Code != 422 || decode(t, rec)["field"] != "newPassword" {
		t.Fatalf("weak new = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/auth/password", cookie: carol,
		body: `{"currentPassword":"` + temp + `","newPassword":"carol's own phrase"}`}); rec.Code != 204 {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/projects", cookie: carol}); rec.Code != 200 {
		t.Fatalf("after change = %d", rec.Code)
	}

	// Disabling ends her sessions immediately; she cannot sign in until re-enabled.
	if rec := doCall(t, h, call{method: "POST", target: "/api/users/" + id + "/disable", cookie: owner}); rec.Code != 204 {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/auth/me", cookie: carol}); rec.Code != 401 {
		t.Fatalf("session after disable = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/auth/login", body: `{"login":"carol","password":"carol's own phrase"}`}); rec.Code != 401 {
		t.Fatalf("login while disabled = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/users/" + id + "/enable", cookie: owner}); rec.Code != 204 {
		t.Fatalf("enable = %d", rec.Code)
	}

	// Reset gives a new temporary password and ends sessions.
	carol, _ = login(t, h, "carol", "carol's own phrase")
	rec = doCall(t, h, call{method: "POST", target: "/api/users/" + id + "/reset-password", cookie: owner})
	if rec.Code != 200 || len(decode(t, rec)["temporaryPassword"].(string)) < 12 {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "GET", target: "/api/auth/me", cookie: carol}); rec.Code != 401 {
		t.Fatalf("session after reset = %d", rec.Code)
	}
}

func TestAccountsAPIIsOwnerOnly(t *testing.T) {
	t.Parallel()
	h := accountsStack(t)
	owner, _ := login(t, h, "owner", "correct horse battery")
	created := decode(t, doCall(t, h, call{method: "POST", target: "/api/users", body: `{"login":"dave"}`, cookie: owner}))
	temp := created["temporaryPassword"].(string)
	dave, _ := login(t, h, "dave", temp)
	_ = doCall(t, h, call{method: "POST", target: "/api/auth/password", cookie: dave, body: `{"currentPassword":"` + temp + `","newPassword":"dave's long phrase"}`})
	for _, c := range []call{
		{method: "GET", target: "/api/users"},
		{method: "POST", target: "/api/users", body: `{"login":"eve"}`},
		{method: "POST", target: "/api/users/u-owner/disable"},
		{method: "POST", target: "/api/users/u-owner/reset-password"},
	} {
		c.cookie = dave
		if rec := doCall(t, h, c); rec.Code != 403 {
			t.Fatalf("%s %s by non-owner = %d", c.method, c.target, rec.Code)
		}
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/users/u-owner/disable", cookie: owner}); rec.Code != 422 {
		t.Fatalf("disable owner = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/users/nope/disable", cookie: owner}); rec.Code != 404 {
		t.Fatalf("unknown user = %d", rec.Code)
	}
}

func TestPasswordChangeLimitsOnlyFailures(t *testing.T) {
	t.Parallel()
	h := accountsStack(t)
	owner, _ := login(t, h, "owner", "correct horse battery")
	pw := "correct horse battery"
	for i, next := range []string{"first new phrase", "second new phrase", "third new phrase", "fourth new phrase", "fifth new phrase", "sixth new phrase", "seventh new phrase"} {
		rec := doCall(t, h, call{method: "POST", target: "/api/auth/password", cookie: owner,
			body: `{"currentPassword":"` + pw + `","newPassword":"` + next + `"}`})
		if rec.Code != 204 {
			t.Fatalf("successful change %d = %d (successes must not be rate limited)", i, rec.Code)
		}
		pw = next
	}
	limited := false
	for range 12 {
		rec := doCall(t, h, call{method: "POST", target: "/api/auth/password", cookie: owner,
			body: `{"currentPassword":"guess","newPassword":"whatever phrase"}`})
		if rec.Code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated wrong current passwords were never rate limited")
	}
}
