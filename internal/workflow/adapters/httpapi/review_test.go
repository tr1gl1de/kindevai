package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/app"
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
	b, err := json.Marshal(pullRequestViews(arts, nil))
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
	if v := pullRequestViews(nil, nil); v == nil || len(v) != 0 {
		t.Fatal("no change set must be an empty list")
	}
	if v := reviewLoopView(domain.Step{Review: true, Phase: domain.PhasePublish}); v.Phase != "publish" {
		t.Fatalf("phase %q", v.Phase)
	}
}

// KIN-37: the change set shows CI on the current head (an older head's result is hidden), the step's CI fix rounds
// and the merge with its reason.
func TestPullRequestsViewShowsCIAndMerge(t *testing.T) {
	t.Parallel()
	e := &domain.Execution{ID: "e1", Steps: []domain.Step{{ID: "s1", CIRound: 2}}}
	arts := []domain.GitArtifact{
		{ExecutionID: "e1", StepID: "s1", Repository: "acme/app", Branch: "feature/t1--s1", HeadSHA: "h2", State: domain.ArtifactOpen,
			CI: domain.CIFailed, CIHeadSHA: "h1", CIDetail: "old", Merge: domain.MergeWaiting, MergeError: "integrate_dev is not approved"},
		{ExecutionID: "e1", StepID: "s9", Repository: "acme/web", Branch: "feature/t1--s9", HeadSHA: "h3", State: domain.ArtifactOpen,
			CI: domain.CIPassed, CIHeadSHA: "h3", Merge: domain.MergeMerged, MergeSHA: "m1"},
	}
	v := pullRequestViews(arts, e)
	if v[0].CI != "" || v[0].CIDetail != "" || v[0].CIRound != 2 || v[0].CIMaxRounds != domain.MaxCIFixRounds ||
		v[0].Merge != "waiting" || v[0].MergeError == "" {
		t.Fatalf("first %+v", v[0])
	}
	if v[1].CI != "passed" || v[1].Merge != "merged" || v[1].MergeSHA != "m1" || v[1].CIRound != 0 {
		t.Fatalf("second %+v", v[1])
	}
	if v[0].CINoChecksSince != nil || v[1].CINoChecksSince != nil {
		t.Fatalf("no-checks window shown for heads with checks: %+v", v)
	}
}

// KIN-105: a head without any check run shows since when, and after how long the task is blocked without CI.
func TestPullRequestsViewShowsAHeadWithoutCheckRuns(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	arts := []domain.GitArtifact{{ExecutionID: "e1", StepID: "s1", Repository: "acme/app", Branch: "feature/t1--s1",
		HeadSHA: "h1", State: domain.ArtifactOpen, CI: domain.CIPending, CIHeadSHA: "h1", NoChecksSince: since}}
	v := pullRequestViews(arts, nil)
	if v[0].CI != "pending" || v[0].CINoChecksSince == nil || !v[0].CINoChecksSince.Equal(since) ||
		v[0].CINoChecksBlockMinutes != 30 {
		t.Fatalf("view %+v", v[0])
	}
}

// KIN-38: the task page gets the YouTrack write-back (pending = "awaiting publication", failed with the error) and
// the documentation decision.
func TestPublicationAndDocsViews(t *testing.T) {
	t.Parallel()
	if publicationView(nil) != nil || docsView(nil) != nil {
		t.Fatal("views without data")
	}
	b, err := json.Marshal(publicationView(&app.Publication{Pending: 2, Failed: 1, Published: 3, LastError: "http_503"}))
	if err != nil || string(b) != `{"pending":2,"failed":1,"published":3,"lastError":"http_503"}` {
		t.Fatalf("publication %s %v", b, err)
	}
	b, err = json.Marshal(docsView(&domain.DocsDecision{Status: domain.DocsUpdated, Rejected: []string{"KIN-A-12"},
		Articles: []domain.ArticleChange{{Article: "KIN-A-19", Section: "KIN-38", Content: "long body"}}}))
	if err != nil || string(b) != `{"status":"updated","articles":[{"article":"KIN-A-19","section":"KIN-38"}],"rejected":["KIN-A-12"]}` {
		t.Fatalf("docs %s %v", b, err)
	}
	b, _ = json.Marshal(docsView(&domain.DocsDecision{Status: domain.DocsNotApplicable, Reason: "n/a"}))
	if string(b) != `{"status":"not_applicable","reason":"n/a","articles":[],"rejected":[]}` {
		t.Fatalf("docs %s", b)
	}
}
