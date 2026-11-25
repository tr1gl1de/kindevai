package main

import (
	"context"

	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// attachScopeCheck makes the runner channel check every workspace.changes report against the step's approved
// scope (KIN-51): the approvals context evaluates the plan scope, approved extensions and the risk policy on the
// Control Plane; a violation holds the step, tells the runner to stop the process and asks the Owner.
func attachScopeCheck(ch *runnersapp.Channel, apv *approvalsapp.Service, wf *workflowapp.Service) {
	ch.Scope = runnerScope{apv: apv, wf: wf}
}

// runnerScope checks steps' changes. A planner attempt (KIN-34) has no scope to check: it runs read-only, its
// worktree is never pushed or merged and is discarded with the attempt, and only its answer is used.
type runnerScope struct {
	apv *approvalsapp.Service
	wf  *workflowapp.Service
}

// CheckChanges implements runnersapp.ScopeChecker. Files the report leaves out (FilesTotal beyond the listed ones,
// or a truncated report) are unreported and stop the step; a hold that loses the race to a newer token is stale.
func (r runnerScope) CheckChanges(ctx context.Context, runnerID, step string, fence int64, ch protocol.WorkspaceChanges) error {
	if planning(step) {
		return nil
	}
	files := make([]string, 0, len(ch.Files))
	for _, f := range ch.Files {
		files = append(files, f.Path)
	}
	unreported := max(ch.FilesTotal-len(ch.Files), 0)
	if ch.Truncated && unreported == 0 {
		unreported = 1
	}
	_, err := r.apv.CheckChanges(ctx, approvalsapp.StepChanges{StepID: step, RunnerID: runnerID, Fence: fence,
		Changed: files, Unreported: unreported})
	return attemptError(err)
}

// ChangesMissing implements runnersapp.ScopeChecker: an attempt that ended without a final changes report holds
// the step with a scope_exceeded request for the missing report (KIN-78).
// A planner attempt without a changes report completes as usual.
func (r runnerScope) ChangesMissing(ctx context.Context, runnerID, step string, fence int64) error {
	if planning(step) {
		return attemptError(r.wf.CompletePlanningAttempt(ctx, step, runnerID, fence))
	}
	_, err := r.apv.CheckChanges(ctx, approvalsapp.StepChanges{StepID: step, RunnerID: runnerID, Fence: fence, Missing: true})
	return attemptError(err)
}
