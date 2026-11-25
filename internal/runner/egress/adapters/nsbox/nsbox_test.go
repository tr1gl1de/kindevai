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
	stale := mk("kindev-egress-stale", 0, false) // a dead socket: abandoned whatever its age
	served := mk("kindev-egress-served", time.Hour, true)
	empty := filepath.Join(base, "kindev-egress-empty") // no socket yet: kept while young
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	removed := sweepStale(base, time.Now())
	if len(removed) != 1 || removed[0] != stale {
		t.Fatalf("removed %v, want only %s", removed, stale)
	}
	if got := sweepStale(base, time.Now().Add(staleAge)); len(got) != 1 || got[0] != empty {
		t.Fatalf("later sweep removed %v, want the empty directory", got)
	}
	for _, d := range []string{served} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s removed: %v", filepath.Base(d), err)
		}
	}
}
