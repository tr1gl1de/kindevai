package app

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// overQuota is a session store's quota check a test flips.
type overQuota struct {
	mu     sync.Mutex
	reason string
	calls  int
}

func (o *overQuota) check() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	return o.reason
}

func (o *overQuota) set(reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reason = reason
}

func (o *overQuota) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

// KIN-115 AC: while an attempt runs, its sandbox's session store is checked; once it is over its quota the attempt
// is stopped (SIGTERM first) and its failure says why.
func TestAttemptOverItsSessionStoreQuotaIsStopped(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	l.termExit = true
	q := &overQuota{}
	x.Egress = &fakeConfiner{box: &fakeSandbox{}, quota: q.check}
	x.QuotaEvery = 5 * time.Millisecond
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "checked while it runs", func() bool { return q.count() >= 2 })
	if slices.Contains(ev.kinds(), protocol.EventFailed) {
		t.Fatalf("stopped within its quota: %v", ev.kinds())
	}
	q.set("session store of the task is over its quota (300.0 MiB > 256.0 MiB)")
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	f := failure(t, ev.last())
	if !strings.Contains(f.Reason, "over its quota (300.0 MiB > 256.0 MiB)") || f.Code != "" {
		t.Fatalf("failure %+v", f)
	}
	if s := l.proc(0).sent(); len(s) == 0 || s[0] {
		t.Fatalf("signals %v, want SIGTERM first", s)
	}
}

// KIN-115 AC: the check of an attempt within its quota stops with the attempt.
func TestQuotaCheckEndsWithTheAttempt(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	q := &overQuota{}
	x.Egress = &fakeConfiner{box: &fakeSandbox{}, quota: q.check}
	x.QuotaEvery = 2 * time.Millisecond
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "checked", func() bool { return q.count() >= 2 })
	l.proc(0).end(domain.Exit{})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	within(t, "the check's end", x.KillAll) // waits for the executor's goroutines, the check among them
	n := q.count()
	time.Sleep(20 * time.Millisecond)
	if m := q.count(); m != n {
		t.Fatalf("still checked after the attempt ended: %d → %d", n, m)
	}
}

// KIN-115 AC: an attempt adopted after a runner restart is watched again; its journal record keeps its task (the
// confiner finds the task's store by it).
func TestAdoptedAttemptIsWatchedForItsQuota(t *testing.T) {
	t.Parallel()
	live, j, ev, rec := restartStrict(t)
	if rec.Task != "T1" {
		t.Fatalf("record task %q, want T1", rec.Task)
	}
	q := &overQuota{reason: "session store of the task is over its quota (20001 entries > 20000)"}
	c := &fakeConfiner{reattach: &fakeSandbox{endpoint: rec.Sandbox.Endpoint}, quota: q.check}
	x2 := &Executor{Launcher: &fakeLauncher{adopted: map[int]*fakeProc{live.pid: live}}, Journal: j, Events: ev,
		Egress: c, Agents: &fakeAgents{}, Grace: 20 * time.Millisecond, QuotaEvery: 5 * time.Millisecond}
	if err := x2.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stopped", func() bool { return len(live.sent()) > 0 })
	live.end(domain.Exit{Signaled: true})
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	if f := failure(t, ev.last()); !strings.Contains(f.Reason, "20001 entries") {
		t.Fatalf("failure %+v", f)
	}
}
