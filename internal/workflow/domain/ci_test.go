package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// published runs the step through implementation, approval and the publish of head1 to feature/t1--s1.
func published(t *testing.T) (Execution, string) {
	t.Helper()
	e, id := approved(t)
	publishHead(t, &e, id, "head1")
	if e.State != ExecutionSucceeded {
		t.Fatalf("execution %s", e.State)
	}
	return e, id
}

func publishHead(t *testing.T, e *Execution, id, head string) {
	t.Helper()
	f := runPhase(t, e, id, "r1", publishClaim())
	if err := e.RecordPush(id, "r1", f, Push{Repository: "api", Branch: "feature/t1--s1", HeadSHA: head}); err != nil {
		t.Fatal(err)
	}
	if err := e.Succeed(id, "r1", f, t0); err != nil {
		t.Fatal(err)
	}
}

// KIN-37: CI failing on a published branch reopens the execution: the step implements again on the runner that
// holds the branch, told why CI failed, then is reviewed and pushed again (fast-forward) as a new head.
func TestACIFailureReopensTheStepForAFixRound(t *testing.T) {
	t.Parallel()
	e, id := published(t)
	if err := e.ReopenForCI(id, "CI failed on head1: test (failure)", t0); err != nil {
		t.Fatal(err)
	}
	s := e.Step(id)
	if e.State != ExecutionRunning || e.FinishedAt != nil || s.State != StepReady || s.Phase != PhaseImplement ||
		s.PinnedRunner != "r1" || s.CIRound != 1 || s.Round != 0 {
		t.Fatalf("reopened: execution %s step %s phase %q pinned %q ci round %d round %d", e.State, s.State, s.Phase,
			s.PinnedRunner, s.CIRound, s.Round)
	}
	if fb := s.RetryFeedback(); !strings.Contains(fb, "test (failure)") {
		t.Fatalf("feedback %q", fb)
	}
	f := runPhase(t, &e, id, "r1", implClaim)
	passTests(t, &e, id, "r1", f, 0)
	e.RecordChanges(id, "r1", f, WorkChanges{BaseSHA: "base1", HeadSHA: "head2", Files: []string{"api/x.go"}, Total: 1})
	if err := e.Succeed(id, "r1", f, t0); err != nil {
		t.Fatal(err)
	}
	if fb := e.Step(id).RetryFeedback(); strings.Contains(fb, "test (failure)") {
		t.Fatalf("the reviewer is told the CI feedback: %q", fb)
	}
	reviewWith(t, &e, id, `{"verdict":"approve","summary":"fixed","findings":[]}`)
	publishHead(t, &e, id, "head2")
	p, ok := e.Step(id).Published()
	if e.State != ExecutionSucceeded || !ok || p.HeadSHA != "head2" || p.BaseSHA != "base1" {
		t.Fatalf("after the fix: execution %s published %+v %v", e.State, p, ok)
	}
}

// KIN-37: at most MaxCIFixRounds CI fix rounds per step; then ErrCIBudgetExhausted and nothing changes.
func TestCIFixRoundsAreBounded(t *testing.T) {
	t.Parallel()
	e, id := published(t)
	for i := range MaxCIFixRounds {
		if err := e.ReopenForCI(id, "red", t0); err != nil {
			t.Fatalf("round %d: %v", i+1, err)
		}
		implement(t, &e, id)
		reviewWith(t, &e, id, `{"verdict":"approve","summary":"ok","findings":[]}`)
		publishHead(t, &e, id, "head1")
	}
	before := e.Clone()
	if err := e.ReopenForCI(id, "red", t0); !errors.Is(err, ErrCIBudgetExhausted) {
		t.Fatalf("%v", err)
	}
	if e.State != before.State || e.Step(id).CIRound != MaxCIFixRounds || e.Step(id).State != StepSucceeded {
		t.Fatalf("changed: %s %d %s", e.State, e.Step(id).CIRound, e.Step(id).State)
	}
}

// Only a succeeded execution's published step can be reopened.
func TestReopenForCINeedsAPublishedSucceededStep(t *testing.T) {
	t.Parallel()
	e, id := approved(t) // running: publish pending
	if err := e.ReopenForCI(id, "red", t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("running: %v", err)
	}
	e2, _ := published(t)
	if err := e2.ReopenForCI("nope", "red", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown step: %v", err)
	}
}

func openArtifact(t *testing.T, head string) GitArtifact {
	t.Helper()
	a := GitArtifact{ExecutionID: "e1", TaskID: "t1", StepID: "s1", Repository: "o/api", Branch: "feature/t1--s1",
		HeadSHA: head, State: ArtifactOpening}
	if err := a.Opened(7, "https://x/7", "dev", t0); err != nil {
		t.Fatal(err)
	}
	return a
}

// KIN-37: a CI result counts only for the artifact's current head; results of an older head are ignored.
func TestCIResultsCountOnlyForTheCurrentHead(t *testing.T) {
	t.Parallel()
	a := openArtifact(t, "head2")
	if a.RecordCI("head1", CIFailed, "old", t0) {
		t.Fatal("an older head's result was recorded")
	}
	if !a.RecordCI("head2", CIPassed, "", t0) || a.CI != CIPassed || a.CIHeadSHA != "head2" {
		t.Fatalf("artifact %+v", a)
	}
	if a.RecordCI("head2", CIPassed, "", t0.Add(time.Minute)) {
		t.Fatal("the same result changed it again")
	}
}

// KIN-37: merge order across repositories: the first unmerged artifact in order goes next, a failed merge stops
// the rest, and an artifact merges only when its pull request is open and its CI passed on its head.
func TestNextMergeFollowsTheChangeSetOrderAndStopsOnFailure(t *testing.T) {
	t.Parallel()
	a, b := openArtifact(t, "h1"), openArtifact(t, "h2")
	b.Repository = "o/web"
	a.RecordCI("h1", CIPassed, "", t0)
	b.RecordCI("h2", CIPassed, "", t0)
	next, stop := NextMerge([]GitArtifact{a, b})
	if next == nil || next.Repository != "o/api" || stop != "" {
		t.Fatalf("next %+v stop %q", next, stop)
	}
	a.Merged("m1", t0)
	if next, _ = NextMerge([]GitArtifact{a, b}); next == nil || next.Repository != "o/web" {
		t.Fatalf("after the first: %+v", next)
	}
	b.MergeFailed("not mergeable", t0)
	if next, stop = NextMerge([]GitArtifact{a, b}); next != nil || !strings.Contains(stop, "o/web") {
		t.Fatalf("after a failure: %+v %q", next, stop)
	}
	c := openArtifact(t, "h3")
	c.RecordCI("h3", CIPending, "", t0)
	if next, stop = NextMerge([]GitArtifact{c}); next != nil || stop != "" {
		t.Fatalf("pending CI: %+v %q", next, stop)
	}
	b.Merged("m2", t0)
	if !AllMerged([]GitArtifact{a, b}) || AllMerged([]GitArtifact{a, c}) || AllMerged(nil) {
		t.Fatal("AllMerged")
	}
}

// main/master are never a merge base, whatever the artifact says.
func TestAMergeIntoAProtectedBaseIsRefused(t *testing.T) {
	t.Parallel()
	a := openArtifact(t, "h1")
	a.PRBase = "main"
	a.RecordCI("h1", CIPassed, "", t0)
	if err := a.CheckMergeable(); !errors.Is(err, ErrProtectedBranch) {
		t.Fatalf("%v", err)
	}
}

// KIN-37: a task whose CI fix budget ran out waits for the Owner: BLOCKED with reason ci.
func TestATaskCanBeBlockedOnCI(t *testing.T) {
	t.Parallel()
	task := newTask(t)
	for _, to := range []TaskState{TaskAnalyzing, TaskReady, TaskRunning, TaskVerifying} {
		if err := task.Transition(to, Reason{}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := task.Transition(TaskBlocked, Reason{Code: WaitCI, Detail: "CI still failing"}, t0); err != nil {
		t.Fatal(err)
	}
	if task.BlockedFrom != TaskVerifying {
		t.Fatalf("blocked from %s", task.BlockedFrom)
	}
}

// KIN-105: a head without any check run (a repository without CI) counts how long it has had none: the window
// starts at the first such result of the head, any check run or a new head ends it, and the Owner's resume starts
// a new one; past NoCIBlockAfter the task is blocked rather than merged.
func TestAHeadWithoutCheckRunsCountsItsWindow(t *testing.T) {
	t.Parallel()
	a := openArtifact(t, "head2")
	if a.NoChecksFor(t0) != 0 {
		t.Fatal("window before any result")
	}
	if !a.RecordNoChecks("head2", t0) || a.CI != CIPending || !a.NoChecksSince.Equal(t0) {
		t.Fatalf("first observation: %+v", a)
	}
	if a.RecordNoChecks("head2", t0.Add(10*time.Minute)) || a.NoChecksFor(t0.Add(NoCIBlockAfter)) != NoCIBlockAfter {
		t.Fatalf("the window restarted: %+v", a)
	}
	if !a.RecordCI("head2", CIPending, "", t0.Add(11*time.Minute)) || !a.NoChecksSince.IsZero() {
		t.Fatalf("a running check run kept the window: %+v", a)
	}
	a.RecordNoChecks("head2", t0.Add(12*time.Minute))
	a.HeadSHA = "head3" // a fix round pushed a new head
	if a.NoChecksFor(t0.Add(time.Hour)) != 0 {
		t.Fatal("an older head's window counted")
	}
	if !a.RecordNoChecks("head3", t0.Add(20*time.Minute)) || !a.NoChecksSince.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("the new head's window: %+v", a)
	}
	if !a.RestartNoChecksWindow() || a.NoChecksFor(t0.Add(time.Hour)) != 0 || a.RestartNoChecksWindow() {
		t.Fatalf("restart: %+v", a)
	}
}

// KIN-105: the Owner's retry clears a refused merge (the gate runs again); nothing else is cleared.
func TestARefusedMergeCanBeRetried(t *testing.T) {
	t.Parallel()
	a := openArtifact(t, "head2")
	if a.RetryMerge(t0) {
		t.Fatal("retried a merge that was not refused")
	}
	a.MergeFailed("GitHub refused the merge", t0)
	if !a.RetryMerge(t0) || a.Merge != "" || a.MergeError != "" {
		t.Fatalf("after retry: %+v", a)
	}
}

// KIN-106: the published head of a step's work branch outlives the CI fix round's reset of its attempt window (and
// a runner loss's): the next implementer continues from it.
func TestThePublishedHeadOutlivesACIFixRound(t *testing.T) {
	t.Parallel()
	e, id := published(t)
	s := e.Step(id)
	if s.PublishedHead() != "head1" {
		t.Fatalf("published head %q", s.PublishedHead())
	}
	if err := e.ReopenForCI(id, "lint (failure)", t0); err != nil {
		t.Fatal(err)
	}
	if s = e.Step(id); s.PublishedHead() != "head1" {
		t.Fatalf("after the reopen %q", s.PublishedHead())
	}
	s.restartReview()
	if s.PublishedHead() != "head1" {
		t.Fatalf("after a runner loss %q", s.PublishedHead())
	}
}

// KIN-113: the Owner may waive CI only for a head that has no check run at all; the waiver makes that head
// mergeable, never one with red or running CI, a newer head or a main/master base.
func TestCIIsWaivedOnlyForAHeadWithoutCheckRuns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		prep     func(a *GitArtifact)
		waivable bool
	}{
		{"no result yet", func(a *GitArtifact) {}, false},
		{"no check runs", func(a *GitArtifact) { a.RecordNoChecks("h1", t0) }, true},
		{"running checks", func(a *GitArtifact) { a.RecordCI("h1", CIPending, "", t0) }, false},
		{"red CI", func(a *GitArtifact) { a.RecordCI("h1", CIFailed, "lint", t0) }, false},
		{"green CI", func(a *GitArtifact) { a.RecordCI("h1", CIPassed, "", t0) }, false},
		{"merged", func(a *GitArtifact) { a.RecordNoChecks("h1", t0); a.Merged("m1", t0) }, false},
		{"protected base", func(a *GitArtifact) { a.RecordNoChecks("h1", t0); a.PRBase = "master" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := openArtifact(t, "h1")
			c.prep(&a)
			err := a.WaiveCI(t0)
			if (err == nil) != c.waivable {
				t.Fatalf("waive: %v", err)
			}
			if c.waivable && (a.CIWaivedHead != "h1" || !a.CIWaivedOnHead() || a.CheckMergeable() != nil) {
				t.Fatalf("waived head not mergeable: %+v %v", a, a.CheckMergeable())
			}
			if !c.waivable && a.CIWaivedOnHead() {
				t.Fatalf("waived: %+v", a)
			}
		})
	}
}

// KIN-113: a waiver holds only while its head still has no check run: a check run that appears, or a new head,
// ends it, and main/master stay refused.
func TestACIWaiverEndsWithChecksOrANewHead(t *testing.T) {
	t.Parallel()
	a := openArtifact(t, "h1")
	a.RecordNoChecks("h1", t0)
	if err := a.WaiveCI(t0); err != nil {
		t.Fatal(err)
	}
	a.RecordCI("h1", CIPending, "", t0.Add(time.Minute))
	if a.CIWaivedOnHead() || a.CheckMergeable() == nil {
		t.Fatal("a waiver survived a running check")
	}
	a.RecordCI("h1", CIFailed, "lint", t0.Add(2*time.Minute))
	if a.CheckMergeable() == nil {
		t.Fatal("red CI mergeable after a waiver")
	}
	b := openArtifact(t, "h1")
	b.RecordNoChecks("h1", t0)
	if err := b.WaiveCI(t0); err != nil {
		t.Fatal(err)
	}
	b.HeadSHA = "h2"
	b.RecordNoChecks("h2", t0)
	if b.CIWaivedOnHead() || b.CheckMergeable() == nil {
		t.Fatal("a waiver of an older head counted")
	}
	c := openArtifact(t, "h1")
	c.RecordNoChecks("h1", t0)
	if err := c.WaiveCI(t0); err != nil {
		t.Fatal(err)
	}
	c.PRBase = "main"
	if err := c.CheckMergeable(); !errors.Is(err, ErrProtectedBranch) {
		t.Fatalf("protected base: %v", err)
	}
}
