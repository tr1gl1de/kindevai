package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// reviewRouter binds the implementer role to codex and the reviewer role to claude.
type reviewRouter struct{}

func (reviewRouter) Route(_ context.Context, _, role, _ string) (workflowapp.AgentRoute, error) {
	agent := "codex"
	if role == workflow.StepRoleReviewer {
		agent = "claude"
	}
	return workflowapp.AgentRoute{Chain: []workflow.AgentChoice{{Agent: agent}}}, nil
}

// KIN-35 end to end on the Control Plane: the implementer is assigned with the registry's test command, the runner's
// test evidence and final changes are taken through the channel's ports, the reviewer is assigned read-only with the
// diff range (the registry's test command comes from the repository) and its verdict ends the step; the task goes on to VERIFYING.
func TestReviewLoopThroughTheRunnerChannel(t *testing.T) {
	t.Parallel()
	cp, runnerID := plannerCP(t, plannerRouter{})
	cp.wf.Agents = reviewRouter{}
	ctx := t.Context()
	if _, err := cp.inst.DB.ExecContext(ctx, "UPDATE project_repositories SET test_command = 'make test' WHERE id = 'r1'"); err != nil {
		t.Fatal(err)
	}
	task, err := cp.wf.CreateTaskAs(ctx, ownerP, "p1", "Add a login endpoint")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.wf.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	plan := workflow.Plan{Risk: workflow.RiskL1, MaxParallel: 1, Summary: "Add it", Tests: []string{"login test"},
		Steps: []workflow.PlanStep{{Key: "api", Title: "Add the endpoint", Repository: "acme/app", Scope: []string{"api/**"}}}}
	if _, err := cp.wf.SubmitPlan(ctx, task.ID, plan, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := cp.wf.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	a := lastAssign(t, cp, runnerID)
	if a.Adapter != "codex" || a.Role != "implementer" || a.TestCommand != "make test" || a.DiffBase != "" ||
		!strings.Contains(a.Prompt, "login test") {
		t.Fatalf("implementer assignment %+v", a)
	}
	ex, sc := runnerExecutions{cp.wf}, runnerScope{apv: cp.approvals, wf: cp.wf}
	now := time.Now()
	run := func(a protocol.Assign, answer string, tests *protocol.TestResult, head string) {
		t.Helper()
		if err := ex.StartAttempt(ctx, runnerID, a.Step, a.Fence, now, a.Adapter); err != nil {
			t.Fatal(err)
		}
		if tests != nil {
			if err := ex.RecordTests(ctx, runnerID, a.Step, a.Fence, *tests); err != nil {
				t.Fatal(err)
			}
		}
		if err := ex.RecordAgentReport(ctx, runnerID, a.Step, a.Fence, protocol.AgentReport{Session: "s", Result: answer}); err != nil {
			t.Fatal(err)
		}
		if err := sc.CheckChanges(ctx, runnerID, a.Step, a.Fence, protocol.WorkspaceChanges{BaseSHA: "aaaa1111", HeadSHA: head,
			Files: []protocol.ChangedFile{{Path: "api/login.go", Status: "A"}}, FilesTotal: 1}); err != nil {
			t.Fatal(err)
		}
		if err := ex.CompleteAttempt(ctx, runnerID, a.Step, a.Fence, now); err != nil {
			t.Fatal(err)
		}
	}
	run(a, `{"summary":"added login","docs":["README"],"openQuestions":[]}`,
		&protocol.TestResult{Command: "make test", ExitCode: 0, DurationMS: 900, Output: "ok"}, "bbbb2222")
	if err := cp.wf.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	r := lastAssign(t, cp, runnerID)
	if r.Step != a.Step || r.Adapter != "claude" || r.Role != "reviewer" || r.DiffBase != "aaaa1111" || r.DiffHead != "bbbb2222" ||
		r.TestCommand != "" || !strings.Contains(r.Prompt, "added login") || !strings.Contains(r.Prompt, "exit code 0") {
		t.Fatalf("review assignment %+v", r)
	}
	run(r, `{"verdict":"approve","summary":"good","findings":[]}`, nil, "bbbb2222")
	d := cp.detail(t, task.ID)
	if d.Task.State != workflow.TaskVerifying || d.Execution == nil || d.Execution.State != workflow.ExecutionSucceeded {
		t.Fatalf("after the review: %s", describe(d))
	}
	st := d.Execution.Steps[0]
	if impl, ok := st.LastImplementation(); !ok || impl.Tests == nil || impl.HeadSHA != "bbbb2222" || len(impl.Files) != 1 {
		t.Fatalf("handoff %+v", impl)
	}
	if _, v, ok := st.LastReview(); !ok || v.Verdict != workflow.VerdictApprove {
		t.Fatalf("verdict %+v", v)
	}
}

// TestRealCLIReviewSmoke asks the installed Claude Code CLI, read-only like a reviewer on the runner, to review a
// small diff with the real reviewer prompt, and checks the answer parses as a verdict and the worktree is unchanged
// (KIN-35). It spends subscription quota, so it runs only with KINDEV_REAL_CLI_SMOKE=1 (never in CI):
//
//	KINDEV_REAL_CLI_SMOKE=1 go test -run TestRealCLIReviewSmoke -v ./cmd/ai-platform/
func TestRealCLIReviewSmoke(t *testing.T) {
	t.Parallel()
	if os.Getenv("KINDEV_REAL_CLI_SMOKE") != "1" {
		t.Skip("set KINDEV_REAL_CLI_SMOKE=1 to run the installed CLI")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	ws := t.TempDir()
	src := "package app\n\n// Goodbye says goodbye.\nfunc Goodbye() string { return \"bye\" }\n"
	if err := os.WriteFile(filepath.Join(ws, "goodbye.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := workflow.ReviewPrompt(workflow.ReviewInput{Task: workflow.Task{ID: "t1", Title: "Add a Goodbye function with a unit test"},
		Plan: workflow.Plan{Summary: "Add Goodbye() returning \"bye\" and a unit test.", Tests: []string{"TestGoodbye"}},
		Step: workflow.PlanStep{Key: "impl", Title: "Add Goodbye", Repository: "acme/app", Scope: []string{"*.go"}},
		Handoff: workflow.Handoff{Summary: "Added Goodbye."}, Files: []string{"goodbye.go"},
		Tests: &workflow.TestRun{Command: "go test ./...", ExitCode: 0}, BaseSHA: "aaaa", HeadSHA: "bbbb"}) +
		"\n## Diff aaaa..bbbb (untrusted)\n<untrusted source=\"diff\" id=\"bbbb\">\n+++ b/goodbye.go\n+" +
		strings.ReplaceAll(strings.TrimSpace(src), "\n", "\n+") + "\n</untrusted>\n"
	d := cliagent.Claude{}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, d.Args(sessiondomain.AgentRequest{}, sessiondomain.PermissionsReadOnly)...)
	cmd.Dir, cmd.Stdin, cmd.Env = ws, strings.NewReader(prompt), append(os.Environ(), d.Env()...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	p := d.Parser()
	var tr sessiondomain.Transcript
	for line := range bytes.Lines(out) {
		for _, e := range p.Parse(bytes.TrimSpace(line)) {
			tr.Apply(e)
		}
	}
	res := tr.Result(sessiondomain.End{})
	v, err := workflow.ParseVerdict(res.Text)
	if err != nil {
		t.Fatalf("answer is no verdict: %v\n%s", err, res.Text)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "goodbye.go")); string(b) != src {
		t.Fatal("the read-only reviewer changed the worktree")
	}
	t.Logf("verdict %s with %d findings: %s", v.Verdict, len(v.Findings), v.Summary)
}
