package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

type fakeWorkspaces struct {
	mu       sync.Mutex
	reqs     []WorkspaceRequest
	finished []string
	err      error
	interim  []string // the files Changes reports while the attempt runs
	checks   int      // Changes calls
	late     bool     // Changes was called after Finish
	finErr   error    // Finish fails
}

func (w *fakeWorkspaces) Changes(_ context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.checks++
	if slices.Contains(w.finished, jkey(step, fence)) {
		w.late = true
	}
	ch := &protocol.WorkspaceChanges{BaseSHA: "abc", FilesTotal: len(w.interim), Interim: true}
	for _, f := range w.interim {
		ch.Files = append(ch.Files, protocol.ChangedFile{Path: f, Status: "A"})
	}
	return ch, nil
}

func (w *fakeWorkspaces) setInterim(files ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.interim = files
}

func (w *fakeWorkspaces) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checks
}

func (w *fakeWorkspaces) Prepare(_ context.Context, r WorkspaceRequest) (string, protocol.WorkspacePrepared, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reqs = append(w.reqs, r)
	if w.err != nil {
		return "", protocol.WorkspacePrepared{}, w.err
	}
	return "/ws/" + r.Step, protocol.WorkspacePrepared{Repository: r.Repository, Branch: "feature/" + r.Task + "--" + r.Step,
		Base: r.Base, BaseSHA: "abc", Instructions: []protocol.InstructionFile{{Path: "AGENTS.md", SHA256: "h"}}}, nil
}

func (w *fakeWorkspaces) Finish(_ context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finished = append(w.finished, jkey(step, fence))
	if w.finErr != nil {
		return nil, w.finErr
	}
	return &protocol.WorkspaceChanges{BaseSHA: "abc", HeadSHA: "def", Files: []protocol.ChangedFile{{Path: "a.go", Status: "M"}},
		FilesTotal: 1}, nil
}

func (w *fakeWorkspaces) done() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.finished)
}

func repoAssignment() domain.Assignment {
	a := assignment("P1")
	a.Base, a.Token = "dev", domain.NewSecret("ghs_tok")
	return a
}

func TestAssignmentWithABaseRunsInItsWorktree(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	ws := &fakeWorkspaces{}
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	if s := l.launched[0]; s.Workdir != "/ws/S1" || s.Dir != "/work/S1-1" {
		t.Fatalf("launch %+v", s)
	}
	if r := ws.reqs[0]; r.Base != "dev" || r.Task != "T1" || r.Repository != "acme/app" || r.Token.Reveal() != "ghs_tok" {
		t.Fatalf("workspace request %+v", r)
	}
	if recs, _ := j.List(); len(recs) != 1 || recs[0].Workdir != "/ws/S1" {
		t.Fatalf("journal %+v", recs)
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	want := []string{protocol.EventAccepted, protocol.EventWorkspacePrepared, protocol.EventOutput, protocol.EventStarted,
		protocol.EventWorkspaceChanges, protocol.EventCompleted}
	if k := ev.kinds(); !slices.Equal(k, want) {
		t.Fatalf("events %v", k)
	}
	if !slices.Equal(ws.done(), []string{"S1-1"}) {
		t.Fatalf("finished %v", ws.done())
	}
	for _, e := range ev.evs {
		if strings.Contains(e.body, "ghs_tok") {
			t.Fatalf("token in event %s", e.kind)
		}
	}
}

func TestWorkspaceFailureFailsTheAttemptWithoutLaunching(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{err: errors.New("not enough free disk space")}
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	if l.count() != 0 {
		t.Fatal("launched without a workspace")
	}
	if e := ev.last(); e.kind != protocol.EventFailed || !strings.Contains(e.body, "workspace") {
		t.Fatalf("%+v", e)
	}
	if done, _ := j.Finished("S1", 1); !done {
		t.Fatal("attempt not finished")
	}
}

// KIN-77: a failure reason quoting a git error carries no known secret (the token, by exact value).
func TestWorkspaceFailureReasonIsRedacted(t *testing.T) {
	t.Parallel()
	x, _, _, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{err: errors.New("fetch https://git.local/x?t=plainTokenValue failed")}
	a := repoAssignment()
	a.Token = domain.NewSecret("plainTokenValue")
	if err := x.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if e := ev.last(); e.kind != protocol.EventFailed || strings.Contains(e.body, "plainToken") || !strings.Contains(e.body, "[redacted]") {
		t.Fatalf("%+v", e)
	}
}

func TestAssignmentWithoutABaseKeepsTheEmptyDirectory(t *testing.T) {
	t.Parallel()
	x, l, _, _ := newExecutor(domain.Policy{})
	ws := &fakeWorkspaces{}
	x.Workspaces = ws
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	if s := l.launched[0]; s.Workdir != "" || len(ws.reqs) != 0 {
		t.Fatalf("launch %+v, workspace requests %d", s, len(ws.reqs))
	}
}

func TestAssignmentSecretIsRedacted(t *testing.T) {
	t.Parallel()
	a := repoAssignment()
	if s := strings.Join([]string{a.Token.String(), a.Token.GoString()}, " "); strings.Contains(s, "ghs_") {
		t.Fatal(s)
	}
}

// A preparation cut short by the connection's end is not the attempt's failure: the command is re-sent.
func TestInterruptedPreparationIsRetried(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{err: context.Canceled}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := x.Assign(ctx, repoAssignment()); err == nil {
		t.Fatal("no error: the command would not be re-sent")
	}
	if l.count() != 0 || slices.Contains(ev.kinds(), protocol.EventFailed) {
		t.Fatalf("events %v", ev.kinds())
	}
	if recs, _ := j.List(); len(recs) != 0 {
		t.Fatal("record left")
	}
	if done, _ := j.Finished("S1", 1); done {
		t.Fatal("attempt finished")
	}
}

type failingJournal struct{ *memJournal }

func (j failingJournal) Save(r domain.Record) error {
	if r.Workdir != "" {
		return errors.New("disk full")
	}
	return j.memJournal.Save(r)
}

// A workspace prepared for an attempt that is then dropped (its record could not be saved) is released, so the
// re-sent assignment can take it again.
func TestDroppedAttemptReleasesItsWorkspace(t *testing.T) {
	t.Parallel()
	x, l, j, _ := newExecutor(domain.Policy{})
	x.Journal = failingJournal{j}
	ws := &fakeWorkspaces{}
	x.Workspaces = ws
	if err := x.Assign(t.Context(), repoAssignment()); err == nil {
		t.Fatal("no error")
	}
	if l.count() != 0 || !slices.Equal(ws.done(), []string{"S1-1"}) {
		t.Fatalf("launched %d, released %v", l.count(), ws.done())
	}
}

// Before the worktree is saved and removed, whatever the agent left running in its process group is killed.
func TestLeftoverProcessesAreKilledBeforeTheWorkspaceIsFinished(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{}
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	if s := l.proc(0).sent(); !slices.Contains(s, true) {
		t.Fatalf("signals %v", s)
	}
}

// KIN-51: while the process runs the executor reports the worktree's changes every ChangesEvery, only when they
// differ from the last report, and never once the attempt is being finished.
func TestRunningAttemptReportsItsChangesPeriodically(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	ws := &fakeWorkspaces{}
	x.Workspaces, x.ChangesEvery = ws, 5*time.Millisecond
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	interim := func() []string {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		var out []string
		for _, e := range ev.evs {
			if e.kind == protocol.EventWorkspaceChanges && strings.Contains(e.body, `"interim":true`) {
				out = append(out, e.body)
			}
		}
		return out
	}
	waitFor(t, "checks", func() bool { return ws.calls() >= 3 })
	if n := len(interim()); n != 0 {
		t.Fatalf("%d interim reports of an unchanged worktree", n)
	}
	ws.setInterim("outside.txt")
	waitFor(t, "interim report", func() bool { return len(interim()) == 1 })
	c := ws.calls()
	waitFor(t, "more checks", func() bool { return ws.calls() >= c+3 })
	if got := interim(); len(got) != 1 || !strings.Contains(got[0], "outside.txt") {
		t.Fatalf("interim reports %v", got)
	}
	ws.setInterim("outside.txt", "more.txt")
	waitFor(t, "second interim report", func() bool { return len(interim()) == 2 })
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	ev.mu.Lock()
	final := ev.evs[len(ev.evs)-2]
	ev.mu.Unlock()
	if final.kind != protocol.EventWorkspaceChanges || strings.Contains(final.body, "interim") {
		t.Fatalf("events %v", ev.kinds())
	}
	time.Sleep(20 * time.Millisecond)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.late {
		t.Fatal("changes listed after the workspace was finished")
	}
}

// KIN-51: when the runner cannot list what the attempt changed, it still reports (as truncated), so the Control
// Plane holds the step instead of completing it unchecked.
func TestUnlistableChangesAreReportedAsTruncated(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	x.Workspaces = &fakeWorkspaces{finErr: errors.New("index corrupt")}
	if err := x.Assign(t.Context(), repoAssignment()); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	ev.mu.Lock()
	report := ev.evs[len(ev.evs)-2]
	ev.mu.Unlock()
	if report.kind != protocol.EventWorkspaceChanges || !strings.Contains(report.body, `"truncated":true`) {
		t.Fatalf("report %+v", report)
	}
}
