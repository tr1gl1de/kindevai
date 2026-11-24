package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/maintenance/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/maintenance/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

func TestModeRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(ctx, d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	s := sqlite.New(d)
	if m, err := s.Load(ctx); err != nil || m.Enabled {
		t.Fatalf("default %+v %v", m, err)
	}
	want := domain.Mode{Enabled: true, Reason: "move", By: "owner", Since: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	for range 2 { // insert, then update
		if err := s.Save(ctx, want); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load(ctx)
	if err != nil || got.Enabled != want.Enabled || got.Reason != want.Reason || got.By != want.By || !got.Since.Equal(want.Since) {
		t.Fatalf("got %+v %v", got, err)
	}
	if err := s.Save(ctx, domain.Disabled()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx); got.Enabled {
		t.Fatal("still enabled")
	}
}
