package main

import (
	"context"

	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
)

// attachScopeCheck makes the runner channel check every workspace.changes report against the step's approved
// scope (KIN-51): the approvals context evaluates the plan scope, approved extensions and the risk policy on the
// Control Plane; a violation holds the step, tells the runner to stop the process and asks the Owner.
func attachScopeCheck(ch *runnersapp.Channel, apv *approvalsapp.Service) {
	ch.Scope = runnerScope{apv}
}

type runnerScope struct{ apv *approvalsapp.Service }

// CheckChanges implements runnersapp.ScopeChecker. Files the report leaves out (FilesTotal beyond the listed ones,
// or a truncated report) are unreported and stop the step; a hold that loses the race to a newer token is stale.
func (r runnerScope) CheckChanges(ctx context.Context, runnerID, step string, fence int64, ch protocol.WorkspaceChanges) error {
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
func (r runnerScope) ChangesMissing(ctx context.Context, runnerID, step string, fence int64) error {
	_, err := r.apv.CheckChanges(ctx, approvalsapp.StepChanges{StepID: step, RunnerID: runnerID, Fence: fence, Missing: true})
	return attemptError(err)
}
