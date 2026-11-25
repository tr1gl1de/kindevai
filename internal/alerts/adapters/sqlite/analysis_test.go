package sqlite_test

import (
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
)

// One analysis task per alert, kept after the alert closed, deleted with the alert.
func TestAnalysisTaskRoundTripOnePerAlert(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	a := alert(t, "a1", "p1", "storage/m1/data", t0)
	if err := r.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Analysis(ctx, "a1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("before: %v", err)
	}
	task, err := domain.NewAnalysisTask("a1", "p1", "KIN", "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateAnalysis(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateAnalysis(ctx, task); err == nil {
		t.Fatal("a second analysis task for the alert was stored")
	}
	// The alert closes (immutable from now on); the issue is still recorded.
	if err := a.Resolve("u1", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := task.MarkCreated("KIN-7", "https://yt.example/issue/KIN-7", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveAnalysis(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := r.Analysis(ctx, "a1")
	if err != nil || got.IssueID != "KIN-7" || got.IssueURL != "https://yt.example/issue/KIN-7" || got.YouTrackProject != "KIN" ||
		got.RequestedBy != "u1" || !got.RequestedAt.Equal(t0) || got.CreatedAt == nil || got.ProjectID != "p1" {
		t.Fatalf("got %+v, %v", got, err)
	}
	m, err := r.Analyses(ctx, []string{"a1", "a2"})
	if err != nil || len(m) != 1 || m["a1"].IssueID != "KIN-7" {
		t.Fatalf("analyses = %+v, %v", m, err)
	}
	if m, err := r.Analyses(ctx, nil); err != nil || len(m) != 0 {
		t.Fatalf("empty = %+v, %v", m, err)
	}
	if _, err := d.ExecContext(ctx, "DELETE FROM alerts WHERE id = 'a1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Analysis(ctx, "a1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after the alert's deletion: %v", err)
	}
}
