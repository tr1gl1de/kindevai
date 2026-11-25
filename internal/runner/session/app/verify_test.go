package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// verifyingExecutor runs attempts in a worktree (in the background, as CLI attempts do) inside a strict egress
// sandbox that verifies only when the test sends its verdict.
func verifyingExecutor(t *testing.T) (*Executor, *fakeLauncher, *fakeEvents, *fakeSandbox) {
	t.Helper()
	x, l, _, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{}
	sb := &fakeSandbox{verifying: make(chan struct{}, 4), verdict: make(chan error, 4)}
	x.Egress = &fakeConfiner{box: sb}
	l.termExit = true
	t.Cleanup(func() { // a failed test may leave a sandbox verifying: end it, then the attempts
		for range cap(sb.verdict) {
			select {
			case sb.verdict <- errors.New("the test ended"):
			default:
			}
		}
		x.KillAll()
	})
	return x, l, ev, sb
}

// within fails the test unless fn returns within the bound (it would block on the executor's lock).
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s blocked while the sandbox verified", what)
	}
}

// KIN-104 AC: a cancel that arrives while the sandbox verifies reaches the sandbox at once (the helper then never
// starts the agent) and the attempt is reported as cancelled.
func TestCancelWhileTheSandboxVerifies(t *testing.T) {
	t.Parallel()
	x, l, ev, sb := verifyingExecutor(t)
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-sb.verifying
	within(t, "Cancel", func() { x.Cancel("S1", 1, "") })
	if n := sb.cancelled(); n != 1 {
		t.Fatalf("the sandbox was cancelled %d times while it verified, want 1", n)
	}
	sb.verdict <- nil // the helper reports its verdict, then exits without starting the agent
	waitFor(t, "failed", func() bool { return len(ev.of(protocol.EventFailed)) == 1 })
	if f := failure(t, ev.of(protocol.EventFailed)[0]); !strings.HasPrefix(f.Reason, "cancelled") || f.Code != "" {
		t.Fatalf("failure %+v, want cancelled", f)
	}
	if s := l.proc(0).sent(); len(s) == 0 || s[0] {
		t.Fatalf("signals %v, want SIGTERM first", s)
	}
	if slices.Contains(ev.kinds(), protocol.EventCompleted) {
		t.Fatalf("events %v", ev.kinds())
	}
}

// KIN-104 AC: a cancel during the verification is reported as cancelled even when the sandbox then fails to verify
// (its helper ended): not as a network failure, which would hold the step for the network.
func TestCancelWhileTheSandboxVerifiesThenFails(t *testing.T) {
	t.Parallel()
	x, _, ev, sb := verifyingExecutor(t)
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-sb.verifying
	within(t, "Cancel", func() { x.Cancel("S1", 1, "") })
	sb.verdict <- errors.New("no verdict from the sandbox (the helper ended)")
	waitFor(t, "failed", func() bool { return len(ev.of(protocol.EventFailed)) == 1 })
	if f := failure(t, ev.of(protocol.EventFailed)[0]); !strings.HasPrefix(f.Reason, "cancelled") || f.Code != "" {
		t.Fatalf("failure %+v, want cancelled", f)
	}
	waitFor(t, "run dropped", func() bool { return len(x.Attempts()) == 0 })
}

// KIN-104 AC: one attempt's sandbox verification blocks neither other attempts' commands nor the executor's state.
func TestSandboxVerificationBlocksNoOtherAttempt(t *testing.T) {
	t.Parallel()
	x, l, ev, sb := verifyingExecutor(t)
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-sb.verifying
	within(t, "Attempts", func() {
		if a := x.Attempts(); len(a) != 1 || !a[0].Running || a[0].PID != 0 {
			t.Errorf("attempts %+v", a)
		}
	})
	within(t, "a cancel of another attempt", func() { x.Cancel("other", 1, "") })
	other := repoAssignment()
	other.Step = "S2"
	within(t, "another assignment", func() {
		if err := x.Assign(t.Context(), other); err != nil {
			t.Error(err)
		}
	})
	<-sb.verifying // S2's sandbox verifies too
	sb.verdict <- nil
	sb.verdict <- nil
	waitFor(t, "both started", func() bool { return len(ev.of(protocol.EventStarted)) == 2 })
	if n := sb.cancelled(); n != 0 {
		t.Fatalf("an uncancelled sandbox was cancelled %d times", n)
	}
	l.proc(0).end(domain.Exit{})
	l.proc(1).end(domain.Exit{})
	waitFor(t, "completed", func() bool { return len(ev.of(protocol.EventCompleted)) == 2 })
}

// KillAll while a sandbox verifies: the sandbox is cancelled at once and the process is killed (SIGKILL, no grace)
// as soon as the launch knows it.
func TestKillAllWhileTheSandboxVerifies(t *testing.T) {
	t.Parallel()
	x, l, ev, sb := verifyingExecutor(t)
	l.termExit = false // only SIGKILL ends it
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-sb.verifying
	go func() { // KillAll waits for the launch; the helper reports its verdict meanwhile
		waitFor(t, "sandbox cancelled", func() bool { return sb.cancelled() == 1 })
		sb.verdict <- nil
	}()
	within(t, "KillAll", x.KillAll)
	if s := l.proc(0).sent(); len(s) == 0 || !s[0] { // later: finishWorkspace kills the group's leftovers
		t.Fatalf("signals %v, want SIGKILL first (no SIGTERM)", s)
	}
	if f := failure(t, ev.last()); ev.last().kind != protocol.EventFailed || !strings.HasPrefix(f.Reason, "cancelled") {
		t.Fatalf("last event %s %+v", ev.last().kind, f)
	}
}
