package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// sqlProjects is a minimal stand-in for the projects context's app API.
type sqlProjects struct{ d *sql.DB }

func (p sqlProjects) Lookup(ctx context.Context, id string) (app.ProjectRef, error) {
	var name string
	err := db.Conn(ctx, p.d).QueryRowContext(ctx, "SELECT name FROM projects WHERE id = ?", id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return app.ProjectRef{}, domain.ErrNotFound
	}
	return app.ProjectRef{ID: id, Name: name}, err
}

func (sqlProjects) ValidateRole(role string) error {
	if role != "member" && role != "project_admin" {
		return &domain.ValidationError{Field: "role", Code: "role_unknown"}
	}
	return nil
}

func (p sqlProjects) Grant(ctx context.Context, projectID, userID, role string) error {
	_, err := db.Conn(ctx, p.d).ExecContext(ctx, "INSERT INTO project_memberships(project_id, user_id, role, created_at) VALUES (?, ?, ?, '2026-11-24T00:00:00.000000000Z')", projectID, userID, role)
	return err
}

func invitationsStack(t *testing.T, logs *bytes.Buffer, publicBurst int) (http.Handler, *sql.DB) {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), "INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', '2026-11-24T00:00:00.000000000Z')"); err != nil {
		t.Fatal(err)
	}
	hasher := password.New(password.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 4})
	users := sqlite.NewUserRepository(d)
	sessions := sqlite.NewSessionRepository(d)
	hash, _ := hasher.Hash(t.Context(), "correct horse battery")
	if err := users.Create(t.Context(), domain.User{ID: "u-owner", Login: "owner", PasswordHash: hash, Role: domain.RoleOwner, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	lim := func(burst int) *ratelimit.Limiter {
		return ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Minute, MaxKeys: 100}, time.Now)
	}
	authn := &AuthHandler{
		Service:   &app.AuthService{Users: users, Sessions: sessions, Passwords: hasher, Now: time.Now, Rand: rand.Reader},
		IPLimiter: lim(100), LoginLimiter: lim(100), Logger: logger,
	}
	invitations := &app.InvitationsService{
		Invitations: sqlite.NewInvitationRepository(d), Accounts: users, Projects: sqlProjects{d},
		Tx: db.Transactor{DB: d}, Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Hasher: hasher, Rand: rand.Reader, Now: time.Now, NewID: rand.Text, Logger: logger,
	}
	mux := http.NewServeMux()
	authn.Register(mux)
	(&InvitationsHandler{Service: invitations, Limiter: lim(publicBurst), Logger: logger}).Register(mux)
	return authn.Middleware(mux), d
}

func TestInvitationFlow(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h, d := invitationsStack(t, &logs, 100)
	owner, _ := login(t, h, "owner", "correct horse battery")

	rec := doCall(t, h, call{method: "POST", target: "/api/invitations", body: `{"projectId":"p1","role":"project_admin","ttlHours":24}`, cookie: owner})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	created := decode(t, rec)
	token, _ := created["token"].(string)
	inv, _ := created["invitation"].(map[string]any)
	if len(token) != 43 || inv["projectName"] != "Alpha" || inv["role"] != "project_admin" || inv["state"] != "pending" {
		t.Fatalf("create body = %v", created)
	}

	rec = doCall(t, h, call{method: "GET", target: "/api/invitations", cookie: owner})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "tokenHash") {
		t.Fatalf("list = %d %s (must not expose the token)", rec.Code, rec.Body)
	}

	// Public preview and accept: no session needed; the token travels only in the JSON body.
	rec = doCall(t, h, call{method: "POST", target: "/api/invitations/preview", body: `{"token":"` + token + `"}`})
	if rec.Code != 200 || decode(t, rec)["projectName"] != "Alpha" {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	rec = doCall(t, h, call{method: "POST", target: "/api/invitations/accept", body: `{"token":"` + token + `","login":"dave","password":"x"}`})
	if rec.Code != http.StatusUnprocessableEntity || decode(t, rec)["field"] != "password" {
		t.Fatalf("weak password = %d %s", rec.Code, rec.Body)
	}
	rec = doCall(t, h, call{method: "POST", target: "/api/invitations/accept", body: `{"token":"` + token + `","login":"dave","password":"dave's long password"}`})
	if rec.Code != http.StatusCreated || sessionCookie(t, rec) != nil {
		t.Fatalf("accept = %d %s (must not sign in)", rec.Code, rec.Body)
	}
	var role string
	if err := d.QueryRowContext(t.Context(), "SELECT m.role FROM project_memberships m JOIN users u ON u.id = m.user_id WHERE u.login = 'dave'").Scan(&role); err != nil || role != "project_admin" {
		t.Fatalf("membership role = %q, %v", role, err)
	}
	login(t, h, "dave", "dave's long password")

	rec = doCall(t, h, call{method: "POST", target: "/api/invitations/accept", body: `{"token":"` + token + `","login":"erin","password":"erin's long password"}`})
	if rec.Code != http.StatusGone || decode(t, rec)["error"] != "invitation_used" {
		t.Fatalf("reuse = %d %s", rec.Code, rec.Body)
	}
	rec = doCall(t, h, call{method: "POST", target: "/api/invitations/preview", body: `{"token":"nope"}`})
	if rec.Code != http.StatusNotFound || decode(t, rec)["error"] != "invitation_invalid" {
		t.Fatalf("unknown token = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("the token reached the logs")
	}
}

func TestInvitationRevoke(t *testing.T) {
	t.Parallel()
	h, _ := invitationsStack(t, &bytes.Buffer{}, 100)
	owner, _ := login(t, h, "owner", "correct horse battery")
	created := decode(t, doCall(t, h, call{method: "POST", target: "/api/invitations", body: `{"projectId":"p1","role":"member"}`, cookie: owner}))
	token, _ := created["token"].(string)
	id, _ := created["invitation"].(map[string]any)["id"].(string)

	if rec := doCall(t, h, call{method: "POST", target: "/api/invitations/" + id + "/revoke", cookie: owner}); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/invitations/" + id + "/revoke", cookie: owner}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second revoke = %d %s", rec.Code, rec.Body)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/invitations/missing/revoke", cookie: owner}); rec.Code != http.StatusNotFound {
		t.Fatalf("missing revoke = %d %s", rec.Code, rec.Body)
	}
	rec := doCall(t, h, call{method: "POST", target: "/api/invitations/preview", body: `{"token":"` + token + `"}`})
	if rec.Code != http.StatusGone || decode(t, rec)["error"] != "invitation_revoked" {
		t.Fatalf("preview revoked = %d %s", rec.Code, rec.Body)
	}
}

func TestInvitationAdminIsOwnerOnlyAndPublicPathsAreLimited(t *testing.T) {
	t.Parallel()
	h, _ := invitationsStack(t, &bytes.Buffer{}, 2)
	for _, c := range []call{
		{method: "GET", target: "/api/invitations"},
		{method: "POST", target: "/api/invitations", body: `{"projectId":"p1","role":"member"}`},
	} {
		if rec := doCall(t, h, c); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without session = %d", c.method, c.target, rec.Code)
		}
	}
	owner, _ := login(t, h, "owner", "correct horse battery")
	created := decode(t, doCall(t, h, call{method: "POST", target: "/api/invitations", body: `{"projectId":"p1","role":"member"}`, cookie: owner}))
	token, _ := created["token"].(string)
	if rec := doCall(t, h, call{method: "POST", target: "/api/invitations/accept", body: `{"token":"` + token + `","login":"frank","password":"frank's long password"}`}); rec.Code != http.StatusCreated {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	member, _ := login(t, h, "frank", "frank's long password")
	if rec := doCall(t, h, call{method: "GET", target: "/api/invitations", cookie: member}); rec.Code != http.StatusForbidden {
		t.Fatalf("member list = %d", rec.Code)
	}
	if rec := doCall(t, h, call{method: "POST", target: "/api/invitations", body: `{"projectId":"p1","role":"member"}`, cookie: member}); rec.Code != http.StatusForbidden {
		t.Fatalf("member create = %d", rec.Code)
	}
	// Burst 2 per client address for preview/accept; the accept above used one.
	doCall(t, h, call{method: "POST", target: "/api/invitations/preview", body: `{"token":"guess1"}`})
	if rec := doCall(t, h, call{method: "POST", target: "/api/invitations/preview", body: `{"token":"guess2"}`}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("token guessing not limited: %d", rec.Code)
	}
}
