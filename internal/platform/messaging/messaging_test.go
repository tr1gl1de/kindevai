package messaging_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// clock is a manually advanced test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// openDB opens a migrated database file; reopening the same path simulates a process restart.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := db.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	return d
}

func newDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	return openDB(t, path), path
}

// config is a deterministic configuration: fixed clock, jitter at the midpoint.
func config(c *clock) messaging.Config {
	return messaging.Config{Now: c.Now, Jitter: func() float64 { return 0.5 }}
}

func count(t *testing.T, d *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// recorder is a fake external system that records the idempotency keys it received.
type recorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *recorder) deliver(_ context.Context, d messaging.Delivery) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, d.Key)
	return nil
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}
