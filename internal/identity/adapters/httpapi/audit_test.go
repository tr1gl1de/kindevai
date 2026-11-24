package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
)

// auditLog returns the stored events, oldest first, as "actor_type actor_id actor_label | kind | subject | details".
func auditLog(t *testing.T, d *sql.DB) []string {
	t.Helper()
	rows, err := d.QueryContext(t.Context(),
		"SELECT actor_type || ' ' || actor_id || ' ' || actor_label || ' | ' || kind || ' | ' || subject || ' | ' || details FROM audit_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func expectLog(t *testing.T, d *sql.DB, want ...string) {
	t.Helper()
	got := auditLog(t, d)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// KIN-46 AC: sign-in events: login_succeeded and logout by the user, login_failed with login and IP but never
// the password; a login name is kept only for an existing account (an unknown one may be a mistyped password).
func TestSignInIsAudited(t *testing.T) {
	t.Parallel()
	env := authStackEnv(t, 100)
	const ip = "203.0.113.7:4000"
	ok := doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: goodLogin, remote: ip})
	doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: `{"login":"owner","password":"wrong password!"}`, remote: ip})
	doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: `{"login":"Tr0ub4dor3x","password":"p"}`, remote: ip})
	doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: `{"login":"a b","password":"p"}`, remote: ip})
	// Signing in again from the same browser replaces its session: not a logout.
	again := doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: goodLogin, remote: ip, cookie: sessionCookie(t, ok).Value})
	doCall(t, env.h, call{method: "POST", target: "/api/auth/logout", cookie: sessionCookie(t, again).Value})
	doCall(t, env.h, call{method: "POST", target: "/api/auth/logout", cookie: "unknown-token"})
	expectLog(t, env.db,
		`user u1 owner | auth.login_succeeded | user:u1 | {"ip":"203.0.113.7"}`,
		`system anonymous anonymous | auth.login_failed | user:u1 | {"ip":"203.0.113.7","login":"owner","reason":"wrong_password"}`,
		`system anonymous anonymous | auth.login_failed |  | {"ip":"203.0.113.7","reason":"unknown_login"}`,
		`system anonymous anonymous | auth.login_failed |  | {"ip":"203.0.113.7","reason":"unknown_login"}`,
		`user u1 owner | auth.login_succeeded | user:u1 | {"ip":"203.0.113.7"}`,
		`user u1 owner | auth.logout | user:u1 | {}`,
	)
	for _, leak := range []string{"wrong password", "Tr0ub4dor3x", "correct horse"} {
		if strings.Contains(strings.Join(auditLog(t, env.db), "\n"), leak) {
			t.Fatalf("audit log contains %q", leak)
		}
	}
}

// Attempts refused by the rate limiter never reach the service, so they add no rows: the limiter bounds the volume.
func TestRateLimitedLoginsAreNotAudited(t *testing.T) {
	t.Parallel()
	env := authStackEnv(t, 1)
	bad := `{"login":"owner","password":"wrong password!"}`
	for range 5 {
		doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: bad, remote: "203.0.113.9:1"})
	}
	if n := len(auditLog(t, env.db)); n != 1 {
		t.Fatalf("%d audit rows for 1 allowed and 4 rate-limited attempts, want 1", n)
	}
}

type failingAudit struct{}

func (failingAudit) Record(context.Context, auditapp.Entry) error { return errors.New("disk full") }

// KIN-46 AC: rolling back the action leaves no event, and a failed event rolls back the action.
func TestSignInRollsBackWithoutAudit(t *testing.T) {
	t.Parallel()
	env := authStackWith(t, 100, func(*sql.DB) app.AuditLog { return failingAudit{} })
	if rec := doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: goodLogin}); rec.Code != 500 || sessionCookie(t, rec) != nil {
		t.Fatalf("login with a broken audit log = %d", rec.Code)
	}
	var n int
	if err := env.db.QueryRowContext(t.Context(), "SELECT count(*) FROM user_sessions").Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions = %d, %v: the session outlived its failed audit event", n, err)
	}

	// A failed attempt is answered 401 even when it cannot be recorded: a broken audit log must not change the
	// answer (500 would differ from the usual 401) or let failures skip the per-account limiter.
	bad := `{"login":"owner","password":"wrong password!"}`
	if rec := doCall(t, env.h, call{method: "POST", target: "/api/auth/login", body: bad}); rec.Code != 401 {
		t.Fatalf("failed login with a broken audit log = %d, want 401", rec.Code)
	}

	// Logout: the session stays valid when its logout cannot be recorded.
	good := authStackEnv(t, 100)
	tok := sessionCookie(t, doCall(t, good.h, call{method: "POST", target: "/api/auth/login", body: goodLogin})).Value
	good.authn.Service.Audit = failingAudit{}
	if rec := doCall(t, good.h, call{method: "POST", target: "/api/auth/logout", cookie: tok}); rec.Code != 500 {
		t.Fatalf("logout with a broken audit log = %d", rec.Code)
	}
	if rec := doCall(t, good.h, call{method: "GET", target: "/api/auth/me", cookie: tok}); rec.Code != 200 {
		t.Fatalf("session revoked although its logout was not recorded: %d", rec.Code)
	}
}

// KIN-46 AC: setup.completed is recorded with the first Owner, in the same transaction.
func TestSetupIsAudited(t *testing.T) {
	t.Parallel()
	h, ch, d := setupStackWith(t, 100, nil)
	if rec, body := send(t, h, req{method: "POST", target: "/api/setup", body: setupBody(ch.get())}); rec.Code != 201 {
		t.Fatalf("setup = %d %v", rec.Code, body)
	}
	expectLog(t, d, `user u1 owner | setup.completed | user:u1 | {"platform_name":"Acme"}`)

	h, ch, d = setupStackWith(t, 100, func(*sql.DB) app.AuditLog { return failingAudit{} })
	if rec, _ := send(t, h, req{method: "POST", target: "/api/setup", body: setupBody(ch.get())}); rec.Code != 500 {
		t.Fatalf("setup with a broken audit log = %d", rec.Code)
	}
	if _, body := send(t, h, req{method: "GET", target: "/api/setup/status"}); body["setupRequired"] != true {
		t.Fatalf("setup completed although its audit event failed: %v", body)
	}
	expectLog(t, d)
}
