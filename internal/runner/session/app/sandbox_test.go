package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

type fakeSandbox struct {
	mu       sync.Mutex
	released int
	endpoint string
	fault    chan string
	summary  string
}

func (s *fakeSandbox) Endpoint() string     { return s.endpoint }
func (s *fakeSandbox) Fault() <-chan string { return s.fault }
func (s *fakeSandbox) Summary() string      { return s.summary }
func (s *fakeSandbox) Profile() string      { return "fp-1" }

func (s *fakeSandbox) Command([]string, []string, string) (*exec.Cmd, error) {
	return nil, errors.New("unused")
}
func (s *fakeSandbox) Await(context.Context) error { return nil }
func (s *fakeSandbox) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released++
}

func (s *fakeSandbox) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

type fakeConfiner struct {
	box *fakeSandbox
	err error
	got []domain.Assignment

	mu         sync.Mutex
	reattach   *fakeSandbox // what Reattach returns (nil: an error)
	reattached []domain.Record
}

func (c *fakeConfiner) Reattach(_ context.Context, rec domain.Record, _ string) (Sandbox, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reattached = append(c.reattached, rec)
	if c.reattach == nil {
		return nil, errors.New("the adapter socket directory is gone")
	}
	return c.reattach, nil
}

func (c *fakeConfiner) Confine(_ context.Context, a domain.Assignment, _, _ string) (Sandbox, error) {
	c.got = append(c.got, a)
	if c.err != nil {
		return nil, c.err
	}
	if c.box == nil {
		return nil, nil
	}
	return c.box, nil
}

func failure(t *testing.T, e event) protocol.Failure {
	t.Helper()
	var f protocol.Failure
	if err := json.Unmarshal([]byte(e.body), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// KIN-31: strict, but no verified sandbox: nothing is launched and the failure says "network" (the step waits for
// the network, no unconfined fallback).
func TestStrictWithoutSandboxNeverLaunches(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	x.Egress = &fakeConfiner{err: errors.New("strict_egress_unsupported: unsupported on darwin")}
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	if l.count() != 0 {
		t.Fatal("an attempt was launched without its strict egress sandbox")
	}
	if k := ev.kinds(); !slices.Equal(k, []string{protocol.EventAccepted, protocol.EventFailed}) {
		t.Fatalf("events %v", k)
	}
	if f := failure(t, ev.last()); f.Code != protocol.FailureNetwork {
		t.Fatalf("failure %+v, want code network", f)
	}
	if done, _ := j.Finished("S1", 1); !done {
		t.Fatal("the refused attempt was not finished")
	}
}

// A sandbox whose preflight fails inside the launcher is a network failure too, and is released.
func TestUnverifiedSandboxIsANetworkFailure(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	box := &fakeSandbox{}
	x.Egress = &fakeConfiner{box: box}
	l.err = fmt.Errorf("%w: egress_leak: dns reached 1.1.1.1:53", ErrNetwork)
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	if f := failure(t, ev.last()); f.Code != protocol.FailureNetwork {
		t.Fatalf("failure %+v, want code network", f)
	}
	if box.count() != 1 {
		t.Fatalf("sandbox released %d times", box.count())
	}
}

func TestConfinedAttemptRunsInItsSandbox(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	box := &fakeSandbox{}
	c := &fakeConfiner{box: box}
	x.Egress = c
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	if len(c.got) != 1 || c.got[0].Project != "P1" {
		t.Fatalf("confined %+v", c.got)
	}
	if s := l.launched[0]; s.Sandbox != box || s.PermitBypass() != nil {
		t.Fatalf("launch spec %+v", s)
	}
	if box.count() != 0 {
		t.Fatal("sandbox released while the attempt runs")
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	waitFor(t, "sandbox released", func() bool { return box.count() == 1 })
}

// Owner decision (KB KIN-A-11/KIN-A-21): a permission bypass only inside a verified strict egress sandbox.
func TestPermitBypassOnlyInASandbox(t *testing.T) {
	t.Parallel()
	if err := (LaunchSpec{}).PermitBypass(); !errors.Is(err, ErrBypassUnconfined) {
		t.Fatalf("unconfined PermitBypass = %v", err)
	}
	if err := (LaunchSpec{Sandbox: &fakeSandbox{}}).PermitBypass(); err != nil {
		t.Fatalf("confined PermitBypass = %v", err)
	}
}

// Not strict (Confine returns no sandbox): the attempt runs as before, and may not bypass permissions.
func TestUnconfinedAttemptRunsWithoutBypass(t *testing.T) {
	t.Parallel()
	x, l, _, _ := newExecutor(domain.Policy{})
	x.Egress = &fakeConfiner{}
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	if s := l.launched[0]; s.Sandbox != nil || !errors.Is(s.PermitBypass(), ErrBypassUnconfined) {
		t.Fatalf("launch spec %+v", s)
	}
	x.KillAll()
}
