package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// slowWorkspaces is a workspace manager whose preparation hangs until released (a large first clone).
func slowWorkspaces() *fakeWorkspaces {
	return &fakeWorkspaces{gate: make(chan struct{}), started: make(chan string, 4)}
}

func (ev *fakeEvents) of(kind string) []event {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	var out []event
	for _, e := range ev.evs {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// KIN-71 AC: the preparation runs in the background (the attempt is preparing); the command is acknowledged as soon
// as the attempt is recorded in the journal.
func TestAssignIsAcknowledgedWhileTheWorkspaceIsPrepared(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	x.Workspaces = ws
	done := make(chan error, 1)
	go func() { done <- x.Command(t.Context(), assignCommand(t, repoAssignment())) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the assignment blocked the command channel while the workspace was prepared")
	}
	<-ws.started
	recs, _ := j.List()
	if len(recs) != 1 || !recs[0].Preparing || recs[0].PID != 0 {
		t.Fatalf("journal %+v", recs)
	}
	if k := ev.kinds(); !slices.Equal(k, []string{protocol.EventAccepted, protocol.EventWorkspacePreparing}) {
		t.Fatalf("events %v", k)
	}
	if a := x.Attempts(); len(a) != 1 || !a[0].Running || a[0].PID != 0 {
		t.Fatalf("attempts %+v", a)
	}
	// Commands keep flowing: a repeated assignment is a no-op, a cancel of another attempt returns at once.
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	x.Cancel("other", 1, "")
	close(ws.gate)
	waitFor(t, "launch", func() bool { return l.count() == 1 })
	if n := len(ws.reqs); n != 1 {
		t.Fatalf("%d preparations", n)
	}
	x.KillAll()
}

func assignCommand(t *testing.T, a domain.Assignment) protocol.Command {
	t.Helper()
	b, err := json.Marshal(protocol.Assign{Step: a.Step, Fence: a.Fence, Execution: a.Execution, Task: a.Task,
		Project: a.Project, Repository: a.Repository, Adapter: a.Adapter, Scenario: a.Scenario, Base: a.Base})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Command{ID: "assign/S1/1", Seq: 1, Kind: protocol.CommandAssign, Body: b}
}

// KIN-71 AC: a cancel during the preparation stops git (its context) and frees the workspace; the attempt fails as
// cancelled and never launches.
func TestCancelDuringPreparationStopsItAndFreesTheWorkspace(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-ws.started
	x.Cancel("S1", 1, "user")
	waitFor(t, "failed", func() bool { return len(ev.of(protocol.EventFailed)) == 1 })
	if f := ev.of(protocol.EventFailed)[0]; !strings.Contains(f.body, "cancelled: user") {
		t.Fatalf("failure %s", f.body)
	}
	waitFor(t, "finished", func() bool { done, _ := j.Finished("S1", 1); return done })
	x.wg.Wait()
	if l.count() != 0 || !slices.Equal(ws.done(), []string{"S1-1"}) || len(x.Attempts()) != 0 {
		t.Fatalf("launched %d, released %v, attempts %+v", l.count(), ws.done(), x.Attempts())
	}
}

// KIN-71: a preparation that takes longer than PrepareTimeout fails the attempt with a clear reason.
func TestPreparationTimesOut(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	x.Workspaces, x.PrepareTimeout = slowWorkspaces(), 30*time.Millisecond
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return len(ev.of(protocol.EventFailed)) == 1 })
	if f := ev.of(protocol.EventFailed)[0]; !strings.Contains(f.body, "workspace preparation timed out after 30ms") {
		t.Fatalf("failure %s", f.body)
	}
	waitFor(t, "finished", func() bool { done, _ := j.Finished("S1", 1); return done })
	if l.count() != 0 {
		t.Fatal("launched")
	}
	newer := repoAssignment() // the timed-out attempt has ended: a newer one is not held up
	newer.Fence = 2
	x.PrepareTimeout = time.Hour
	close(x.Workspaces.(*fakeWorkspaces).gate)
	done := make(chan error, 1)
	go func() { done <- x.Assign(t.Context(), newer) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the newer attempt waited for the timed-out one")
	}
	waitFor(t, "launch", func() bool { return l.count() == 1 })
	x.KillAll()
}

// KIN-71 AC: a runner restart during the preparation stops it; the next runner process fails the attempt with a
// clear reason and frees the workspace.
func TestRestartDuringPreparationFailsTheAttempt(t *testing.T) {
	t.Parallel()
	x, _, j, ev := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-ws.started
	x.Close()
	x.wg.Wait() // the preparation stopped with the executor
	if n := len(ev.of(protocol.EventFailed)); n != 0 {
		t.Fatalf("%d failures reported by a closed executor", n)
	}
	if recs, _ := j.List(); len(recs) != 1 {
		t.Fatalf("journal %+v", recs)
	}
	next := &Executor{Launcher: &fakeLauncher{}, Journal: j, Events: ev, Workspaces: ws}
	if err := next.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f := ev.of(protocol.EventFailed); len(f) != 1 || !strings.Contains(f[0].body, "workspace preparation interrupted by a runner restart") {
		t.Fatalf("failures %+v", f)
	}
	if done, _ := j.Finished("S1", 1); !done || !slices.Contains(ws.done(), "S1-1") {
		t.Fatalf("finished %v, released %v", done, ws.done())
	}
}

// KIN-71: the preparation's phases are reported, and repeated while it lasts (the Control Plane extends the claim's
// lease with them); nothing is reported once the workspace is prepared.
func TestPreparationProgressIsReported(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	ws.phases = []string{protocol.PreparingFetching}
	x.Workspaces, x.PrepareKeepalive = ws, 5*time.Millisecond
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-ws.started
	waitFor(t, "keepalives", func() bool { return len(ev.of(protocol.EventWorkspacePreparing)) >= 4 })
	close(ws.gate)
	waitFor(t, "launch", func() bool { return l.count() == 1 })
	evs := ev.of(protocol.EventWorkspacePreparing)
	var first, last protocol.WorkspacePreparing
	_ = json.Unmarshal([]byte(evs[0].body), &first)
	_ = json.Unmarshal([]byte(evs[len(evs)-1].body), &last)
	if first.Phase != protocol.PreparingQueued || last.Phase != protocol.PreparingFetching {
		t.Fatalf("phases %+v … %+v", first, last)
	}
	k := ev.kinds()
	if i, p := slices.Index(k, protocol.EventWorkspacePrepared), slices.Index(k, protocol.EventStarted); i < 0 || p < i ||
		slices.Contains(k[i:], protocol.EventWorkspacePreparing) {
		t.Fatalf("events %v", k)
	}
	x.KillAll()
}

// KIN-71: a newer token for the step stops an attempt still preparing its workspace (stale fence) before the new
// one is prepared.
func TestNewerTokenSupersedesAPreparingAttempt(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-ws.started
	newer := repoAssignment()
	newer.Fence = 2
	if err := x.Assign(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	if f := ev.of(protocol.EventFailed); len(f) != 1 || f[0].fence != 1 || !strings.Contains(f[0].body, protocol.CancelStaleFence) {
		t.Fatalf("failures %+v", f)
	}
	if got := <-ws.started; got != "S1-2" {
		t.Fatalf("preparing %s", got)
	}
	close(ws.gate)
	waitFor(t, "launch", func() bool { return l.count() == 1 })
	if s := l.launched[0]; s.Attempt != 2 {
		t.Fatalf("launched %+v", s)
	}
	x.KillAll()
}

// KIN-71: StopAll (runner disabled or revoked) also stops attempts still preparing, and waits for them.
func TestStopAllStopsPreparingAttempts(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-ws.started
	x.StopAll(protocol.CancelRunnerDisabled)
	if f := ev.of(protocol.EventFailed); len(f) != 1 || !strings.Contains(f[0].body, protocol.CancelRunnerDisabled) {
		t.Fatalf("failures %+v", f)
	}
	if l.count() != 0 {
		t.Fatal("launched")
	}
}

// KIN-71: KillAll does not wait for a preparation to finish on its own.
func TestKillAllStopsPreparingAttempts(t *testing.T) {
	t.Parallel()
	x, _, _, _ := newExecutor(domain.Policy{})
	ws := slowWorkspaces()
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	<-ws.started
	done := make(chan struct{})
	go func() { x.KillAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("KillAll waited for the preparation")
	}
}
