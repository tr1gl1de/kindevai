package nsbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Socket directories of killed launchers are swept; a fresh one or one still served is kept.
func TestSweepStale(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mk := func(name string, age time.Duration, serve bool) string {
		d := filepath.Join(base, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		var lc net.ListenConfig
		l, err := lc.Listen(context.Background(), "unix", filepath.Join(d, "s"))
		if err != nil {
			t.Fatal(err)
		}
		if serve {
			t.Cleanup(func() { _ = l.Close() })
		} else {
			ul := l.(*net.UnixListener)
			ul.SetUnlinkOnClose(false) // a killed process leaves its socket file behind
			_ = l.Close()
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
		return d
	}
	stale := mk("kindev-egress-stale", time.Hour, false)
	fresh := mk("kindev-egress-fresh", 0, false)
	served := mk("kindev-egress-served", time.Hour, true)
	sweepStale(base, time.Now())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale directory kept: %v", err)
	}
	for _, d := range []string{fresh, served} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s removed: %v", filepath.Base(d), err)
		}
	}
}
