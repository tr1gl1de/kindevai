package app_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// KIN-60 AC: a CI failure on the integration branch or a PR raises a project alert (Alert First: only an outbox
// event for the Alert Center, no task), and green CI there resolves it.
func TestCIAlertsFollowCompletedCheckSuites(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	repo := domain.Repository{ID: "r1", ProjectID: "p1", Owner: "acme", Name: "api", IntegrationBranch: "dev"}
	suite := func(branch string, pr int, conclusion string) domain.GitHubEvent {
		return domain.GitHubEvent{Event: domain.EventCheckSuite, Action: "completed", Repository: "acme/api",
			Branch: branch, PullRequest: pr, HeadSHA: "abc", Status: "completed", Conclusion: conclusion}
	}
	for _, ev := range []domain.GitHubEvent{suite("dev", 0, "failure"), suite("feature", 0, "failure"),
		suite("fix", 7, "success"), suite("dev", 0, "cancelled")} {
		if err := e.gh.CIAlerts(t.Context(), repo, ev); err != nil {
			t.Fatal(err)
		}
	}
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	if len(e.out.msgs) != 2 {
		t.Fatalf("messages = %+v", e.out.msgs)
	}
	var p struct {
		ProjectID, Repository, Branch, HeadSHA, Conclusion string
		PullRequest                                        int
	}
	m := e.out.msgs[0]
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if m.Topic != app.TopicCIFailed || m.Adapter != "alerts" || p.ProjectID != "p1" || p.Repository != "acme/api" ||
		p.Branch != "dev" || p.Conclusion != "failure" {
		t.Fatalf("failed event %s %s", m.Topic, m.Payload)
	}
	if m := e.out.msgs[1]; m.Topic != app.TopicCIPassed || m.Key == e.out.msgs[0].Key {
		t.Fatalf("passed event %+v", m)
	}
}

// Review: a re-run failing again on the same commit raises again (each webhook delivery is its own event); a suite
// on the integration branch keeps the branch key even when it heads a PR; a closed PR resolves its CI alert.
func TestCIAlertsRerunsBranchKeyAndClosedPR(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	repo := domain.Repository{ID: "r1", ProjectID: "p1", Owner: "acme", Name: "api", IntegrationBranch: "dev"}
	suite := func(delivery, branch string, pr int, conclusion string) domain.GitHubEvent {
		return domain.GitHubEvent{Event: domain.EventCheckSuite, Action: "completed", Repository: "acme/api",
			Branch: branch, PullRequest: pr, HeadSHA: "abc", Status: "completed", Conclusion: conclusion, Delivery: delivery}
	}
	closed := domain.GitHubEvent{Event: domain.EventPullRequest, Action: "closed", Repository: "acme/api", Branch: "fix",
		PullRequest: 4, Delivery: "d5"}
	for _, ev := range []domain.GitHubEvent{suite("d1", "fix", 4, "failure"), suite("d2", "fix", 4, "success"),
		suite("d3", "fix", 4, "failure"), suite("d4", "dev", 9, "failure"), closed} {
		if err := e.gh.CIAlerts(t.Context(), repo, ev); err != nil {
			t.Fatal(err)
		}
	}
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	var got []string
	for _, m := range e.out.msgs {
		var p struct{ PullRequest int }
		_ = json.Unmarshal(m.Payload, &p)
		got = append(got, m.Topic+"/"+strconv.Itoa(p.PullRequest))
	}
	want := []string{app.TopicCIFailed + "/4", app.TopicCIPassed + "/4", app.TopicCIFailed + "/4", app.TopicCIFailed + "/0",
		app.TopicCIPassed + "/4"}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
