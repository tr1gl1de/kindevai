package main

import (
	"context"
	"errors"
	"fmt"

	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// wirePublishing turns on publishing a review-loop step's approved work (KIN-36): the change set is stored with the
// workflow and the outbox opens each published branch's pull request with the GitHub App. Call it before msg.start.
func wirePublishing(wf *workflowapp.Service, inst *bootstrap.Installation, msg *messagingModule, gh *integrationsapp.GitHubService) {
	wf.ChangeSets = workflowdb.New(inst.DB)
	wf.PullRequests = workflowPullRequests{gh: gh}
	msg.Outbox.Handle(workflowapp.TopicOpenPullRequest, wf.HandleOpenPullRequest)
}

// pullRequestOpener is the integrations context's GitHubService as publishing needs it.
type pullRequestOpener interface {
	OpenPullRequest(ctx context.Context, in integrationsapp.PullRequestInput) (integrations.PullRequestRef, error)
}

// workflowPullRequests translates the integrations context's pull requests and errors for the workflow: a
// main/master base or head becomes the workflow's ErrProtectedBranch, and what retrying cannot fix (repository not
// registered, App not installed or not configured) is permanent.
type workflowPullRequests struct{ gh pullRequestOpener }

var _ workflowapp.PullRequests = workflowPullRequests{}

func (p workflowPullRequests) OpenPullRequest(ctx context.Context, r workflowapp.PullRequestRequest) (workflowapp.PullRequest, error) {
	pr, err := p.gh.OpenPullRequest(ctx, integrationsapp.PullRequestInput{ProjectID: r.ProjectID, Repository: r.Repository,
		Head: r.Branch, Title: r.Title, Body: r.Body})
	switch {
	case errors.Is(err, integrations.ErrProtectedBranch):
		return workflowapp.PullRequest{}, messaging.Permanent(fmt.Errorf("%w: %w", workflow.ErrProtectedBranch, err))
	case errors.Is(err, integrations.ErrNotFound), errors.Is(err, integrations.ErrNotInstalled),
		errors.Is(err, integrations.ErrNotConfigured):
		return workflowapp.PullRequest{}, messaging.Permanent(err)
	case err != nil:
		return workflowapp.PullRequest{}, err
	}
	return workflowapp.PullRequest{Number: pr.Number, URL: pr.URL, Base: pr.Base, Reused: pr.Reused}, nil
}

var _ runnersapp.PushReporter = runnerExecutions{}

// RecordPush stores a publish attempt's push report (KIN-36); a planner attempt never pushes (checked only).
func (r runnerExecutions) RecordPush(ctx context.Context, runnerID, step string, fence int64, p protocol.WorkspacePushed) error {
	if planning(step) {
		return attemptError(r.wf.CheckPlanningAttempt(ctx, step, runnerID, fence))
	}
	return attemptError(r.wf.RecordPush(ctx, step, runnerID, fence, workflow.Push{Repository: p.Repository,
		Branch: p.Branch, HeadSHA: p.HeadSHA}))
}
