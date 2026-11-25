package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// restartStrict runs a strict attempt, stops the executor (the runner restarts; the process keeps running) and
// returns the live process and its journal record.
func restartStrict(t *testing.T) (*fakeProc, *memJournal, *fakeEvents, domain.Record) {
	t.Helper()
	x, l, j, ev := newExecutor(domain.Policy{})
	l.termExit = true
	x.Egress = &fakeConfiner{box: &fakeSandbox{endpoint: "/run/user/1000/kindev-egress-1/s"}}
	x.Agents = &fakeAgents{}
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	x.Close()
	recs, _ := j.List()
	if len(recs) != 1 {
		t.Fatalf("journal %+v", recs)
	}
	return l.proc(0), j, ev, recs[0]
}

// KIN-81: the journal record of a strict attempt names its sandbox's way out (the adapter socket) and the CLI role
// its profile was chosen for, so a restarted runner can serve it again.
func TestStrictAttemptRecordsItsSandbox(t *testing.T) {
	t.Parallel()
	_, _, _, rec := restartStrict(t)
	if rec.Sandbox == nil || rec.Sandbox.Endpoint != "/run/user/1000/kindev-egress-1/s" || rec.Sandbox.Role != domain.AgentClaude {
		t.Fatalf("record sandbox %+v", rec.Sandbox)
	}
}

// After a restart an adopted strict attempt gets its proxy adapter back on the same socket and runs on.
func TestRecoverReattachesTheSandbox(t *testing.T) {
	t.Parallel()
	live, j, ev, rec := restartStrict(t)
	box := &fakeSandbox{endpoint: rec.Sandbox.Endpoint}
	c := &fakeConfiner{reattach: box}
	x2 := &Executor{Launcher: &fakeLauncher{adopted: map[int]*fakeProc{live.pid: live}}, Journal: j, Events: ev,
		Egress: c, Agents: &fakeAgents{}}
	if err := x2.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(c.reattached) != 1 || c.reattached[0].Sandbox.Endpoint != rec.Sandbox.Endpoint || c.reattached[0].Project != "P1" {
		t.Fatalf("reattached %+v", c.reattached)
	}
	if s := live.sent(); len(s) != 0 {
		t.Fatalf("the reattached attempt was signalled: %v", s)
	}
	live.end(domain.Exit{Code: 0})
	waitFor(t, "sandbox released", func() bool { return box.count() == 1 })
	waitFor(t, "journal cleared", func() bool { r, _ := j.List(); return len(r) == 0 })
}

// An adapter that cannot be restored (or a runner without strict egress) stops the attempt: it has no way out, so
// it ends with code network (the step waits for the network) instead of hanging until the CLI's timeout.
func TestRecoverStopsAStrictAttemptWithoutItsAdapter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		egress Confiner
	}{
		{"reattach fails", &fakeConfiner{}},
		{"no strict egress", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			live, j, ev, _ := restartStrict(t)
			x2 := &Executor{Launcher: &fakeLauncher{adopted: map[int]*fakeProc{live.pid: live}}, Journal: j, Events: ev,
				Egress: tc.egress, Agents: &fakeAgents{}, Grace: 20 * time.Millisecond}
			if err := x2.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
			f := failure(t, ev.last())
			if f.Code != protocol.FailureNetwork || !strings.Contains(f.Reason, "restart") {
				t.Fatalf("failure %+v, want code network", f)
			}
			if s := live.sent(); len(s) == 0 {
				t.Fatal("the attempt without an adapter was not stopped")
			}
			waitFor(t, "journal cleared", func() bool { r, _ := j.List(); return len(r) == 0 })
		})
	}
}

// KIN-81: the sandbox reports at runtime that its egress is no longer the verified one (the adapter already cut
// every connection): the attempt is stopped and ends with code network.
func TestSandboxFaultStopsTheAttempt(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	l.termExit = true
	box := &fakeSandbox{fault: make(chan string, 1)}
	x.Egress = &fakeConfiner{box: box}
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	box.fault <- "egress_ip_mismatch"
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	f := failure(t, ev.last())
	if f.Code != protocol.FailureNetwork || !strings.Contains(f.Reason, "egress_ip_mismatch") {
		t.Fatalf("failure %+v, want code network with the check's code", f)
	}
	if s := l.proc(0).sent(); len(s) == 0 || s[0] {
		t.Fatalf("signals %v, want SIGTERM first", s)
	}
	waitFor(t, "sandbox released", func() bool { return box.count() == 1 })
}

// Connections the allowlist denied are reported in the attempt's output before its end.
func TestDeniedConnectionsAreReported(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	box := &fakeSandbox{summary: "strict egress: 2 connection(s) denied by the allowlist (last: evil.test:443)"}
	x.Egress = &fakeConfiner{box: box}
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	ev.mu.Lock()
	defer ev.mu.Unlock()
	i := slices.IndexFunc(ev.evs, func(e event) bool {
		return e.kind == protocol.EventOutput && strings.Contains(e.body, "denied by the allowlist")
	})
	if i < 0 || ev.evs[len(ev.evs)-1].kind != protocol.EventCompleted {
		t.Fatalf("events %+v", ev.evs)
	}
}
