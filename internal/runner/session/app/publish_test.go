package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

type fakePusher struct {
	*fakeWorkspaces
	pushes []PushRequest
	err    error
	hold   chan struct{} // when set, a push waits until it is closed or its context ends
}

func (p *fakePusher) Push(ctx context.Context, r PushRequest) (protocol.WorkspacePushed, error) {
	if p.hold != nil {
		select {
		case <-p.hold:
		case <-ctx.Done():
			return protocol.WorkspacePushed{}, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, r)
	if p.err != nil {
		return protocol.WorkspacePushed{}, p.err
	}
	return protocol.WorkspacePushed{Repository: r.Repository, Branch: "feature/" + r.Task + "--" + r.Step, HeadSHA: r.Head}, nil
}

const reviewedHead = "0123456789abcdef0123456789abcdef01234567"

func publishAssignment() domain.Assignment {
	a := repoAssignment()
	a.Adapter, a.Role, a.Scenario, a.DiffHead = domain.AdapterPublish, domain.RolePublisher, "", reviewedHead
	return a
}

// KIN-36: a publish assignment launches no process: the runner pushes the reviewed head with the assignment's
// token, reports workspace.pushed and completes; a repeated command does not push again.
func TestPublishPushesTheReviewedHeadWithoutAProcess(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	ws := &fakePusher{fakeWorkspaces: &fakeWorkspaces{}}
	x.Workspaces = ws
	if err := x.Assign(t.Context(), publishAssignment()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	want := []string{protocol.EventAccepted, protocol.EventStarted, protocol.EventWorkspacePreparing, protocol.EventWorkspacePushed,
		protocol.EventCompleted}
	if k := ev.kinds(); !slices.Equal(k, want) {
		t.Fatalf("events %v", k)
	}
	if l.count() != 0 || len(ws.reqs) != 0 {
		t.Fatal("a process or worktree was started")
	}
	if r := ws.pushes[0]; r.Head != reviewedHead || r.Task != "T1" || r.Step != "S1" || r.Repository != "acme/app" ||
		r.Token.Reveal() != "ghs_tok" {
		t.Fatalf("push %+v", r)
	}
	for _, e := range ev.evs {
		if strings.Contains(e.body, "ghs_tok") {
			t.Fatalf("token in event %s", e.kind)
		}
	}
	if done, _ := j.Finished("S1", 1); !done {
		t.Fatal("attempt not finished")
	}
	if err := x.Assign(t.Context(), publishAssignment()); err != nil || len(ws.pushes) != 1 {
		t.Fatalf("repeated: %v, %d pushes", err, len(ws.pushes))
	}
}

// KIN-36: a rejected push fails the attempt with the (redacted) reason; nothing is reported as pushed.
func TestRejectedPushFailsTheAttempt(t *testing.T) {
	t.Parallel()
	x, _, _, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakePusher{fakeWorkspaces: &fakeWorkspaces{}, err: errors.New("non-fast-forward for ghs_tok")}
	if err := x.Assign(t.Context(), publishAssignment()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	if e := ev.last(); !strings.Contains(e.body, "non-fast-forward") || strings.Contains(e.body, "ghs_tok") ||
		slices.Contains(ev.kinds(), protocol.EventWorkspacePushed) {
		t.Fatalf("%v %+v", ev.kinds(), e)
	}
}

// A runner whose workspace manager cannot push fails a publish assignment.
func TestPublishWithoutAPusherFails(t *testing.T) {
	t.Parallel()
	x, _, _, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{}
	if err := x.Assign(t.Context(), publishAssignment()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
}

// A slow push keeps the claim alive (workspace.preparing, which renews the lease) and a cancel stops it: the attempt
// fails as cancelled and nothing is reported as pushed.
func TestASlowPushKeepsTheClaimAliveAndCanBeCancelled(t *testing.T) {
	t.Parallel()
	x, _, j, ev := newExecutor(domain.Policy{})
	ws := &fakePusher{fakeWorkspaces: &fakeWorkspaces{}, hold: make(chan struct{})}
	x.Workspaces, x.PrepareKeepalive = ws, 5*time.Millisecond
	if err := x.Assign(t.Context(), publishAssignment()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "keepalives", func() bool {
		n := 0
		for _, k := range ev.kinds() {
			if k == protocol.EventWorkspacePreparing {
				n++
			}
		}
		return n >= 3
	})
	x.Cancel("S1", 1, "cancelled")
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	if e := ev.last(); !strings.Contains(e.body, "cancelled") || slices.Contains(ev.kinds(), protocol.EventWorkspacePushed) {
		t.Fatalf("%v %+v", ev.kinds(), e)
	}
	waitFor(t, "finished", func() bool { done, _ := j.Finished("S1", 1); return done })
}

// A runner restart during a push fails the attempt from its journal record (the Control Plane assigns it again).
func TestARestartDuringAPushFailsTheAttempt(t *testing.T) {
	t.Parallel()
	x, _, j, ev := newExecutor(domain.Policy{})
	ws := &fakePusher{fakeWorkspaces: &fakeWorkspaces{}, hold: make(chan struct{})}
	x.Workspaces = ws
	if err := x.Assign(t.Context(), publishAssignment()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "started", func() bool { return slices.Contains(ev.kinds(), protocol.EventStarted) })
	x.Close()
	if slices.Contains(ev.kinds(), protocol.EventFailed) {
		t.Fatal("the stopping runner reported the push")
	}
	next, _, _, ev2 := newExecutor(domain.Policy{})
	next.Journal = j
	next.Workspaces = &fakePusher{fakeWorkspaces: &fakeWorkspaces{}}
	if err := next.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e := ev2.last(); e.kind != protocol.EventFailed || !strings.Contains(e.body, "interrupted") {
		t.Fatalf("%v %+v", ev2.kinds(), e)
	}
}
