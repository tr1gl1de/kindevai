package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// KIN-36: a step in its publish phase and a publish attempt's pushed branch survive a save and load; the change
// set keeps one artifact per repository and branch, updated in place when its pull request opens.
func TestPublishPhaseAndChangeSetAreStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, func() time.Time { return t0 })
	repo := sqlite.New(d)
	_, e := running(t, svc)
	st := e.StepByKey("backend")
	st.Phase = domain.PhasePublish
	st.Attempts = append(st.Attempts, domain.Attempt{Number: 1, RunnerID: "r1", Fence: 1, State: domain.AttemptSucceeded,
		StartedAt: t0, FinishedAt: &t0, Role: domain.StepRolePublisher, HeadSHA: "h1", PushedBranch: "feature/t--s"})
	if err := repo.SaveExecution(ctx, &e); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Step(st.ID); s.Phase != domain.PhasePublish || s.Attempts[0].PushedBranch != "feature/t--s" {
		t.Fatalf("step %+v", s)
	}

	a := domain.NewGitArtifact(&got, got.Step(st.ID), domain.Publication{Repository: "api", Branch: "feature/t--s",
		BaseSHA: "b0", HeadSHA: "h1"}, t0)
	if err := repo.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := a.Opened(7, "https://github.com/acme/api/pull/7", "dev", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := domain.NewGitArtifact(&got, got.Step(st.ID), domain.Publication{Repository: "web", Branch: "feature/t--s", HeadSHA: "h2"}, t0)
	if err := repo.SaveArtifact(ctx, b); err != nil {
		t.Fatal(err)
	}
	one, err := repo.Artifact(ctx, e.ID, "api", "feature/t--s")
	if err != nil || one.State != domain.ArtifactOpen || one.PRNumber != 7 || one.PRBase != "dev" || one.BaseSHA != "b0" ||
		!one.UpdatedAt.Equal(t0.Add(time.Minute)) || one.TaskID != e.TaskID {
		t.Fatalf("artifact %+v %v", one, err)
	}
	all, err := repo.TaskArtifacts(ctx, e.TaskID)
	if err != nil || len(all) != 2 || all[0].Repository != "api" || all[1].Repository != "web" {
		t.Fatalf("task artifacts %+v %v", all, err)
	}
	if _, err := repo.Artifact(ctx, e.ID, "api", "feature/other"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing artifact: %v", err)
	}
	bad := a
	bad.Branch = "main"
	if err := repo.SaveArtifact(ctx, bad); err == nil {
		t.Fatal("stored an artifact of main")
	}
}

// KIN-37: a step's CI fix rounds and feedback and an artifact's CI and merge state survive a save and load; the
// change set is found by branch, and the open unmerged artifacts of VERIFYING tasks are listed for reconciliation.
func TestCIAndMergeStateAreStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, func() time.Time { return t0 })
	repo := sqlite.New(d)
	_, e := running(t, svc)
	st := e.StepByKey("backend")
	st.CIRound, st.CIFeedback = 2, "lint (failure)"
	if err := repo.SaveExecution(ctx, &e); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Step(st.ID); s.CIRound != 2 || s.CIFeedback != "lint (failure)" {
		t.Fatalf("step %+v", s)
	}
	a := domain.NewGitArtifact(&got, got.Step(st.ID), domain.Publication{Repository: "api", Branch: "feature/t--s", HeadSHA: "h1"}, t0)
	if err := a.Opened(7, "u", "dev", t0); err != nil {
		t.Fatal(err)
	}
	a.RecordCI("h1", domain.CIFailed, "test (failure)", t0)
	a.MergeHeld("integrate_dev is not approved", t0)
	if err := repo.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	byBranch, err := repo.BranchArtifacts(ctx, "api", "feature/t--s")
	if err != nil || len(byBranch) != 1 {
		t.Fatalf("by branch %+v %v", byBranch, err)
	}
	if b := byBranch[0]; b.CI != domain.CIFailed || b.CIHeadSHA != "h1" || b.CIDetail != "test (failure)" ||
		b.Merge != domain.MergeWaiting || b.MergeError != "integrate_dev is not approved" {
		t.Fatalf("artifact %+v", b)
	}
	if v, err := repo.VerifyingArtifacts(ctx, 10); err != nil || len(v) != 0 {
		t.Fatalf("a running task's artifacts listed: %+v %v", v, err)
	}
	task, err := repo.Task(ctx, e.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Transition(domain.TaskVerifying, domain.Reason{}, t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if v, err := repo.VerifyingArtifacts(ctx, 10); err != nil || len(v) != 1 || v[0].PRNumber != 7 {
		t.Fatalf("verifying %+v %v", v, err)
	}
	a.Merged("m1", t0)
	if err := repo.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	if v, err := repo.VerifyingArtifacts(ctx, 10); err != nil || len(v) != 0 {
		t.Fatalf("a merged artifact listed: %+v %v", v, err)
	}
}
