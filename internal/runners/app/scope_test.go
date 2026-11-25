package app

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// recScope records the change reports the channel hands to the scope check (KIN-51).
type recScope struct {
	mu   sync.Mutex
	got  []protocol.WorkspaceChanges
	keys []string
	// missing lists the attempts whose completion came without a final changes report (KIN-78).
	missing []string
	err     error
}

func (r *recScope) CheckChanges(_ context.Context, runnerID, step string, fence int64, ch protocol.WorkspaceChanges) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ch)
	r.keys = append(r.keys, runnerID+"/"+step)
	_ = fence
	return r.err
}

func changesEvent(seq int64, step string, fence int64, body json.RawMessage) protocol.Event {
	return protocol.Event{Epoch: "sp", Seq: seq, Base: seq - 1, Step: step, Fence: fence, AttemptSeq: seq,
		Kind: protocol.EventWorkspaceChanges, At: 1, Body: body}
}

// workspace.changes of the current attempt goes to the scope check with the runner from the authenticated session;
// an unreadable report is checked as one whose files are unknown (truncated), so it cannot slip past the check.
func TestWorkspaceChangesReachTheScopeCheck(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	sc := &recScope{}
	e.ch.Scope = sc
	e.wf.fence["s1"], e.wf.state["s1"] = 3, "running"
	sess, _ := newSession("r1")
	body, _ := json.Marshal(protocol.WorkspaceChanges{BaseSHA: "b", Files: []protocol.ChangedFile{{Path: "a.go", Status: "M"}},
		FilesTotal: 1, Interim: true})
	if err := handle(t, e.ch, sess, protocol.TypeEvent, changesEvent(1, "s1", 3, body)); err != nil {
		t.Fatal(err)
	}
	if err := handle(t, e.ch, sess, protocol.TypeEvent, changesEvent(2, "s1", 3, json.RawMessage(`"nope"`))); err != nil {
		t.Fatal(err)
	}
	if len(sc.got) != 2 || !slices.Equal(sc.keys, []string{"r1/s1", "r1/s1"}) {
		t.Fatalf("checked %+v %v", sc.got, sc.keys)
	}
	if g := sc.got[0]; len(g.Files) != 1 || g.Files[0].Path != "a.go" || !g.Interim || g.Truncated {
		t.Fatalf("report %+v", g)
	}
	if g := sc.got[1]; !g.Truncated || len(g.Files) != 0 {
		t.Fatalf("unreadable report checked as %+v", g)
	}
	if rec := e.repo.events[evKey("s1", 3, 1)]; rec.Outcome != domain.OutcomeApplied {
		t.Fatalf("record %+v", rec)
	}
}

// A report of a superseded attempt is not checked (its fence is stale: the runner is told to stop it); a hold that
// finds the token superseded is the same stale case.
func TestStaleWorkspaceChangesAreNotChecked(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	sc := &recScope{}
	e.ch.Scope = sc
	e.wf.fence["s1"], e.wf.state["s1"] = 4, "running"
	sess, _ := newSession("r1")
	if err := handle(t, e.ch, sess, protocol.TypeEvent, changesEvent(1, "s1", 3, json.RawMessage(`{}`))); err != nil {
		t.Fatal(err)
	}
	if len(sc.got) != 0 {
		t.Fatalf("stale report checked: %+v", sc.got)
	}
	sc.err = domain.ErrStaleAttempt
	if err := handle(t, e.ch, sess, protocol.TypeEvent, changesEvent(2, "s1", 4, json.RawMessage(`{}`))); err != nil {
		t.Fatal(err)
	}
	if rec := e.repo.events[evKey("s1", 4, 2)]; rec.Outcome != domain.OutcomeRejected || rec.Reason != protocol.CancelStaleFence {
		t.Fatalf("record %+v", rec)
	}
	if cmds := e.commands("r1"); len(cmds) != 2 || cmds[1].Kind != protocol.CommandCancel {
		t.Fatalf("commands %+v", cmds)
	}
}

func (r *recScope) ChangesMissing(_ context.Context, runnerID, step string, fence int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.missing = append(r.missing, runnerID+"/"+step+"/"+strconv.FormatInt(fence, 10))
	return r.err
}

func attemptEvent(epoch string, seq int64, step string, fence, attemptSeq int64, kind string, body json.RawMessage) protocol.Event {
	return protocol.Event{Epoch: epoch, Seq: seq, Base: seq - 1, Step: step, Fence: fence, AttemptSeq: attemptSeq,
		Kind: kind, At: 1, Body: body}
}

var (
	preparedBody = json.RawMessage(`{"repository":"acme/app","branch":"b","base":"dev","baseSha":"b1","instructions":[]}`)
	interimBody  = json.RawMessage(`{"branch":"b","baseSha":"b1","headSha":"","files":[],"filesTotal":0,"interim":true}`)
	finalBody    = json.RawMessage(`{"branch":"b","baseSha":"b1","headSha":"h1","files":[],"filesTotal":0}`)
)

// KIN-78: an attempt that prepared a worktree completes only after its final (non-interim) workspace.changes was
// accepted; a completion without one holds the step (scope_exceeded, changes report not received) instead of
// completing it unchecked. The completion is recorded as applied: it was handled.
func TestCompletionWithoutAFinalChangesReportHoldsTheStep(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	sc := &recScope{}
	e.ch.Scope = sc
	e.wf.fence["s1"], e.wf.state["s1"] = 3, "running"
	sess, _ := newSession("r1")
	for i, ev := range []protocol.Event{
		attemptEvent("sp", 1, "s1", 3, 1, protocol.EventWorkspacePrepared, preparedBody),
		attemptEvent("sp", 2, "s1", 3, 2, protocol.EventWorkspaceChanges, interimBody), // interim only
		attemptEvent("sp", 3, "s1", 3, 3, protocol.EventCompleted, nil),
	} {
		if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	if !slices.Equal(sc.missing, []string{"r1/s1/3"}) {
		t.Fatalf("missing report not raised: %v", sc.missing)
	}
	if e.wf.state["s1"] != "running" || slices.Contains(e.wf.history(), "complete") {
		t.Fatalf("step completed without a changes report: %s %v", e.wf.state["s1"], e.wf.history())
	}
	if rec := e.repo.events[evKey("s1", 3, 3)]; rec.Outcome != domain.OutcomeApplied {
		t.Fatalf("completion record %+v", rec)
	}
}

// A final report accepted before the completion (events of one runner are applied in spool order, and the runner
// reports its changes before its end) lets the step complete; a step without a worktree (no workspace.prepared,
// e.g. a planning step) completes without any report.
func TestCompletionAfterTheFinalChangesReportCompletes(t *testing.T) {
	t.Parallel()
	for name, evs := range map[string][]protocol.Event{
		"final report": {
			attemptEvent("sp", 1, "s1", 3, 1, protocol.EventWorkspacePrepared, preparedBody),
			attemptEvent("sp", 2, "s1", 3, 2, protocol.EventWorkspaceChanges, interimBody),
			attemptEvent("sp", 3, "s1", 3, 3, protocol.EventWorkspaceChanges, finalBody),
			attemptEvent("sp", 4, "s1", 3, 4, protocol.EventCompleted, nil),
		},
		"no workspace": {attemptEvent("sp", 1, "s1", 3, 1, protocol.EventCompleted, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newChannelEnv()
			sc := &recScope{}
			e.ch.Scope = sc
			e.wf.fence["s1"], e.wf.state["s1"] = 3, "running"
			sess, _ := newSession("r1")
			for i, ev := range evs {
				if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
			}
			if len(sc.missing) != 0 || e.wf.state["s1"] != "done" {
				t.Fatalf("missing %v, state %s", sc.missing, e.wf.state["s1"])
			}
		})
	}
}

// An attempt adopted after a runner restart reports under a new spool epoch and gets only the end-of-step report:
// the workspace.prepared of the earlier runner process still counts, so the adopted attempt completes only with its
// final report, and is held without it. A final report of another attempt (older fence) does not count.
func TestAdoptedAttemptNeedsItsOwnFinalChangesReport(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		evs  []protocol.Event
		held bool
	}{
		"final report after the restart": {evs: []protocol.Event{
			attemptEvent("sp1", 1, "s1", 3, 1, protocol.EventWorkspacePrepared, preparedBody),
			attemptEvent("sp2", 1, "s1", 3, 2, protocol.EventWorkspaceChanges, finalBody),
			attemptEvent("sp2", 2, "s1", 3, 3, protocol.EventCompleted, nil),
		}},
		"no report after the restart": {held: true, evs: []protocol.Event{
			attemptEvent("sp1", 1, "s1", 2, 1, protocol.EventWorkspaceChanges, finalBody), // the previous attempt's
			attemptEvent("sp1", 2, "s1", 3, 1, protocol.EventWorkspacePrepared, preparedBody),
			attemptEvent("sp2", 1, "s1", 3, 2, protocol.EventCompleted, nil),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newChannelEnv()
			sc := &recScope{}
			e.ch.Scope = sc
			e.wf.fence["s1"], e.wf.state["s1"] = 3, "running"
			sess, _ := newSession("r1")
			for i, ev := range tc.evs {
				if ev.Fence == 2 { // the previous attempt's report, accepted while it was current
					e.wf.fence["s1"] = 2
				}
				if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				e.wf.fence["s1"] = 3
			}
			if held := len(sc.missing) == 1; held != tc.held || (e.wf.state["s1"] == "done") == tc.held {
				t.Fatalf("missing %v, state %s", sc.missing, e.wf.state["s1"])
			}
		})
	}
}
