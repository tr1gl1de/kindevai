package sqlite_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
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
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x'), ('p2', 'beta', 'Beta', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func alert(t *testing.T, id, project, key string, at time.Time) domain.Alert {
	t.Helper()
	a, err := domain.NewAlert(id, domain.Occurrence{ProjectID: project, Kind: domain.KindStorageLevel,
		Severity: domain.SeverityWarning, DedupKey: key, Subject: "m1/data", Details: map[string]string{"usedPercent": "80"}}, at)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAlertRoundTripAndOneActivePerKey(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	a := alert(t, "a1", "", "storage/m1/data", t0)
	a.LastEvent = "ev1"
	if err := r.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := r.Active(ctx, "storage/m1/data")
	if err != nil || got.ID != "a1" || got.Details["usedPercent"] != "80" || got.LastEvent != "ev1" || !got.FirstSeenAt.Equal(t0) {
		t.Fatalf("active = %+v, %v", got, err)
	}
	if err := r.Create(ctx, alert(t, "a2", "", "storage/m1/data", t0)); err == nil {
		t.Fatal("second active alert with the same key accepted")
	}
	if err := got.Acknowledge("u1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := got.Resolve("", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Active(ctx, "storage/m1/data"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("resolved alert still active: %v", err)
	}
	if ok, err := r.Applied(ctx, "storage/m1/data", "ev1"); !ok || err != nil {
		t.Fatalf("applied ev1 = %v, %v", ok, err)
	}
	if ok, _ := r.Applied(ctx, "storage/m1/data", "ev2"); ok {
		t.Fatal("ev2 reported applied")
	}
	back, _ := r.Get(ctx, "a1")
	if back.State != domain.StateResolved || back.AckedBy != "u1" || back.ClosedBy != "" || back.ClosedAt == nil {
		t.Fatalf("back = %+v", back)
	}
	// Closed alerts are immutable; a new one with the same key may open.
	back.Occurrences = 9
	if err := r.Save(ctx, back); err == nil {
		t.Fatal("closed alert changed")
	}
	if err := r.Create(ctx, alert(t, "a3", "", "storage/m1/data", t0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
}

// AC: project isolation at the query level.
func TestListScopes(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	for i, x := range []struct{ id, project string }{{"plat", ""}, {"one", "p1"}, {"two", "p2"}} {
		if err := r.Create(ctx, alert(t, x.id, x.project, "k/"+x.id, t0.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q app.ListQuery) []string {
		as, err := r.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	if got := ids(app.ListQuery{ProjectIDs: []string{"p1"}}); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("p1 = %q", got)
	}
	if got := ids(app.ListQuery{Platform: true, ProjectIDs: []string{"p1", "p2"}}); !slices.Equal(got, []string{"two", "one", "plat"}) {
		t.Fatalf("all = %q", got)
	}
	if got := ids(app.ListQuery{}); len(got) != 0 {
		t.Fatalf("empty scope = %q", got)
	}
}

func TestPreferencesDefaultAndSave(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	p, err := r.Preferences(ctx, "u1")
	if err != nil || p.Language != domain.LangEN || len(p.Disabled) != 0 {
		t.Fatalf("default = %+v, %v", p, err)
	}
	want, _ := domain.NewPreferences("u1", "ru", []domain.NotificationType{domain.NotifyAlertRaised})
	for range 2 { // upsert
		if err := r.SavePreferences(ctx, want, t0); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := r.Preferences(ctx, "u1")
	if got.Language != domain.LangRU || !slices.Equal(got.Disabled, want.Disabled) {
		t.Fatalf("got = %+v", got)
	}
}

// KIN-60: closed alerts older than the cutoff are deleted in batches; active and recent ones stay.
func TestDeleteClosedBefore(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	old, recent := t0.Add(-100*24*time.Hour), t0.Add(-time.Hour)
	for i, c := range []struct {
		at     time.Time
		closed bool
	}{{old, true}, {old, true}, {old, true}, {old, false}, {recent, true}} {
		id := "a" + string(rune('0'+i))
		a := alert(t, id, "", "k"+id, c.at)
		if err := r.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		if c.closed {
			if err := a.Resolve("", c.at); err != nil {
				t.Fatal(err)
			}
			if err := r.Save(ctx, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	cutoff := t0.Add(-90 * 24 * time.Hour)
	if n, err := r.DeleteClosed(ctx, cutoff, 2); err != nil || n != 2 {
		t.Fatalf("first batch = %d, %v", n, err)
	}
	if n, err := r.DeleteClosed(ctx, cutoff, 2); err != nil || n != 1 {
		t.Fatalf("second batch = %d, %v", n, err)
	}
	for _, id := range []string{"a3", "a4"} {
		if _, err := r.Get(ctx, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := r.Get(ctx, "a0"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a0 not purged: %v", err)
	}
}
