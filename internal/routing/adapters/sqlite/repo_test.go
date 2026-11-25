package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/routing/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/routing/app"
	"github.com/tr1gl1de/kindevai/internal/routing/domain"
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

func TestSwitchesRoundTrip(t *testing.T) {
	t.Parallel()
	r, ctx := sqlite.New(testDB(t)), t.Context()
	g := app.Switch{ProjectID: "", Agent: "codex", Enabled: false, UpdatedBy: "owner", UpdatedAt: t0}
	p := app.Switch{ProjectID: "p1", Agent: "claude", Enabled: true, UpdatedBy: "admin", UpdatedAt: t0}
	for _, s := range []app.Switch{g, p, {ProjectID: "", Agent: "codex", Enabled: true, UpdatedBy: "o2", UpdatedAt: t0.Add(time.Hour)}} {
		if err := r.SetSwitch(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.Switches(ctx, "")
	if err != nil || len(got) != 1 || !got[0].Enabled || got[0].UpdatedBy != "o2" || !got[0].UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("global switches %+v, %v", got, err)
	}
	if got, _ := r.Switches(ctx, "p1"); len(got) != 1 || !reflect.DeepEqual(got[0], p) {
		t.Fatalf("project switches %+v", got)
	}
}

func TestBindingsRoundTrip(t *testing.T) {
	t.Parallel()
	r, ctx := sqlite.New(testDB(t)), t.Context()
	b := app.StoredBinding{ProjectID: "p1", UpdatedBy: "admin", UpdatedAt: t0, Binding: domain.Binding{
		Role: domain.RoleReviewer, Primary: domain.Choice{Agent: "codex", Model: "gpt-5"},
		Fallback: []domain.Choice{{Agent: "claude", Model: "opus"}}, Strategy: domain.StrategyDifferentFromAuthor}}
	if err := r.SetBinding(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := r.Bindings(ctx, "p1")
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], b) {
		t.Fatalf("bindings %+v, %v", got, err)
	}
	if got, _ := r.Bindings(ctx, ""); len(got) != 0 {
		t.Fatalf("global bindings %+v", got)
	}
	if err := r.DeleteBinding(ctx, "p1", domain.RoleReviewer); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Bindings(ctx, "p1"); len(got) != 0 {
		t.Fatalf("after delete %+v", got)
	}
}

func TestCooldownsExpire(t *testing.T) {
	t.Parallel()
	r, ctx := sqlite.New(testDB(t)), t.Context()
	for _, c := range []app.Cooldown{
		{RunnerID: "r1", Agent: "claude", Until: t0.Add(time.Hour), Reason: "limit"},
		{RunnerID: "r1", Agent: "codex", Until: t0.Add(-time.Second)},
		{RunnerID: "r2", Agent: "claude", Until: t0.Add(time.Minute)},
		{RunnerID: "r2", Agent: "claude", Until: t0.Add(2 * time.Hour), Reason: "again"},
	} {
		if err := r.SetCooldown(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.Cooldowns(ctx, t0)
	if err != nil || len(got) != 2 || got[0].Reason != "limit" || got[1].Reason != "again" || !got[1].Until.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("cooldowns %+v, %v", got, err)
	}
}
