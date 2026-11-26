package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	approvals "github.com/tr1gl1de/kindevai/internal/approvals/domain"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/fsstore"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/gitcli"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/scan"
	workspaceapp "github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Scope enforcement end to end (KIN-51): the Control Plane of the KIN-23 e2e tests with the scope check wired like
// `serve`, a runner with real git worktrees (KIN-29) cloned from a local origin, and the mock agent's scope
// scenario, whose first attempt writes outside.txt beyond the mock plan's scope "mock".

// scopeGit runs the system git with a fixed identity and no user configuration.
func scopeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=dev",
		"GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scopeOrigin creates <root>/acme/app.git whose dev branch holds README.md; it returns root.
func scopeOrigin(t *testing.T) string {
	t.Helper()
	root, src := t.TempDir(), t.TempDir()
	scopeGit(t, src, "init", "-q", "-b", "dev")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scopeGit(t, src, "add", "-A")
	scopeGit(t, src, "commit", "-q", "-m", "init")
	scopeGit(t, root, "clone", "-q", "--bare", src, filepath.Join(root, "acme", "app.git"))
	return root
}

// testWorkspaces adapts the workspace Manager to the session port as the ai-runner binary does.
type testWorkspaces struct{ m *workspaceapp.Manager }

func (w testWorkspaces) Prepare(ctx context.Context, r sessionapp.WorkspaceRequest) (string, protocol.WorkspacePrepared, error) {
	p, err := w.m.Prepare(ctx, workspaceapp.Request{Step: r.Step, Fence: r.Fence, Task: r.Task, Repository: r.Repository,
		Base: r.Base})
	return p.Dir, protocol.WorkspacePrepared{Repository: p.Repository, Branch: p.Branch, Base: p.Base, BaseSHA: p.BaseSHA}, err
}

func (w testWorkspaces) Finish(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	return testChanges(w.m.Finish(ctx, step, fence))
}

func (w testWorkspaces) Diff(ctx context.Context, step string, fence int64, from, to string, limit int) (string, bool, error) {
	return w.m.Diff(ctx, step, fence, from, to, limit)
}

func (w testWorkspaces) Changes(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	ch, err := testChanges(w.m.Changes(ctx, step, fence))
	if ch != nil {
		ch.Interim = true
	}
	return ch, err
}

func testChanges(res *workspaceapp.Result, err error) (*protocol.WorkspaceChanges, error) {
	if err != nil || res == nil {
		return nil, err
	}
	ch := &protocol.WorkspaceChanges{Branch: res.Branch, BaseSHA: res.BaseSHA, HeadSHA: res.HeadSHA, FilesTotal: res.Total,
		Truncated: res.Truncated}
	for _, f := range res.Files {
		ch.Files = append(ch.Files, protocol.ChangedFile{Path: f.Path, Status: f.Status})
	}
	return ch, nil
}

// scopeEnv is a Control Plane and a runner with worktrees from a local origin.
func scopeEnv(t *testing.T, changesEvery time.Duration) (*controlPlane, *runner) {
	t.Helper()
	return scopeEnvWith(t, changesEvery, func(w sessionapp.Workspaces) sessionapp.Workspaces { return w })
}

// scopeEnvWith is scopeEnv whose runner's workspaces are wrapped by wrap.
func scopeEnvWith(t *testing.T, changesEvery time.Duration, wrap func(sessionapp.Workspaces) sessionapp.Workspaces) (*controlPlane, *runner) {
	t.Helper()
	root := scopeOrigin(t)
	cp := startCPWith(t, t.TempDir(), "127.0.0.1:0", fakeSources{"p1 acme/app": integrationsapp.CheckoutSource{Branch: "dev"}})
	id, key := cp.seed(t, "p1")
	g, err := gitcli.New("file://"+root, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ws := testWorkspaces{&workspaceapp.Manager{Git: g, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: dir}}}
	r := startRunnerWith(t, dir, cp.addr, id, key, nil, nil, wrap(ws), changesEvery)
	return cp, r
}

func heldForApproval(d workflowapp.TaskDetail) bool {
	if d.Execution == nil {
		return false
	}
	st := d.Execution.Steps[0]
	return st.State == workflow.StepWaiting && st.WaitReason == workflow.WaitApproval
}

// scopeRequest is the task's pending scope_exceeded approval.
func (cp *controlPlane) scopeRequest(t *testing.T, taskID string) approvalsapp.ListItem {
	t.Helper()
	items, err := cp.approvals.List(t.Context(), ownerP, approvalsapp.ListQuery{TaskID: taskID,
		States: []approvals.State{approvals.StatePending}})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Reason == approvals.ReasonScopeExceeded {
			return it
		}
	}
	t.Fatalf("no pending scope_exceeded approval among %+v", items)
	return approvalsapp.ListItem{}
}

// The interim report stops the running step: the step is WAITING(approval), the runner's process is stopped
// (its later events are stale), the Approval Center lists a scope_exceeded request with the files beyond the
// scope and the audit log records it. Approving it makes the step READY; the next attempt runs on the same branch
// (outside.txt is now approved) and the task succeeds.
func TestChangesBeyondTheScopeStopTheRunningStep(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp, r := scopeEnv(t, 50*time.Millisecond)
	task := cp.mockTask(t, "p1", "scope")
	d := cp.waitTask(t, task, heldForApproval)
	st := d.Execution.Steps[0]
	if len(st.Attempts) != 1 || st.Attempts[0].State != workflow.AttemptCancelled {
		t.Fatalf("attempts %+v", st.Attempts)
	}
	req := cp.scopeRequest(t, task)
	if !slices.Equal(req.Paths, []string{"outside.txt"}) || req.StepKey != "scope" {
		t.Fatalf("approval %+v", req.Approval)
	}
	// The process was stopped without being released: its end reaches the Control Plane as a stale report.
	waitFor(t, "the first attempt's end", func() (bool, string) {
		var n int
		_ = cp.inst.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_attempt_events WHERE step_id = ? AND
			fence = 1 AND kind IN ('execution.completed', 'execution.failed') AND outcome = 'rejected'`, st.ID).Scan(&n)
		return n == 1, ""
	})
	var details string
	if err := cp.inst.DB.QueryRowContext(t.Context(), `SELECT details FROM audit_events WHERE kind = ? AND subject = ?`,
		approvalsapp.KindRequested, "approval:"+req.ID).Scan(&details); err != nil ||
		!strings.Contains(details, `"reason":"scope_exceeded"`) || !strings.Contains(details, `"paths":"outside.txt"`) {
		t.Fatalf("audit %s %v", details, err)
	}

	if _, err := cp.approvals.Decide(t.Context(), ownerP, req.ID, approvalsapp.DecideInput{Approve: true,
		Revision: req.Revision, PlanHash: req.PlanHash}); err != nil {
		t.Fatal(err)
	}
	d = cp.waitTask(t, task, succeeded)
	if a := d.Execution.Steps[0].Attempts; len(a) != 2 || a[1].State != workflow.AttemptSucceeded {
		t.Fatalf("attempts %+v", a)
	}
	branch := scopeGit(t, filepath.Join(r.dir, "repos", "acme", "app.git"), "ls-tree", "--name-only", "-r",
		"refs/heads/feature/"+task+"--"+st.ID)
	if !strings.Contains(branch, "outside.txt") || !strings.Contains(branch, "mock/scope.txt") {
		t.Fatalf("branch files:\n%s", branch)
	}
}

// Without interim reports the report at the end of the step stops it just the same; rejecting the extension
// cancels the task.
func TestChangesBeyondTheScopeAtTheEndOfTheStep(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp, r := scopeEnv(t, 0)
	task := cp.mockTask(t, "p1", "scope")
	cp.waitTask(t, task, stepRunning)
	waitFor(t, "the agent's files", func() (bool, string) {
		m, _ := filepath.Glob(filepath.Join(r.dir, "workspaces", "acme", "app", "*", "outside.txt"))
		return len(m) == 1, ""
	})
	time.Sleep(200 * time.Millisecond) // the step keeps running: nothing is reported before its end
	d := cp.detail(t, task)
	if !stepRunning(d) {
		t.Fatalf("step stopped before its end: %s", describe(d))
	}
	release(t, attemptDir(r, d))
	cp.waitTask(t, task, heldForApproval)
	req := cp.scopeRequest(t, task)
	if !slices.Equal(req.Paths, []string{"outside.txt"}) {
		t.Fatalf("approval %+v", req.Approval)
	}
	if _, err := cp.approvals.Decide(t.Context(), ownerP, req.ID, approvalsapp.DecideInput{Approve: false,
		Revision: req.Revision, PlanHash: req.PlanHash, Comment: "no"}); err != nil {
		t.Fatal(err)
	}
	cp.waitTask(t, task, func(d workflowapp.TaskDetail) bool { return d.Task.State == workflow.TaskCancelled })
}

// noFinalReport is a runner's workspaces that save and remove the worktree at the end of an attempt but report
// nothing (as after a crash between the two, or a runner that skips the report).
type noFinalReport struct{ sessionapp.Workspaces }

func (w noFinalReport) Finish(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	_, err := w.Workspaces.Finish(ctx, step, fence)
	return nil, err
}

// KIN-78: a step that worked in a worktree completes only with its final changes report. The runner sends
// workspace.prepared and execution.completed without workspace.changes: the step is held for approval with a
// scope_exceeded request for the missing report instead of succeeding unchecked. With the report the same
// scenario succeeds.
func TestCompletionWithoutAChangesReportStopsTheStep(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp, _ := scopeEnvWith(t, 0, func(w sessionapp.Workspaces) sessionapp.Workspaces { return noFinalReport{w} })
	task := cp.mockTask(t, "p1", "success")
	d := cp.waitTask(t, task, heldForApproval)
	st := d.Execution.Steps[0]
	if len(st.Attempts) != 1 || st.Attempts[0].State != workflow.AttemptCancelled ||
		!strings.Contains(st.Attempts[0].Error, "no changes report") {
		t.Fatalf("attempts %+v", st.Attempts)
	}
	if req := cp.scopeRequest(t, task); !slices.Equal(req.Paths, []string{approvalsapp.MissingChangesReport}) {
		t.Fatalf("approval %+v", req.Approval)
	}

	with, _ := scopeEnv(t, 0)
	with.waitTask(t, with.mockTask(t, "p1", "success"), succeeded)
}
