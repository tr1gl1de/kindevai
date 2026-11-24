package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

func leaseRow(t *testing.T, dir string) (lease, bool) {
	t.Helper()
	d, err := db.Open(t.Context(), DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v string
	if err := d.QueryRowContext(t.Context(), "SELECT value FROM meta WHERE key = ?", metaInstanceLock).Scan(&v); err != nil {
		return lease{}, false
	}
	var l lease
	if err := json.Unmarshal([]byte(v), &l); err != nil {
		t.Fatal(err)
	}
	return l, true
}

func writeLease(t *testing.T, dir string, l lease) {
	t.Helper()
	d, err := db.Open(t.Context(), DBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	b, _ := json.Marshal(l)
	if _, err := d.ExecContext(t.Context(), `INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaInstanceLock, string(b)); err != nil {
		t.Fatal(err)
	}
}

// KIN-41: besides the lock file the running instance holds a record in the database (host, pid, heartbeat); it is
// released on a clean shutdown.
func TestInstanceRecordHeldWhileRunning(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	inst, err := Run(t.Context(), Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	l, ok := leaseRow(t, dir)
	if !ok || l.Holder == "" || l.PID == 0 || l.Host == "" || l.HeartbeatAt.IsZero() {
		t.Fatalf("lease %+v %v", l, ok)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := leaseRow(t, dir); ok {
		t.Fatal("lease kept after Close")
	}
}

// A live record of another instance (for example on another host sharing the data over a network file system,
// where the lock file is not reliable) stops a second instance; a stale one (crash) is taken over.
func TestLiveInstanceRecordBlocksSecondInstance(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	inst := mustRun(t, dir)
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeLease(t, dir, lease{Holder: "other", Host: "elsewhere", PID: 4242, StartedAt: now, HeartbeatAt: now})
	_, err := Run(t.Context(), Options{DataDir: dir})
	if !errors.Is(err, ErrInstanceActive) || !strings.Contains(err.Error(), "elsewhere") {
		t.Fatalf("second instance: %v", err)
	}
	old := now.Add(-time.Hour)
	writeLease(t, dir, lease{Holder: "other", Host: "elsewhere", PID: 4242, StartedAt: old, HeartbeatAt: old})
	inst = mustRun(t, dir)
	if l, _ := leaseRow(t, dir); l.Holder == "other" {
		t.Fatal("stale lease not taken over")
	}
	_ = inst
}

// The running instance renews its heartbeat, so a long-running Control Plane never looks stale.
func TestInstanceRecordHeartbeat(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	inst, err := Run(t.Context(), Options{DataDir: dir, LeaseTTL: 60 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	first, _ := leaseRow(t, dir)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if l, _ := leaseRow(t, dir); l.HeartbeatAt.After(first.HeartbeatAt) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("heartbeat not renewed")
}
