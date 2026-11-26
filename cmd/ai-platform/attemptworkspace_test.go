package main

import (
	"context"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// latestOnly is an attempt's recorded events by kind.
type latestOnly map[string]runners.AttemptEvent

func (latestOnly) AttemptEvents(context.Context, string, int64, int64, int) ([]runners.AttemptEvent, error) {
	return nil, nil
}

func (l latestOnly) LatestAttemptEvent(_ context.Context, _ string, _ int64, kind string) (runners.AttemptEvent, bool, error) {
	e, ok := l[kind]
	return e, ok, nil
}

// The task UI's workspace reports (KIN-70) are the runner's latest workspace.prepared and workspace.changes events
// decoded into typed fields; a missing or unreadable report is none, never an error.
func TestAttemptWorkspaceDecodesTheLatestReports(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	log := workflowAttemptLog{log: runnersapp.AttemptLog{Repo: latestOnly{
		protocol.EventWorkspacePrepared: {RunnerAt: at, Body: []byte(`{"repository":"acme/app","branch":"feature/t--s",
			"base":"dev","baseSha":"b1","instructions":[{"path":"AGENTS.md","sha256":"aa","size":3},
			{"path":".claude/rules/x.md","sha256":"bb","size":1,"symlink":true}],"instructionsTruncated":true}`)},
		protocol.EventWorkspaceChanges: {RunnerAt: at.Add(time.Minute), Body: []byte(`{"branch":"feature/t--s","baseSha":"b1",
			"headSha":"h1","files":[{"path":"a.go","status":"A"}],"filesTotal":2,"truncated":true,
			"instructionsChanged":[{"path":"AGENTS.md","before":"aa","after":"cc"}]}`)},
	}}}
	w, err := log.AttemptWorkspace(t.Context(), "s", 1)
	if err != nil || w.Prepared == nil || w.Changes == nil {
		t.Fatalf("workspace %+v %v", w, err)
	}
	p := w.Prepared
	if !p.At.Equal(at) || p.Repository != "acme/app" || p.Branch != "feature/t--s" || p.Base != "dev" || p.BaseSHA != "b1" ||
		!p.InstructionsTruncated || len(p.Instructions) != 2 || p.Instructions[0].Size != 3 || !p.Instructions[1].Symlink {
		t.Fatalf("prepared %+v", p)
	}
	c := w.Changes
	if !c.At.Equal(at.Add(time.Minute)) || c.HeadSHA != "h1" || len(c.Files) != 1 || c.Files[0].Status != "A" ||
		c.FilesTotal != 2 || !c.Truncated || len(c.InstructionsChanged) != 1 || c.InstructionsChanged[0].After != "cc" {
		t.Fatalf("changes %+v", c)
	}

	broken := workflowAttemptLog{log: runnersapp.AttemptLog{Repo: latestOnly{
		protocol.EventWorkspacePrepared: {Body: []byte(`{not json`)},
		protocol.EventWorkspaceChanges:  {Body: nil},
	}}}
	if w, err := broken.AttemptWorkspace(t.Context(), "s", 1); err != nil || w.Prepared != nil || w.Changes != nil {
		t.Fatalf("unreadable reports %+v %v", w, err)
	}
	if w, err := (workflowAttemptLog{log: runnersapp.AttemptLog{Repo: latestOnly{}}}).AttemptWorkspace(t.Context(), "s", 1); err != nil ||
		w.Prepared != nil || w.Changes != nil {
		t.Fatalf("no reports %+v %v", w, err)
	}
}

// eventsOnly is an attempt's recorded events in order.
type eventsOnly []runners.AttemptEvent

func (e eventsOnly) AttemptEvents(context.Context, string, int64, int64, int) ([]runners.AttemptEvent, error) {
	return e, nil
}

func (eventsOnly) LatestAttemptEvent(context.Context, string, int64, string) (runners.AttemptEvent, bool, error) {
	return runners.AttemptEvent{}, false, nil
}

// KIN-71: the task UI shows a workspace.preparing event's phase; an unknown phase (untrusted text) is not shown.
func TestAttemptLogShowsThePreparationPhase(t *testing.T) {
	t.Parallel()
	log := workflowAttemptLog{log: runnersapp.AttemptLog{Repo: eventsOnly{
		{AttemptSeq: 2, Kind: protocol.EventWorkspacePreparing, Body: []byte(`{"phase":"fetching","elapsedSec":20}`)},
		{AttemptSeq: 3, Kind: protocol.EventWorkspacePreparing, Body: []byte(`{"phase":"<b>x</b>"}`)},
	}}}
	evs, err := log.AttemptEvents(t.Context(), "s", 1, 0, 10)
	if err != nil || len(evs) != 2 || evs[0].Reason != protocol.PreparingFetching || evs[1].Reason != "" {
		t.Fatalf("%+v %v", evs, err)
	}
}
