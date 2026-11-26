package nsbox

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// Socket directories of killed launchers are swept; a fresh one or one still served is kept.
//
// KIN-98: a fresh directory whose socket refuses connections is kept too: net.Listen binds the socket file before
// it listens, and a sweep by a parallel Open in that gap took the new attempt's directory for an abandoned one.
//
// Not parallel: the "dead" socket's listener fd lives in this process, and a parallel test that forks a sandbox
// helper holds a copy of every fd while the child waits for its uid_map; a connect would then still succeed.
func TestSweepStale(t *testing.T) {
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
	stale := mk("kindev-egress-stale", 2*staleAge, false) // a dead socket, not touched since: abandoned
	served := mk("kindev-egress-served", time.Hour, true)
	empty := filepath.Join(base, "kindev-egress-empty") // no socket yet: kept while young
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	binding := filepath.Join(base, "kindev-egress-binding") // bound, not listening yet: kept while young
	if err := os.Mkdir(binding, 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: filepath.Join(binding, "s")}); err != nil {
		t.Fatal(err)
	}
	removed := sweepStale(base, time.Now(), nil)
	if len(removed) != 1 || removed[0] != stale {
		t.Fatalf("removed %v, want only %s", removed, stale)
	}
	got := sweepStale(base, time.Now().Add(staleAge), nil)
	slices.Sort(got)
	if want := []string{binding, empty}; !slices.Equal(got, want) {
		t.Fatalf("later sweep removed %v, want %v", got, want)
	}
	for _, d := range []string{served} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("%s removed: %v", filepath.Base(d), err)
		}
	}
}
