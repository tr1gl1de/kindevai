package main

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/nsbox"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/journal"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// KIN-100: the strict egress sandboxes of the daemon (and of `ai-runner network`, which runs the self-test on the
// same data dir) keep the adapter directories of the attempts in the journal, so a sweep never removes a sandbox
// before a restarted runner reattaches it; a finished attempt's directory may be swept again.
func TestStrictEgressKeepsJournalledSandboxes(t *testing.T) {
	t.Parallel()
	d, err := datadir.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	j := journal.Dir{Path: attemptsDir(d.Path())}
	const sock = "/run/user/1000/kindev-egress-123/s"
	for _, r := range []sessiondomain.Record{
		{Step: "step-1", Fence: 1, PID: 10, Sandbox: &sessiondomain.SandboxRef{Endpoint: sock}},
		{Step: "step-2", Fence: 1, PID: 11}, // not sandboxed
		{Step: "step-3", Fence: 2, PID: 12, Sandbox: &sessiondomain.SandboxRef{Endpoint: "/tmp/kindev-egress-9/s"}},
	} {
		if err := j.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Finish("step-3", 2); err != nil {
		t.Fatal(err)
	}
	m, ok := strictEgress(d, "/bin/true", slog.New(slog.DiscardHandler)).Boxes.(*nsbox.Manager)
	if !ok || m.InUse == nil {
		t.Fatal("the strict egress manager does not know the journalled sandboxes")
	}
	got, err := m.InUse()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{sock}) {
		t.Fatalf("InUse = %v, want only the running attempt's adapter %s", got, sock)
	}
}
