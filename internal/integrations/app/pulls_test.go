package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func (g *githubAPI) OpenPullRequest(_ context.Context, tok domain.Secret, owner, name string, in domain.PullRequestInput) (domain.PullRequestRef, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "pr "+owner+"/"+name+" "+in.Head+"->"+in.Base+" "+tok.Reveal())
	return domain.PullRequestRef{Number: 5, URL: "https://github.com/" + owner + "/" + name + "/pull/5", Base: in.Base}, nil
}

// KIN-36: a pull request goes into the registered repository's integration branch with a token for that repository
// only; another project's repository, a head that is no feature branch and a main/master base are refused before
// GitHub is called.
func TestOpenPullRequestTargetsTheIntegrationBranch(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	e.configure(t)
	r := e.addRepo(t, "p1", "acme", "api")
	ctx := t.Context()
	in := app.PullRequestInput{ProjectID: "p1", Repository: "acme/api", Head: "feature/t1--s1", Title: "T", Body: "B"}
	if _, err := e.gh.OpenPullRequest(ctx, in); !errors.Is(err, domain.ErrNotInstalled) {
		t.Fatalf("not installed: %v", err)
	}
	e.api.read = domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 7, DefaultBranch: "dev"}}
	e.deliver(t, app.TopicRepositorySync)
	pr, err := e.gh.OpenPullRequest(ctx, in)
	if err != nil || pr.Number != 5 || pr.Base != "dev" {
		t.Fatalf("%+v %v", pr, err)
	}
	if last := e.api.calls[len(e.api.calls)-1]; last != "pr acme/api feature/t1--s1->dev ghs_token-for-api" {
		t.Fatalf("call %q", last)
	}
	calls := len(e.api.calls)
	for name, bad := range map[string]app.PullRequestInput{
		"other project": {ProjectID: "p2", Repository: "acme/api", Head: "feature/x"},
		"head main":     {ProjectID: "p1", Repository: "acme/api", Head: "main"},
		"head not work": {ProjectID: "p1", Repository: "acme/api", Head: "release/1"},
	} {
		if _, err := e.gh.OpenPullRequest(ctx, bad); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	r, _ = e.store.Repository(ctx, "p1", r.ID)
	r.IntegrationBranch = "master"
	if err := e.store.SaveRepository(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.gh.OpenPullRequest(ctx, in); !errors.Is(err, domain.ErrProtectedBranch) {
		t.Fatalf("base master: %v", err)
	}
	if len(e.api.calls) != calls {
		t.Fatalf("GitHub called for refused requests: %v", e.api.calls[calls:])
	}
}
