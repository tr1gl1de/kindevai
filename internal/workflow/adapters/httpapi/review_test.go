package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// KIN-35: the task page gets each step's review loop: phase and round, the latest handoff (the model's account with
// the runner's base/head, files and test evidence) and the latest verdict with its findings.
func TestReviewLoopView(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	p := domain.Plan{Risk: domain.RiskL1, MaxParallel: 1, Summary: "s", Steps: []domain.PlanStep{{Key: "api", Title: "API",
		Repository: "acme/app", Scope: []string{"api/**"}}}}
	rev, err := domain.NewPlanRevision("t1", 1, p, domain.RiskL1, []string{"acme/app"}, "u", now)
	if err != nil {
		t.Fatal(err)
	}
	e := domain.NewExecution("e1", rev, func() string { return "s1" }, now)
	if v := reviewLoopView(e.Steps[0]); v != nil {
		t.Fatalf("a step outside the loop has a view %+v", v)
	}
	step := func(agent, answer string, tests *domain.TestRun) {
		t.Helper()
		l, err := e.ClaimStep("s1", "r1", domain.StepClaim{Agent: domain.AgentChoice{Agent: agent}, Review: true, TestCommand: "make test"}, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_ = e.Start("s1", "r1", l.Fence, now, time.Minute)
		e.RecordAgentReport("s1", "r1", l.Fence, domain.AgentReport{Result: answer})
		if tests != nil {
			e.RecordTests("s1", "r1", l.Fence, *tests)
		}
		e.RecordChanges("s1", "r1", l.Fence, domain.WorkChanges{BaseSHA: "aaa", HeadSHA: "bbb", Files: []string{"api/x.go"}, Total: 1})
		if err := e.Succeed("s1", "r1", l.Fence, now); err != nil {
			t.Fatal(err)
		}
	}
	step("codex", `{"summary":"added","docs":["README"],"openQuestions":["limits?"]}`, &domain.TestRun{Command: "make test", DurationMS: 12, Output: "ok"})
	step("claude", `{"verdict":"changes_requested","summary":"almost","findings":[{"severity":"major","file":"api/x.go","line":4,"message":"nil"}]}`, nil)
	b, err := json.Marshal(reviewLoopView(e.Steps[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"phase":"implement"`, `"round":1`, `"maxRounds":2`, `"pinnedRunner":"r1"`, `"summary":"added"`,
		`"docs":["README"]`, `"openQuestions":["limits?"]`, `"baseSha":"aaa"`, `"headSha":"bbb"`, `"files":["api/x.go"]`,
		`"testStatus":"passed"`, `"command":"make test"`, `"exitCode":0`, `"verdict":"changes_requested"`,
		`"findings":[{"severity":"major","file":"api/x.go","line":4,"message":"nil"}]`, `"agent":"claude"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("view lacks %s:\n%s", want, b)
		}
	}
}

// KIN-36: the task page gets its change set: each published branch with head, state and the pull request link
// (https only); a step in its publish phase shows it.
func TestPullRequestsView(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	arts := []domain.GitArtifact{
		{Repository: "acme/app", Branch: "feature/t1--s1", HeadSHA: "h1", State: domain.ArtifactOpen, PRNumber: 7,
			PRURL: "https://github.com/acme/app/pull/7", PRBase: "dev", UpdatedAt: now},
		{Repository: "acme/web", Branch: "feature/t1--s2", HeadSHA: "h2", State: domain.ArtifactFailed,
			PRURL: "javascript:alert(1)", Error: "not installed", UpdatedAt: now},
	}
	b, err := json.Marshal(pullRequestViews(arts))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"repository":"acme/app"`, `"branch":"feature/t1--s1"`, `"headSha":"h1"`, `"state":"open"`,
		`"number":7`, `"url":"https://github.com/acme/app/pull/7"`, `"base":"dev"`, `"state":"failed"`, `"error":"not installed"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("view lacks %s:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "javascript") {
		t.Fatalf("unsafe link kept:\n%s", b)
	}
	if v := pullRequestViews(nil); v == nil || len(v) != 0 {
		t.Fatal("no change set must be an empty list")
	}
	if v := reviewLoopView(domain.Step{Review: true, Phase: domain.PhasePublish}); v.Phase != "publish" {
		t.Fatalf("phase %q", v.Phase)
	}
}
