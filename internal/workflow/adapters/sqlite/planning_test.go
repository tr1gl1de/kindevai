package sqlite_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

func TestPlanningRunsRoundTripWithOneActiveRunPerTask(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, fixed)
	task, err := svc.CreateTask(ctx, "p1", "Login", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	repo := sqlite.New(d)
	if _, err := repo.LatestPlanningRun(ctx, task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no run yet: %v", err)
	}
	r := domain.NewPlanningRun("R1", task.ID, "u1", true, t0)
	if err := repo.CreatePlanningRun(ctx, &r); err != nil || r.Version != 1 {
		t.Fatalf("create: %v %d", err, r.Version)
	}
	dup := domain.NewPlanningRun("R2", task.ID, "u1", false, t0)
	if err := repo.CreatePlanningRun(ctx, &dup); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second active run: %v", err)
	}
	c := domain.PlanningContext{IssueText: "Users log in <b>", Articles: []domain.ContextArticle{{ID: "KIN-A-9", Title: "Auth", Text: "24h"}}}
	if err := r.ContextReady(c, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Assign("run1", domain.AgentChoice{Agent: "claude", Model: "opus"}, t0, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := r.Report("run1", 1, "sess-1", "{}"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePlanningRun(ctx, &r); err != nil {
		t.Fatal(err)
	}
	stale := r
	stale.Version = 1
	if err := repo.SavePlanningRun(ctx, &stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	got, err := repo.PlanningRun(ctx, "R1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.PlanningRunning || got.Fence != 1 || got.Agent != "claude" || got.Model != "opus" ||
		got.Session != "sess-1" || got.SessionRunner != "run1" || got.SessionAgent != "claude" || got.Result != "{}" ||
		got.Context.IssueText != c.IssueText || len(got.Context.Articles) != 1 || got.LeaseExpires == nil ||
		!got.LeaseExpires.Equal(t0.Add(time.Minute)) || got.Version != 2 || got.CreatedBy != "u1" {
		t.Fatalf("loaded %+v", got)
	}
	if ids, _ := repo.ActivePlanningRuns(ctx); !slices.Equal(ids, []string{"R1"}) {
		t.Fatalf("active %v", ids)
	}
	if ids, _ := repo.PlanningRunsHeldBy(ctx, "run1"); !slices.Equal(ids, []string{"R1"}) {
		t.Fatalf("held %v", ids)
	}
	until := t0.Add(time.Hour)
	if err := got.Hold("run1", 1, domain.WaitCLILimit, "limit", until, t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePlanningRun(ctx, &got); err != nil {
		t.Fatal(err)
	}
	held, _ := repo.PlanningRun(ctx, "R1")
	if held.NotBefore == nil || !held.NotBefore.Equal(until) || held.WaitReason != domain.WaitCLILimit || held.LeaseExpires != nil {
		t.Fatalf("held %+v", held)
	}
	if ids, _ := repo.PlanningRunsHeldBy(ctx, "run1"); len(ids) != 0 {
		t.Fatalf("held after hold %v", ids)
	}
	if err := held.Cancel("stop", t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.SavePlanningRun(ctx, &held); err != nil {
		t.Fatal(err)
	}
	next := domain.NewPlanningRun("R3", task.ID, "u1", false, t0)
	if err := repo.CreatePlanningRun(ctx, &next); err != nil {
		t.Fatalf("a new run after the last ended: %v", err)
	}
	if l, err := repo.LatestPlanningRun(ctx, task.ID); err != nil || l.ID != "R3" {
		t.Fatalf("latest %+v %v", l, err)
	}
}
