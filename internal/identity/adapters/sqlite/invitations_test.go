package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

func seedProject(t *testing.T, d *sql.DB, id string) {
	t.Helper()
	if _, err := d.ExecContext(t.Context(), "INSERT INTO projects(id, slug, name, created_at) VALUES (?, ?, ?, '2026-11-24T00:00:00.000000000Z')", id, id, "Project "+id); err != nil {
		t.Fatal(err)
	}
}

func seedOwner(t *testing.T, d *sql.DB) {
	t.Helper()
	u := domain.User{ID: "owner", Login: "owner", PasswordHash: "h", Role: domain.RoleOwner, CreatedAt: time.Now()}
	if err := NewUserRepository(d).Create(t.Context(), u); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationRepository(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	seedOwner(t, d)
	seedProject(t, d, "p1")
	r := NewInvitationRepository(d)
	ctx := t.Context()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	inv := domain.Invitation{ID: "i1", TokenHash: domain.HashInvitationToken("tok"), ProjectID: "p1", Role: "member",
		CreatedBy: "owner", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := r.Create(ctx, inv); err != nil {
		t.Fatal(err)
	}
	got, err := r.ByTokenHash(ctx, domain.HashInvitationToken("tok"))
	if err != nil || got.ID != "i1" || got.ProjectID != "p1" || !got.ExpiresAt.Equal(inv.ExpiresAt) || got.ConsumedAt != nil {
		t.Fatalf("ByTokenHash = %+v, %v", got, err)
	}
	if _, err := r.ByTokenHash(ctx, domain.HashInvitationToken("other")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}
	if _, err := r.ByID(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	// Expired at now: neither consumable nor revocable.
	if ok, err := r.MarkConsumed(ctx, "i1", "owner", now.Add(time.Hour)); ok || err != nil {
		t.Fatalf("consume expired = %v, %v", ok, err)
	}
	if ok, err := r.Revoke(ctx, "i1", now); !ok || err != nil {
		t.Fatalf("Revoke = %v, %v", ok, err)
	}
	if ok, _ := r.MarkConsumed(ctx, "i1", "owner", now); ok {
		t.Fatal("revoked invitation consumed")
	}
	got, _ = r.ByID(ctx, "i1")
	if got.State(now) != domain.InvitationRevoked {
		t.Fatalf("state = %s", got.State(now))
	}
	list, err := r.List(ctx, 10, now)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	// An old pending invitation stays listed (and revocable) however many newer ones exist.
	old := inv
	old.ID, old.TokenHash, old.CreatedAt, old.ExpiresAt = "old", domain.HashInvitationToken("old"), now.Add(-time.Hour), now.Add(time.Hour)
	if err := r.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		n := inv
		n.ID, n.TokenHash, n.CreatedAt = fmt.Sprint("n", i), domain.HashInvitationToken(fmt.Sprint("n", i)), now.Add(time.Duration(i+1)*time.Minute)
		n.ExpiresAt = now // already expired
		if err := r.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	list, err = r.List(ctx, 2, now)
	if err != nil || len(list) != 3 || list[2].ID != "old" {
		t.Fatalf("List(limit 2) = %d items, %v; want the 2 newest plus the pending old one", len(list), err)
	}
}

// projectsPort grants memberships with plain SQL, standing in for the projects context's app API in this
// adapter-level test (the real wiring lives in cmd/ai-platform).
type projectsPort struct{ d *sql.DB }

func (p projectsPort) Lookup(ctx context.Context, id string) (app.ProjectRef, error) {
	var name string
	err := db.Conn(ctx, p.d).QueryRowContext(ctx, "SELECT name FROM projects WHERE id = ?", id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return app.ProjectRef{}, domain.ErrNotFound
	}
	return app.ProjectRef{ID: id, Name: name}, err
}
func (projectsPort) ValidateRole(string) error { return nil }
func (p projectsPort) Grant(ctx context.Context, projectID, userID, role string) error {
	_, err := db.Conn(ctx, p.d).ExecContext(ctx, "INSERT INTO project_memberships(project_id, user_id, role, created_at) VALUES (?, ?, ?, '2026-11-24T00:00:00.000000000Z')", projectID, userID, role)
	return err
}

func TestConcurrentAcceptsYieldExactlyOneAccount(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	seedOwner(t, d)
	seedProject(t, d, "p1")
	svc := &app.InvitationsService{
		Invitations: NewInvitationRepository(d), Accounts: NewUserRepository(d), Projects: projectsPort{d},
		Tx: db.Transactor{DB: d}, Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Hasher: plainHasher{}, Rand: rand.Reader, Now: time.Now, NewID: rand.Text,
	}
	_, token, err := svc.Create(t.Context(), auth.Principal{UserID: "owner", Owner: true}, "p1", "member", 1)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = svc.Accept(t.Context(), token, "racer"+string(rune('a'+i)), "a long enough password")
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, domain.ErrInvitationUsed):
			t.Errorf("loser error = %v, want ErrInvitationUsed", err)
		}
	}
	var users, members int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM users WHERE platform_role = 'user'").Scan(&users)
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM project_memberships").Scan(&members)
	if ok != 1 || users != 1 || members != 1 {
		t.Fatalf("successes %d, users %d, memberships %d; want 1 each", ok, users, members)
	}
	if n := auditCount(t, d, "invitation.accepted"); n != 1 {
		t.Fatalf("%d invitation.accepted audit events, want exactly 1 (losers roll theirs back)", n)
	}
}

func TestAcceptRollsBackOnGrantFailure(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	seedOwner(t, d)
	seedProject(t, d, "p1")
	svc := &app.InvitationsService{
		Invitations: NewInvitationRepository(d), Accounts: NewUserRepository(d), Projects: projectsPort{d},
		Tx: db.Transactor{DB: d}, Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Hasher: plainHasher{}, Rand: rand.Reader, Now: time.Now, NewID: rand.Text,
	}
	_, token, err := svc.Create(t.Context(), auth.Principal{UserID: "owner", Owner: true}, "p1", "bogus_role", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept(t.Context(), token, "newuser", "a long enough password"); err == nil {
		t.Fatal("grant with a role the schema rejects must fail")
	}
	var users int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM users WHERE login = 'newuser'").Scan(&users)
	if users != 0 {
		t.Fatal("user survived the rolled-back acceptance")
	}
	if n := auditCount(t, d, "invitation.accepted"); n != 0 {
		t.Fatalf("audit event survived the rolled-back acceptance: %d", n)
	}
	if n := auditCount(t, d, "invitation.created"); n != 1 {
		t.Fatalf("invitation.created events = %d, want 1", n)
	}
	if _, err := svc.Preview(t.Context(), token); err != nil {
		t.Fatalf("invitation must stay pending: %v", err)
	}
}

func auditCount(t *testing.T, d *sql.DB, kind string) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE kind = ?", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
