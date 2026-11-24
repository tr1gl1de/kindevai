package sqlite_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/network/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

var t0 = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

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
	return d
}

func strictProfile(t *testing.T, id, name string) domain.Profile {
	t.Helper()
	px, _, err := domain.ParseProxyURL("socks5://alice@[2001:db8::1]:1080")
	if err != nil {
		t.Fatal(err)
	}
	p, err := domain.NewProfile(id, domain.ProfileInput{Name: name, Proxy: &px, Enforcement: domain.EnforcementStrict,
		ExpectedIPv4: "203.0.113.7", Allow: []domain.RuleInput{{Host: "*.github.com", Ports: []int{443}}}})
	if err != nil {
		t.Fatal(err)
	}
	p.CreatedAt, p.UpdatedAt = t0, t0
	return p
}

func TestProfilesRoundTrip(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	p := strictProfile(t, "a", "Strict")
	if err := r.CreateProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateProfile(ctx, strictProfile(t, "b", "Strict")); !errors.Is(err, domain.ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	got, err := r.Profile(ctx, "a")
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("Profile = %+v, %v; want %+v", got, err, p)
	}
	off, _ := domain.NewProfile("a", domain.ProfileInput{Name: "Direct"})
	off.CreatedAt, off.UpdatedAt = t0, t0.Add(time.Hour)
	if err := r.SaveProfile(ctx, off); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Profile(ctx, "a"); !reflect.DeepEqual(got, off) {
		t.Fatalf("after save %+v", got)
	}
	if _, err := r.Profile(ctx, "zzz"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAssignmentsAndRevision(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	_ = r.CreateProfile(ctx, strictProfile(t, "a", "A"))
	for _, s := range []domain.Scope{{Level: domain.LevelSystem}, {Level: domain.LevelRole, Target: "claude"}} {
		if err := r.SetAssignment(ctx, domain.Assignment{Scope: s, ProfileID: "a", UpdatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	as, err := r.Assignments(ctx)
	if err != nil || len(as) != 2 {
		t.Fatalf("assignments %+v %v", as, err)
	}
	if err := r.DeleteProfile(ctx, "a"); err == nil {
		t.Fatal("deleted an assigned profile")
	}
	_ = r.DeleteAssignment(ctx, domain.Scope{Level: domain.LevelRole, Target: "claude"})
	if as, _ := r.Assignments(ctx); len(as) != 1 || as[0].Scope.Level != domain.LevelSystem {
		t.Fatalf("after delete %+v", as)
	}
	for want := int64(1); want <= 2; want++ {
		if rev, err := r.NextRevision(ctx); err != nil || rev != want {
			t.Fatalf("NextRevision = %d, %v", rev, err)
		}
	}
	if rev, _ := r.Revision(ctx); rev != 2 {
		t.Fatalf("Revision = %d", rev)
	}
}

func TestTestsRoundTrip(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	tt := domain.NewTest("t1", "r1", "", "u1", domain.Expected{IPv4: "203.0.113.7"}, t0)
	if err := r.CreateTest(ctx, tt); err != nil {
		t.Fatal(err)
	}
	_ = r.CreateTest(ctx, domain.NewTest("t2", "r2", "", "u1", domain.Expected{}, t0.Add(time.Second)))
	_ = tt.Complete(domain.Result{OK: true, ObservedIPv4: "203.0.113.7", DurationMs: 5,
		Checks: []domain.Check{{Name: "github", Host: "github.com", OK: true, LatencyMs: 3}}}, t0.Add(time.Minute))
	if err := r.SaveTest(ctx, tt); err != nil {
		t.Fatal(err)
	}
	got, err := r.Test(ctx, "t1")
	if err != nil || !reflect.DeepEqual(got, tt) {
		t.Fatalf("Test = %+v, %v; want %+v", got, err, tt)
	}
	all, _ := r.Tests(ctx, "", 10)
	mine, _ := r.Tests(ctx, "r1", 10)
	if len(all) != 2 || all[0].ID != "t2" || len(mine) != 1 {
		t.Fatalf("all %d mine %d", len(all), len(mine))
	}
}
