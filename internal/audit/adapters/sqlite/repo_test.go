package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/audit/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
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
	return d
}

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func event(t *testing.T, minutes int, actorID, login, kind, project string) domain.Event {
	t.Helper()
	e, err := domain.NewEvent(t0.Add(time.Duration(minutes)*time.Minute), domain.Actor{Type: domain.ActorUser, ID: actorID, Label: login},
		kind, project, "user:x", map[string]string{"role": "member"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func ids(es []domain.Event) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func TestAppendAndQueryFilters(t *testing.T) {
	t.Parallel()
	r := New(testDB(t))
	ctx := t.Context()
	seed := []domain.Event{
		event(t, 0, "u1", "owner", "user.created", ""),          // 1
		event(t, 1, "u1", "owner", "invitation.created", "p1"),  // 2
		event(t, 2, "u2", "carol", "invitation.accepted", "p1"), // 3
		event(t, 3, "u1", "owner", "invitation.created", "p2"),  // 4
		event(t, 4, "u1", "owner", "user.disabled", ""),         // 5
	}
	for i, e := range seed {
		id, err := r.Append(ctx, e)
		if err != nil || id != int64(i+1) {
			t.Fatalf("Append #%d = %d, %v", i, id, err)
		}
	}
	all, err := r.Query(ctx, domain.Filter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); !slices.Equal(got, []int64{5, 4, 3, 2, 1}) {
		t.Fatalf("all = %v", got)
	}
	first := all[4]
	if !first.At.Equal(t0) || first.Actor.Label != "owner" || first.Kind != "user.created" || first.ProjectID != "" ||
		first.Subject != "user:x" || first.Details["role"] != "member" {
		t.Fatalf("round trip = %+v", first)
	}

	for _, tc := range []struct {
		name string
		f    domain.Filter
		want []int64
	}{
		{"project p1 only", domain.Filter{ProjectID: "p1"}, []int64{3, 2}},
		{"other project not leaked", domain.Filter{ProjectID: "p2"}, []int64{4}},
		{"actor by id", domain.Filter{Actor: "u2"}, []int64{3}},
		{"actor by login", domain.Filter{Actor: "owner"}, []int64{5, 4, 2, 1}},
		{"exact kind", domain.Filter{Kind: "invitation.created"}, []int64{4, 2}},
		{"kind namespace", domain.Filter{Kind: "invitation"}, []int64{4, 3, 2}},
		{"namespace is not a string prefix", domain.Filter{Kind: "user.dis"}, nil},
		{"period from inclusive to exclusive", domain.Filter{From: t0.Add(time.Minute), To: t0.Add(3 * time.Minute)}, []int64{3, 2}},
		{"cursor", domain.Filter{Before: 3}, []int64{2, 1}},
		{"limit", domain.Filter{Limit: 2}, []int64{5, 4}},
		{"combined", domain.Filter{Actor: "owner", ProjectID: "p1", Kind: "invitation"}, []int64{2}},
	} {
		if tc.f.Limit == 0 {
			tc.f.Limit = 50
		}
		got, err := r.Query(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if g := ids(got); len(g)+len(tc.want) > 0 && !slices.Equal(g, tc.want) {
			t.Errorf("%s: ids = %v, want %v", tc.name, g, tc.want)
		}
	}
	kinds, err := r.Kinds(ctx)
	if err != nil || !slices.Equal(kinds, []string{"invitation.accepted", "invitation.created", "user.created", "user.disabled"}) {
		t.Fatalf("Kinds = %v, %v", kinds, err)
	}
}

func TestEventsAreAppendOnly(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := New(d)
	if _, err := r.Append(t.Context(), event(t, 0, "u1", "owner", "user.created", "")); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"UPDATE audit_events SET kind = 'user.deleted'",
		"DELETE FROM audit_events",
	} {
		if _, err := d.ExecContext(t.Context(), stmt); err == nil {
			t.Errorf("%q succeeded on the append-only table", stmt)
		}
	}
	if got, _ := r.Query(t.Context(), domain.Filter{Limit: 10}); len(got) != 1 || got[0].Kind != "user.created" {
		t.Fatalf("events changed: %+v", got)
	}
}

func TestAppendJoinsTheCallersTransaction(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := New(d)
	tx := db.Transactor{DB: d}
	boom := errors.New("action failed")
	err := tx.InTx(t.Context(), func(ctx context.Context) error {
		if _, err := r.Append(ctx, event(t, 0, "u1", "owner", "user.created", "")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v", err)
	}
	if got, _ := r.Query(t.Context(), domain.Filter{Limit: 10}); len(got) != 0 {
		t.Fatalf("event of a rolled-back action was kept: %+v", got)
	}
	if err := tx.InTx(t.Context(), func(ctx context.Context) error {
		_, err := r.Append(ctx, event(t, 0, "u1", "owner", "user.created", ""))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Query(t.Context(), domain.Filter{Limit: 10}); len(got) != 1 {
		t.Fatalf("committed event missing: %+v", got)
	}
}
