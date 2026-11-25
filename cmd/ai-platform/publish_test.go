package main

import (
	"context"
	"errors"
	"testing"

	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// fakeOpener is the integrations context's pull request opener: one open pull request per head, reused.
type fakeOpener struct {
	heads []string
	err   error
}

func (f *fakeOpener) OpenPullRequest(_ context.Context, in integrationsapp.PullRequestInput) (integrations.PullRequestRef, error) {
	f.heads = append(f.heads, in.Repository+" "+in.Head)
	if f.err != nil {
		return integrations.PullRequestRef{}, f.err
	}
	return integrations.PullRequestRef{Number: 12, URL: "https://github.com/acme/app/pull/12", Base: "dev",
		Reused: len(f.heads) > 1}, nil
}

// KIN-36: the integrations context's refusals reach the workflow as its protected-branch error or as permanent
// failures; GitHub outages stay retryable.
func TestPullRequestErrorsAreTranslated(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err       error
		permanent bool
		protected bool
	}{
		{integrations.ErrProtectedBranch, true, true},
		{integrations.ErrNotFound, true, false},
		{integrations.ErrNotInstalled, true, false},
		{integrations.ErrNotConfigured, true, false},
		{errors.New("unreachable: timeout"), false, false},
	} {
		_, err := workflowPullRequests{gh: &fakeOpener{err: c.err}}.OpenPullRequest(t.Context(), workflowapp.PullRequestRequest{})
		if messaging.IsPermanent(err) != c.permanent || errors.Is(err, workflow.ErrProtectedBranch) != c.protected {
			t.Errorf("%v → %v", c.err, err)
		}
	}
}
