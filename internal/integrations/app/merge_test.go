package app_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func (g *githubAPI) CheckRuns(_ context.Context, tok domain.Secret, owner, name, sha string) ([]domain.CheckRun, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "checks "+owner+"/"+name+" "+sha+" "+tok.Reveal())
	return g.runs, len(g.runs), nil
}

func (g *githubAPI) PullRequest(_ context.Context, _ domain.Secret, owner, name string, number int) (domain.PullRequestState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "read "+owner+"/"+name+" #"+strconv.Itoa(number))
	return g.pr, nil
}

func (g *githubAPI) MergePullRequest(_ context.Context, tok domain.Secret, owner, name string, number int, sha, base string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "merge "+owner+"/"+name+" #"+strconv.Itoa(number)+" "+sha+"->"+base+" "+tok.Reveal())
	return "m-" + sha, nil
}

// KIN-37: CI status combines the head's check runs with a token of that repository; a merge reads the pull request
// first and merges only an open feature/ pull request into the integration branch at the expected head; a moved
// head, another base or main/master are refused before any merge is sent, and an already merged one is reused.
func TestCIStatusAndMergeOfAPlatformPullRequest(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	e.configure(t)
	e.addRepo(t, "p1", "acme", "api")
	e.api.read = domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 7, DefaultBranch: "dev"}}
	e.deliver(t, app.TopicRepositorySync)
	ctx := t.Context()
	e.api.runs = []domain.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}}
	if res, err := e.gh.CIStatus(ctx, "p1", "acme/api", "h1"); err != nil || res.State != domain.CIStatePassed || res.Output != "" {
		t.Fatalf("CI %+v %v", res, err)
	}
	// KIN-106: a failed check's output comes with the result, apart from the names.
	e.api.runs = []domain.CheckRun{{Name: "test", Status: "completed", Conclusion: "failure", Title: "1 failed", Summary: "FAIL x"}}
	if res, err := e.gh.CIStatus(ctx, "p1", "acme/api", "h1"); err != nil || res.State != domain.CIStateFailed ||
		res.Detail != "failed checks: test (failure)" || res.Output != "### test (failure): 1 failed\nFAIL x" {
		t.Fatalf("CI %+v %v", res, err)
	}
	if _, err := e.gh.CIStatus(ctx, "p2", "acme/api", "h1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another project's repository: %v", err)
	}
	open := domain.PullRequestState{Number: 9, State: "open", HeadRef: "feature/t1--s1", HeadSHA: "h1", Base: "dev"}
	in := app.MergeInput{ProjectID: "p1", Repository: "acme/api", Number: 9, HeadSHA: "h1"}
	e.api.pr = open
	if sha, err := e.gh.MergePullRequest(ctx, in); err != nil || sha != "m-h1" {
		t.Fatalf("merge %q %v", sha, err)
	}
	if last := e.api.calls[len(e.api.calls)-1]; last != "merge acme/api #9 h1->dev ghs_token-for-api" {
		t.Fatalf("call %q", last)
	}
	merges := 1
	for name, tc := range map[string]struct {
		pr   func(p *domain.PullRequestState)
		want error
	}{
		"head moved":    {func(p *domain.PullRequestState) { p.HeadSHA = "h2" }, domain.ErrHeadMoved},
		"base main":     {func(p *domain.PullRequestState) { p.Base = "main" }, domain.ErrProtectedBranch},
		"other base":    {func(p *domain.PullRequestState) { p.Base = "release" }, domain.ErrNotMergeable},
		"closed":        {func(p *domain.PullRequestState) { p.State = "closed" }, domain.ErrNotMergeable},
		"not a feature": {func(p *domain.PullRequestState) { p.HeadRef = "hotfix" }, domain.ErrNotMergeable},
	} {
		p := open
		tc.pr(&p)
		e.api.pr = p
		if _, err := e.gh.MergePullRequest(ctx, in); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.api.pr = domain.PullRequestState{Number: 9, State: "closed", Merged: true, MergeCommitSHA: "m0", HeadRef: "feature/t1--s1", HeadSHA: "h1", Base: "dev"}
	if sha, err := e.gh.MergePullRequest(ctx, in); err != nil || sha != "m0" {
		t.Fatalf("already merged: %q %v", sha, err)
	}
	n := 0
	for _, c := range e.api.calls {
		if len(c) > 5 && c[:5] == "merge" {
			n++
		}
	}
	if n != merges {
		t.Fatalf("merges sent %d, want %d: %v", n, merges, e.api.calls)
	}
}
