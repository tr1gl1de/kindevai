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

type fakeCIGitHub struct {
	state, detail string
	err           error
	merges        int
}

func (f *fakeCIGitHub) CIStatus(context.Context, string, string, string) (string, string, error) {
	return f.state, f.detail, f.err
}

func (f *fakeCIGitHub) MergePullRequest(context.Context, integrationsapp.MergeInput) (string, error) {
	f.merges++
	return "m1", f.err
}

// KIN-37: CI states and the integrations context's refusals reach the workflow as its own: a protected base and
// what retrying cannot fix are permanent, a moved head is ErrHeadMoved, GitHub outages stay retryable; a
// main/master base is refused before the integrations context is asked.
func TestCIAndMergeResultsAreTranslated(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]workflow.CIState{integrations.CIStatePassed: workflow.CIPassed,
		integrations.CIStateFailed: workflow.CIFailed, integrations.CIStatePending: workflow.CIPending} {
		st, err := workflowCI{gh: &fakeCIGitHub{state: state, detail: "d"}}.CIStatus(t.Context(), "p", "r", "h")
		if err != nil || st.State != want {
			t.Errorf("%s → %+v %v", state, st, err)
		}
	}
	for _, c := range []struct {
		err                         error
		permanent, protected, moved bool
	}{
		{integrations.ErrProtectedBranch, true, true, false},
		{integrations.ErrHeadMoved, false, false, true},
		{integrations.ErrNotMergeable, true, false, false},
		{integrations.ErrNotInstalled, true, false, false},
		{errors.New("unreachable: timeout"), false, false, false},
	} {
		_, err := workflowCI{gh: &fakeCIGitHub{err: c.err}}.MergePullRequest(t.Context(), workflowapp.MergeRequest{Base: "dev"})
		if messaging.IsPermanent(err) != c.permanent || errors.Is(err, workflow.ErrProtectedBranch) != c.protected ||
			errors.Is(err, workflowapp.ErrHeadMoved) != c.moved {
			t.Errorf("%v → %v", c.err, err)
		}
	}
	gh := &fakeCIGitHub{}
	if _, err := (workflowCI{gh: gh}).MergePullRequest(t.Context(), workflowapp.MergeRequest{Base: "main"}); !errors.Is(err, workflow.ErrProtectedBranch) || gh.merges != 0 {
		t.Fatalf("main: %v (%d merges)", err, gh.merges)
	}
}
