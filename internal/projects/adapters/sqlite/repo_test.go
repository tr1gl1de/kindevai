package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"u1", "u2"} {
		if _, err := d.ExecContext(t.Context(), "INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES (?, ?, 'h', 'user', 'x')", id, id); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestRepository(t *testing.T) {
	t.Parallel()
	r := New(testDB(t))
	ctx := t.Context()
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	web := domain.Project{ID: "p1", Slug: "web", Name: "Web", CreatedAt: now}
	api := domain.Project{ID: "p2", Slug: "api", Name: "API", CreatedAt: now}
	for _, p := range []domain.Project{web, api} {
		if err := r.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(ctx, domain.Project{ID: "p3", Slug: "web", Name: "Dup", CreatedAt: now}); !errors.Is(err, domain.ErrSlugTaken) {
		t.Fatalf("duplicate slug = %v", err)
	}
	if got, err := r.ByID(ctx, "p1"); err != nil || got != web {
		t.Fatalf("ByID = %+v, %v", got, err)
	}
	if _, err := r.ByID(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	all, _ := r.All(ctx)
	if len(all) != 2 || all[0].Name != "API" {
		t.Fatalf("All = %+v", all)
	}
	if err := r.SetMember(ctx, domain.Membership{ProjectID: "p1", UserID: "u1", Role: domain.RoleMember, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetMember(ctx, domain.Membership{ProjectID: "p1", UserID: "u1", Role: domain.RoleProjectAdmin, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if m, ok, err := r.Membership(ctx, "p1", "u1"); err != nil || !ok || m.Role != domain.RoleProjectAdmin {
		t.Fatalf("Membership = %+v %v %v", m, ok, err)
	}
	if _, ok, _ := r.Membership(ctx, "p2", "u1"); ok {
		t.Fatal("membership leaked to another project")
	}
	mine, _ := r.ForUser(ctx, "u1")
	if len(mine) != 1 || mine[0].ID != "p1" {
		t.Fatalf("ForUser = %+v", mine)
	}
	if ms, _ := r.Members(ctx, "p1"); len(ms) != 1 || ms[0].UserID != "u1" {
		t.Fatalf("Members = %+v", ms)
	}
	if ok, err := r.RemoveMember(ctx, "p1", "u1"); err != nil || !ok {
		t.Fatalf("RemoveMember = %v %v", ok, err)
	}
	if ok, _ := r.RemoveMember(ctx, "p1", "u1"); ok {
		t.Fatal("removed twice")
	}
}
