package app

import (
	"context"
	"encoding/json"
	"slices"
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
	err  error
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
