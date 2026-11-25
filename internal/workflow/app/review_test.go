package app

import (
	"context"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type fakeTestCommands map[string]string

func (f fakeTestCommands) TestCommand(_ context.Context, projectID, repository string) (string, error) {
	return f[projectID+"/"+repository], nil
}

// runAssigned starts the step's current claim on runner, records the CLI's answer, the runner's test evidence and
// changes, and completes it.
func runAssigned(t *testing.T, e schedEnv, taskID, key, runner, answer string, tests *domain.TestRun, ch domain.WorkChanges) {
	t.Helper()
	ctx := t.Context()
	_, ex := e.state(t, taskID)
	st := ex.StepByKey(key)
	if err := e.svc.StartStep(ctx, st.ID, runner, st.Fence); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RecordAgentReport(ctx, st.ID, runner, st.Fence, domain.AgentReport{Result: answer}); err != nil {
		t.Fatal(err)
	}
	if tests != nil {
		if err := e.svc.RecordTests(ctx, st.ID, runner, st.Fence, *tests); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.RecordChanges(ctx, st.ID, runner, st.Fence, ch); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteStep(ctx, st.ID, runner, st.Fence); err != nil {
		t.Fatal(err)
	}
}

var work = domain.WorkChanges{BaseSHA: "b0", HeadSHA: "h1", Files: []string{"src/a.go"}, Total: 1}

// Acceptance (KIN-35): the implementer gets the plan step and the repository's test command; its work goes to an
// independent reviewer (another CLI than the author, on the runner that holds the branch) with the diff range, the
// runner's test evidence and the handoff; changes requested start a fix round with the findings; approval ends it.
func TestReviewLoopRunsImplementationTestsReviewAndFixes(t *testing.T) {
	t.Parallel()
	e, r := newRoutedEnv()
	e.svc.TestCommands = fakeTestCommands{"p1/api": "make test"}
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	got := e.disp.takeAssigned()
	if len(got) != 1 || got[0].Role != "implementer" || got[0].TestCommand != "make test" || got[0].Agent != "codex" ||
		!strings.Contains(got[0].Prompt, "the runner runs `make test`") {
		t.Fatalf("implementer assignment %+v", got)
	}
	runAssigned(t, e, task.ID, "only", "r1", `{"summary":"added a","docs":["README"],"openQuestions":[]}`,
		&domain.TestRun{Command: "make test", ExitCode: 0, Output: "ok"}, work)

	// Another runner appears first in the pool: the review still runs on r1, which holds the branch.
	e.pool.set("p1", cliRunner("r2", "claude", "codex"), cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	got = e.disp.takeAssigned()
	if len(got) != 1 || got[0].Role != "reviewer" || got[0].Agent != "claude" || got[0].DiffBase != "b0" || got[0].DiffHead != "h1" ||
		got[0].TestCommand != "" || !strings.Contains(got[0].Prompt, "added a") || !strings.Contains(got[0].Prompt, "exit code 0") {
		t.Fatalf("review assignment %+v", got)
	}
	if last := r.calls[len(r.calls)-1]; last != "p1 reviewer codex" {
		t.Fatalf("reviewer routed as %q, want author codex", last)
	}
	if _, ex := e.state(t, task.ID); ex.Steps[0].RunnerID != "r1" {
		t.Fatalf("review claimed by %q", ex.Steps[0].RunnerID)
	}
	runAssigned(t, e, task.ID, "only", "r1", `{"verdict":"changes_requested","summary":"s","findings":[{"severity":"major","file":"src/a.go","line":7,"message":"check the error"}]}`,
		nil, work)

	e.schedule(t)
	got = e.disp.takeAssigned()
	if len(got) != 1 || got[0].Role != "implementer" || got[0].TestCommand != "make test" ||
		!strings.Contains(got[0].Prompt, "src/a.go:7 check the error") || !strings.Contains(got[0].Prompt, "fix round 1") {
		t.Fatalf("fix assignment %+v", got)
	}
	runAssigned(t, e, task.ID, "only", "r1", `{"summary":"fixed","docs":[],"openQuestions":[]}`,
		&domain.TestRun{Command: "make test"}, domain.WorkChanges{BaseSHA: "b0", HeadSHA: "h2", Files: []string{"src/a.go"}, Total: 1})
	e.schedule(t)
	if got = e.disp.takeAssigned(); len(got) != 1 || got[0].Role != "reviewer" || got[0].DiffHead != "h2" {
		t.Fatalf("second review %+v", got)
	}
	runAssigned(t, e, task.ID, "only", "r1", `{"verdict":"approve","summary":"good","findings":[]}`, nil,
		domain.WorkChanges{BaseSHA: "b0", HeadSHA: "h2", Files: []string{"src/a.go"}, Total: 1})
	tk, ex := e.state(t, task.ID)
	if ex.State != domain.ExecutionSucceeded || tk.State != domain.TaskVerifying {
		t.Fatalf("after approval: execution %s task %s", ex.State, tk.State)
	}
}

// Acceptance (KIN-35): a step whose branch lives on a runner that went away BLOCKs the task with a clear reason.
func TestReviewWaitsForTheRunnerHoldingTheBranch(t *testing.T) {
	t.Parallel()
	e, _ := newRoutedEnv()
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	e.disp.takeAssigned()
	runAssigned(t, e, task.ID, "only", "r1", `{"summary":"x"}`, nil, work)
	e.pool.set("p1", cliRunner("r2", "claude", "codex"))
	e.schedule(t)
	if got := e.disp.takeAssigned(); len(got) != 0 {
		t.Fatalf("assigned elsewhere: %+v", got)
	}
	tk, _ := e.state(t, task.ID)
	if tk.State != domain.TaskBlocked || tk.BlockReason != domain.WaitRunner || !strings.Contains(tk.Reason.Detail, "runner r1") {
		t.Fatalf("task %+v", tk)
	}
}

// Acceptance (KIN-35): failing tests FAIL the task with the runner's evidence once the retry is used up.
func TestFailingTestsFailTheTaskWithTheEvidence(t *testing.T) {
	t.Parallel()
	e, _ := newRoutedEnv()
	e.svc.TestCommands = fakeTestCommands{"p1/api": "go test ./..."}
	task := e.task(t, onlyStep())
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	for range 2 {
		e.schedule(t)
		e.disp.takeAssigned()
		runAssigned(t, e, task.ID, "only", "r1", `{"summary":"tests pass"}`,
			&domain.TestRun{Command: "go test ./...", ExitCode: 1, Output: "--- FAIL: TestX"}, work)
	}
	tk, _ := e.state(t, task.ID)
	if tk.State != domain.TaskFailed || !strings.Contains(tk.Reason.Detail, "tests failed: go test ./... exited with 1: --- FAIL: TestX") {
		t.Fatalf("task %s %+v", tk.State, tk.Reason)
	}
}
