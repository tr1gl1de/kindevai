package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// plannerRouter binds the planner role to one CLI.
type plannerRouter struct{ agent, model string }

func (r plannerRouter) Route(_ context.Context, _, role, _ string) (workflowapp.AgentRoute, error) {
	if role != workflowapp.RolePlanner {
		return workflowapp.AgentRoute{}, nil
	}
	return workflowapp.AgentRoute{Chain: []workflow.AgentChoice{{Agent: r.agent, Model: r.model}}}, nil
}

type onePool struct {
	mu  sync.Mutex
	ids []string
}

func (p *onePool) Candidates(context.Context, string) ([]workflowapp.Candidate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []workflowapp.Candidate
	for _, id := range p.ids {
		out = append(out, workflowapp.Candidate{ID: id, Tools: []string{"git", "claude", "codex"}, DiskHeavyAllowed: true})
	}
	return out, nil
}

// cliAnswer replays a recorded CLI stream (the runner's fake CLI streams) through the dialect's parser and returns
// what the runner reports at the run's end: the native session and the final answer.
func cliAnswer(t *testing.T, d cliagent.Dialect, file string) protocol.AgentReport {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "runner", "session", "adapters", "cliagent", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	p := d.Parser()
	var tr sessiondomain.Transcript
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		for _, e := range p.Parse(sc.Bytes()) {
			tr.Apply(e)
		}
	}
	res := tr.Result(sessiondomain.End{})
	return protocol.AgentReport{Session: res.Session, Model: res.Model, Result: res.Text}
}

// plannerCP is the Control Plane's workflow wired like `serve` for the planner (KIN-34), without the HTTP server and
// the scheduler loop: the test runs scheduler passes and plays the runner through the channel's Executions port.
func plannerCP(t *testing.T, router plannerRouter) (*controlPlane, string) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	inst := bootstrapWhenUnlocked(t, t.Context(), t.TempDir())
	t.Cleanup(func() { _ = inst.Close() })
	audit := newAudit(inst, logger)
	msg := newMessaging(inst, logger)
	gh, _ := integrations.NewBaseURL(github.DefaultBaseURL)
	integ := newIntegrations(inst, msg, audit, gh, logger)
	apv := newApprovals(inst, msg, audit, integ, logger)
	channel := newRunnerChannel(inst, apv.Workflow, runners.DefaultRetention(), logger)
	pool := &onePool{}
	apv.Workflow.Runners, apv.Workflow.Dispatcher, apv.Workflow.Agents = pool, runnerDispatcher{ch: channel}, router
	attachScopeCheck(channel, apv.Approvals, apv.Workflow)
	wirePlanner(apv.Workflow, inst, msg, integ)
	wirePublishing(apv.Workflow, inst, msg, integ.GitHub)
	cp := &controlPlane{inst: inst, wf: apv.Workflow, approvals: apv.Approvals, hub: runnersapp.NewHub()}
	runnerID, _ := cp.seed(t, "p1")
	pool.ids = []string{runnerID}
	return cp, runnerID
}

// lastAssign decodes the newest execution.assign in the runner's command log.
func lastAssign(t *testing.T, cp *controlPlane, runnerID string) protocol.Assign {
	t.Helper()
	cmds, err := runnersdb.New(cp.inst.DB).CommandsAfter(t.Context(), runnerID, 0, 100)
	if err != nil || len(cmds) == 0 {
		t.Fatalf("commands %v %v", cmds, err)
	}
	c := cmds[len(cmds)-1]
	var a protocol.Assign
	if c.Kind != protocol.CommandAssign || json.Unmarshal(c.Body, &a) != nil {
		t.Fatalf("command %s %s", c.Kind, c.Body)
	}
	return a
}

// KIN-34 end to end on the Control Plane: the planner is assigned through the runner command log (adapter, model and
// role from routing, read-only: no scope, no bypass), the runner's report of the fake CLI stream is taken through
// the channel's ports, and the plan becomes revision 1 through the policy; the session is kept for the plan chat.
func TestPlannerThroughTheRunnerChannel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		dialect cliagent.Dialect
		file    string
		agent   string
		model   string
	}{
		{"claude", cliagent.Claude{}, "claude/plan.jsonl", "claude", "opus"},
		{"codex", cliagent.Codex{}, "codex/plan.jsonl", "codex", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cp, runnerID := plannerCP(t, plannerRouter{agent: c.agent, model: c.model})
			ctx := t.Context()
			task, err := cp.wf.CreateTaskAs(ctx, ownerP, "p1", "Add a login endpoint")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cp.wf.StartPlanning(ctx, ownerP, task.ID); err != nil {
				t.Fatal(err)
			}
			if err := cp.wf.Schedule(ctx); err != nil {
				t.Fatal(err)
			}
			a := lastAssign(t, cp, runnerID)
			if !strings.HasPrefix(a.Step, workflow.PlanningStepPrefix) || a.Adapter != c.agent || a.Model != c.model ||
				a.Role != "planner" || len(a.Scope) != 0 || a.BypassPermissions || a.Repository != "acme/app" ||
				!strings.Contains(a.Prompt, "Add a login endpoint") || !strings.Contains(a.Prompt, workflow.PlanSchemaVersion) {
				t.Fatalf("planner assignment %+v", a)
			}
			ex, sc := runnerExecutions{cp.wf}, runnerScope{apv: cp.approvals, wf: cp.wf}
			now := time.Now()
			if err := ex.StartAttempt(ctx, runnerID, a.Step, a.Fence, now, c.agent); err != nil {
				t.Fatal(err)
			}
			if err := sc.CheckChanges(ctx, runnerID, a.Step, a.Fence, protocol.WorkspaceChanges{}); err != nil {
				t.Fatalf("a planner that changed nothing: %v", err)
			}
			if err := ex.RecordAgentReport(ctx, runnerID, a.Step, a.Fence, cliAnswer(t, c.dialect, c.file)); err != nil {
				t.Fatal(err)
			}
			if err := sc.ChangesMissing(ctx, runnerID, a.Step, a.Fence); err != nil {
				t.Fatal(err)
			}
			d := cp.detail(t, task.ID)
			if d.Task.State != workflow.TaskReady || d.Revision == nil || d.Revision.CreatedBy() != "planner/"+c.agent ||
				len(d.Revision.Plan().Steps) != 2 || d.Planning == nil || d.Planning.State != workflow.PlanningSucceeded {
				t.Fatalf("after the plan: %s, planning %+v", describe(d), d.Planning)
			}
			s, err := cp.wf.PlannerSession(ctx, task.ID)
			if err != nil || s.Session == "" || s.RunnerID != runnerID {
				t.Fatalf("session %+v %v", s, err)
			}
			if err := ex.CompleteAttempt(ctx, runnerID, a.Step, a.Fence, now); err == nil {
				t.Fatal("a second completion of the same attempt must be stale")
			}
		})
	}
}

// KIN-34: the planner's repeat and replan continue the native session; the mock agent never gets one.
func TestAssignBodyCarriesThePlannerSession(t *testing.T) {
	t.Parallel()
	a := workflowapp.Assignment{StepID: "planner_R1", StepKey: "planner", Fence: 2, Agent: "claude", Role: "planner",
		Prompt: "# Plan", Resume: "sess-1"}
	if got := assignBody(a); got.Resume != "sess-1" || got.BypassPermissions {
		t.Fatalf("assign %+v", got)
	}
	a.Agent = ""
	if got := assignBody(a); got.Resume != "" {
		t.Fatalf("mock assign %+v", got)
	}
}

// TestRealCLIPlannerSmoke asks the installed Claude Code CLI for a plan with the real planner prompt, in safe mode
// like the runner, and checks the answer parses and validates as a plan (KIN-34). It spends subscription quota, so
// it runs only with KINDEV_REAL_CLI_SMOKE=1 (never in CI):
//
//	KINDEV_REAL_CLI_SMOKE=1 go test -run TestRealCLIPlannerSmoke -v ./cmd/ai-platform/
func TestRealCLIPlannerSmoke(t *testing.T) {
	t.Parallel()
	if os.Getenv("KINDEV_REAL_CLI_SMOKE") != "1" {
		t.Skip("set KINDEV_REAL_CLI_SMOKE=1 to run the installed CLI")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	ws := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":            "module example.com/app\n\ngo 1.24\n",
		"internal/hello.go": "package internal\n\n// Hello greets.\nfunc Hello() string { return \"hi\" }\n",
		"README.md":         "# app\nA tiny service.\n",
	} {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repos := []string{"acme/app"}
	prompt := workflow.PlannerPrompt(workflow.PlannerInput{Task: workflow.Task{Title: "Add a Goodbye function with a unit test"},
		Repositories: repos, Context: workflow.PlanningContext{IssueText: "Goodbye() returns \"bye\". Cover it with a test."}})
	d := cliagent.Claude{}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, d.Args(sessiondomain.AgentRequest{}, sessiondomain.PermissionsSafe)...)
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
	plan, err := workflow.ExtractPlan(res.Text)
	if err != nil {
		t.Fatalf("answer is no plan: %v\n%s", err, res.Text)
	}
	if err := workflow.ValidatePlan(plan, repos); err != nil {
		t.Fatalf("plan invalid: %v\n%s", err, res.Text)
	}
	t.Logf("session %s, %d steps, risk %s: %s", res.Session, len(plan.Steps), plan.Risk, plan.Summary)
}

// Review (KIN-34): a planner attempt that reports changed files is stopped through the channel's scope port.
func TestPlannerChangesThroughTheScopePort(t *testing.T) {
	t.Parallel()
	cp, runnerID := plannerCP(t, plannerRouter{agent: "claude"})
	ctx := t.Context()
	task, err := cp.wf.CreateTaskAs(ctx, ownerP, "p1", "Read only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.wf.StartPlanning(ctx, ownerP, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := cp.wf.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	a := lastAssign(t, cp, runnerID)
	sc := runnerScope{apv: cp.approvals, wf: cp.wf}
	if err := sc.CheckChanges(ctx, runnerID, a.Step, a.Fence, protocol.WorkspaceChanges{FilesTotal: 1,
		Files: []protocol.ChangedFile{{Path: "outside.txt", Status: "A"}}}); err != nil {
		t.Fatal(err)
	}
	cmds, _ := runnersdb.New(cp.inst.DB).CommandsAfter(ctx, runnerID, 0, 100)
	if last := cmds[len(cmds)-1]; last.Kind != protocol.CommandCancel || !strings.Contains(string(last.Body), "scope_exceeded") {
		t.Fatalf("last command %s %s", last.Kind, last.Body)
	}
	d := cp.detail(t, task.ID)
	if d.Planning.State != workflow.PlanningQueued || !strings.Contains(d.Planning.Feedback, "outside.txt") {
		t.Fatalf("planning %+v", d.Planning)
	}
}
