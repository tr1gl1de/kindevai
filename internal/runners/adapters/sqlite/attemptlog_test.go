package sqlite_test

import (
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// The task UI reads an attempt's applied events after a cursor, in order and in bounded pages (KIN-25); rejected
// events (from runners that did not hold the attempt) never show.
func TestAttemptLogPages(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	rec := func(runner string, fence, seq int64, kind, outcome string, body []byte) {
		t.Helper()
		if err := r.RecordEvent(ctx, domain.AttemptEvent{RunnerID: runner, Step: "s1", Fence: fence, AttemptSeq: seq,
			Kind: kind, Body: body, Outcome: outcome, RunnerAt: t0.Add(time.Duration(seq) * time.Second), ReceivedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	rec("r1", 1, 1, "execution.started", domain.OutcomeApplied, nil)
	for seq := int64(2); seq <= 50; seq++ {
		rec("r1", 1, seq, "execution.output", domain.OutcomeApplied, []byte(`{"stream":"stdout","offset":0,"data":"x"}`))
	}
	rec("r2", 1, 51, "execution.output", domain.OutcomeRejected, nil)
	rec("r1", 2, 1, "execution.started", domain.OutcomeApplied, nil)
	log := app.AttemptLog{Repo: r}
	page, err := log.Events(ctx, "s1", 1, 0, 1000)
	if err != nil || len(page) != app.MaxAttemptLogPage || page[0].Kind != "execution.started" || page[0].AttemptSeq != 1 ||
		!page[0].RunnerAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("first page %d %+v %v", len(page), page[0], err)
	}
	rest, err := log.Events(ctx, "s1", 1, page[len(page)-1].AttemptSeq, 1000)
	if err != nil || len(rest) != 50-app.MaxAttemptLogPage || string(rest[0].Body) == "" || rest[len(rest)-1].AttemptSeq != 50 {
		t.Fatalf("rest %d %v", len(rest), err)
	}
	if none, _ := log.Events(ctx, "s1", 1, 50, 10); len(none) != 0 {
		t.Fatalf("after the end %+v", none)
	}
	if other, _ := log.Events(ctx, "s1", 2, 0, 10); len(other) != 1 {
		t.Fatalf("fence 2 %+v", other)
	}
}

// The task UI shows an attempt's latest workspace report of a kind (KIN-70): the newest applied event wins, rejected
// events and other attempts never show, and an attempt without one reports none.
func TestAttemptLogLatestOfKind(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	rec := func(runner string, fence, seq int64, kind, outcome, body string) {
		t.Helper()
		if err := r.RecordEvent(ctx, domain.AttemptEvent{RunnerID: runner, Step: "s1", Fence: fence, AttemptSeq: seq,
			Kind: kind, Body: []byte(body), Outcome: outcome, RunnerAt: t0.Add(time.Duration(seq) * time.Second), ReceivedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	rec("r1", 1, 1, "workspace.prepared", domain.OutcomeApplied, `{"branch":"a"}`)
	rec("r1", 1, 2, "workspace.changes", domain.OutcomeApplied, `{"headSha":"1"}`)
	rec("r1", 1, 3, "workspace.changes", domain.OutcomeApplied, `{"headSha":"2"}`)
	rec("r2", 1, 4, "workspace.changes", domain.OutcomeRejected, `{"headSha":"forged"}`)
	rec("r1", 2, 1, "workspace.changes", domain.OutcomeApplied, `{"headSha":"other"}`)
	log := app.AttemptLog{Repo: r}
	e, ok, err := log.Latest(ctx, "s1", 1, "workspace.changes")
	if err != nil || !ok || string(e.Body) != `{"headSha":"2"}` || e.AttemptSeq != 3 || !e.RunnerAt.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("latest changes %+v %v %v", e, ok, err)
	}
	if e, ok, err := log.Latest(ctx, "s1", 1, "workspace.prepared"); err != nil || !ok || string(e.Body) != `{"branch":"a"}` {
		t.Fatalf("latest prepared %+v %v %v", e, ok, err)
	}
	if _, ok, err := log.Latest(ctx, "s1", 2, "workspace.prepared"); err != nil || ok {
		t.Fatalf("fence 2 prepared %v %v", ok, err)
	}
}
