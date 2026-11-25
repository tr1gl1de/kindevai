package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// wireCI turns on the CI fix loop and the policy-gated merge into dev (KIN-37): CI of the platform's pull requests
// reaches the workflow (not the Alert Center), the outbox reads CI and merges with the GitHub App, and the approvals
// context is the integrate_dev gate. Call it after wirePublishing and before msg.start.
func wireCI(wf *workflowapp.Service, gate workflowapp.IntegrateGate, msg *messagingModule, gh *integrationsapp.GitHubService) {
	wf.CI, wf.Integrate = workflowCI{gh: gh}, gate
	gh.PlatformCI = wf.CIEvent
	msg.Outbox.Handle(workflowapp.TopicCICheck, wf.HandleCICheck)
	msg.Outbox.Handle(workflowapp.TopicMerge, wf.HandleMerge)
}

// startCIReconciler queues a CI check of the open pull requests of verifying tasks every interval (a lost webhook,
// a later integrate_dev approval); the returned stop waits for the loop.
func startCIReconciler(ctx context.Context, wf *workflowapp.Service, every time.Duration, logger *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := wf.ReconcileCI(ctx); err != nil && ctx.Err() == nil {
				logger.Error("CI reconciliation", "err", err)
			}
		}
	})
	return func() { cancel(); wg.Wait() }
}

// ciGitHub is the integrations context's GitHubService as the CI loop needs it.
type ciGitHub interface {
	CIStatus(ctx context.Context, projectID, repository, sha string) (state, detail string, err error)
	MergePullRequest(ctx context.Context, in integrationsapp.MergeInput) (string, error)
}

// workflowCI translates the integrations context's CI and merge results and errors for the workflow: a main/master
// base is the workflow's ErrProtectedBranch, a moved head ErrHeadMoved, and what retrying cannot fix is permanent.
type workflowCI struct{ gh ciGitHub }

var _ workflowapp.CIChecks = workflowCI{}

func (c workflowCI) CIStatus(ctx context.Context, projectID, repository, sha string) (workflowapp.CIStatus, error) {
	st, detail, err := c.gh.CIStatus(ctx, projectID, repository, sha)
	if err != nil {
		return workflowapp.CIStatus{}, ciError(err)
	}
	switch st {
	case integrations.CIStatePassed:
		return workflowapp.CIStatus{State: workflow.CIPassed}, nil
	case integrations.CIStateFailed:
		return workflowapp.CIStatus{State: workflow.CIFailed, Detail: detail}, nil
	case integrations.CIStateNone:
		return workflowapp.CIStatus{State: workflow.CIPending, NoChecks: true}, nil
	}
	return workflowapp.CIStatus{State: workflow.CIPending}, nil
}

func (c workflowCI) MergePullRequest(ctx context.Context, r workflowapp.MergeRequest) (string, error) {
	if workflow.IsProtectedBranch(r.Base) {
		return "", messaging.Permanent(fmt.Errorf("merge into %q: %w", r.Base, workflow.ErrProtectedBranch))
	}
	sha, err := c.gh.MergePullRequest(ctx, integrationsapp.MergeInput{ProjectID: r.ProjectID, Repository: r.Repository,
		Number: r.Number, HeadSHA: r.HeadSHA})
	if err != nil {
		return "", ciError(err)
	}
	return sha, nil
}

func ciError(err error) error {
	switch {
	case errors.Is(err, integrations.ErrProtectedBranch):
		return messaging.Permanent(fmt.Errorf("%w: %w", workflow.ErrProtectedBranch, err))
	case errors.Is(err, integrations.ErrHeadMoved):
		return fmt.Errorf("%w: %w", workflowapp.ErrHeadMoved, err)
	case errors.Is(err, integrations.ErrNotMergeable), errors.Is(err, integrations.ErrNotFound),
		errors.Is(err, integrations.ErrNotInstalled), errors.Is(err, integrations.ErrNotConfigured):
		return messaging.Permanent(err)
	}
	return err
}

// ciReconcileEvery is the CI reconciliation period of the served Control Plane.
const ciReconcileEvery = workflowapp.ReconcileCIEvery
