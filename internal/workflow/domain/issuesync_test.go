package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// KIN-38 AC4: DOCS_NOT_APPLICABLE needs a reason.
func TestDocsNotApplicableRequiresAReason(t *testing.T) {
	t.Parallel()
	for _, r := range []string{"", "   ", "\n\t"} {
		if _, err := NewDocsNotApplicable("t1", "e1", r); !errors.Is(err, ErrDocsReasonRequired) {
			t.Fatalf("reason %q: err = %v, want ErrDocsReasonRequired", r, err)
		}
	}
	d, err := NewDocsNotApplicable("t1", "e1", "  internal refactoring only  ")
	if err != nil || d.Status != DocsNotApplicable || d.Reason != "internal refactoring only" {
		t.Fatalf("decision = %+v, %v", d, err)
	}
}

// KIN-38 AC3: the implementer's article changes are kept only for the articles the approved plan names in its docs
// impact; without changes the decision is DOCS_NOT_APPLICABLE with the handoff's reason, or unstated.
func TestDecideDocsKeepsChangesWithinTheApprovedScope(t *testing.T) {
	t.Parallel()
	h := Handoff{Summary: "s", Articles: []ArticleChange{
		{Article: "KIN-A-19", Section: "KIN-38 — write-back", Content: "Body"},
		{Article: "KIN-A-12", Section: "Rules", Content: "rewrite the rules"},
		{Article: "not an id", Section: "x", Content: "y"},
	}}
	d := DecideDocs("t1", "e1", h, []string{"KIN-A-19: integrations, YouTrack section"})
	if d.Status != DocsUpdated || len(d.Articles) != 1 || d.Articles[0].Article != "KIN-A-19" {
		t.Fatalf("decision = %+v", d)
	}
	if strings.Join(d.Rejected, ",") != "KIN-A-12,not an id" {
		t.Fatalf("rejected = %v", d.Rejected)
	}

	d = DecideDocs("t1", "e1", Handoff{Summary: "s", DocsNotApplicable: "no behaviour change"}, nil)
	if d.Status != DocsNotApplicable || d.Reason != "no behaviour change" {
		t.Fatalf("not applicable = %+v", d)
	}
	d = DecideDocs("t1", "e1", Handoff{Summary: "s", Articles: h.Articles[1:2]}, nil)
	if d.Status != DocsUnstated || len(d.Rejected) != 1 {
		t.Fatalf("only out-of-scope changes = %+v", d)
	}
	if d := DecideDocs("t1", "e1", Handoff{Summary: "s"}, nil); d.Status != DocsUnstated {
		t.Fatalf("nothing stated = %+v", d)
	}
}

func TestParseHandoffReadsArticleChangesAndTheNotApplicableReason(t *testing.T) {
	t.Parallel()
	h := ParseHandoff(`{"summary":"done","docs":[],"articles":[{"article":"KIN-A-19","section":"S","content":"C"}],` +
		`"docsNotApplicable":"n/a"}`)
	if len(h.Articles) != 1 || h.Articles[0] != (ArticleChange{Article: "KIN-A-19", Section: "S", Content: "C"}) ||
		h.DocsNotApplicable != "n/a" {
		t.Fatalf("handoff = %+v", h)
	}
	long := `{"summary":"s","articles":[` + strings.Repeat(`{"article":"KIN-A-1","section":"S","content":"C"},`, 20) +
		`{"article":"KIN-A-1","section":"S","content":"C"}]}`
	if h := ParseHandoff(long); len(h.Articles) != maxArticleChanges {
		t.Fatalf("articles not bounded: %d", len(h.Articles))
	}
	big := `{"summary":"s","articles":[{"article":"KIN-A-1","section":"S","content":"` +
		strings.Repeat("x", maxArticleContent+10) + `"}]}`
	if h := ParseHandoff(big); len(h.Articles[0].Content) > maxArticleContent+3 {
		t.Fatalf("content not bounded: %d", len(h.Articles[0].Content))
	}
}

// KIN-38 AC1: the plan comment carries a short summary and the approval link; the final comment the pull requests,
// the agent time and the documentation decision.
func TestIssueComments(t *testing.T) {
	t.Parallel()
	c := PlanComment(PlanNote{Revision: 2, Summary: "Add the write-back.\nSecond line", Risk: RiskL2, Steps: 3,
		NeedsApproval: true, Link: "https://kd.example.com/tasks/t1"})
	for _, want := range []string{"revision 2", "Add the write-back.", "L2", "3 steps", "approval",
		"https://kd.example.com/tasks/t1"} {
		if !strings.Contains(c, want) {
			t.Fatalf("plan comment misses %q:\n%s", want, c)
		}
	}
	if c := PlanComment(PlanNote{Revision: 1, Summary: "x", Risk: RiskL0, Steps: 1}); strings.Contains(c, "approval") {
		t.Fatalf("a plan without approval mentions it:\n%s", c)
	}
	c = FinalComment(FinalNote{State: TaskCompleted, Link: "https://kd.example.com/tasks/t1",
		PullRequests: []string{"https://github.com/o/r/pull/7"},
		AgentTime:    AgentTime{Total: 90*time.Minute + 5*time.Second, Status: WorkComplete},
		Docs:         &DocsDecision{Status: DocsNotApplicable, Reason: "no behaviour change"}})
	for _, want := range []string{"COMPLETED", "https://github.com/o/r/pull/7", "1h 30m", "DOCS_NOT_APPLICABLE",
		"no behaviour change", "https://kd.example.com/tasks/t1"} {
		if !strings.Contains(c, want) {
			t.Fatalf("final comment misses %q:\n%s", want, c)
		}
	}
	c = FinalComment(FinalNote{State: TaskFailed, AgentTime: AgentTime{Status: WorkIncomplete, Total: time.Minute},
		Docs: &DocsDecision{Status: DocsUpdated, Articles: []ArticleChange{{Article: "KIN-A-19", Section: "S"}}}})
	for _, want := range []string{"FAILED", "incomplete", "KIN-A-19"} {
		if !strings.Contains(c, want) {
			t.Fatalf("failed comment misses %q:\n%s", want, c)
		}
	}
}

// Review finding: changes are deduplicated per (article, section), the last one winning, and capped in total; an
// untrusted summary cannot mention YouTrack users.
func TestDecideDocsDedupesAndCaps(t *testing.T) {
	t.Parallel()
	var h Handoff
	for i := range 3 * maxArticleChanges {
		h.Articles = append(h.Articles, ArticleChange{Article: "KIN-A-19", Section: string(rune('a' + i%20)), Content: "v" + string(rune('0'+i/20))})
	}
	h.Articles = append(h.Articles, ArticleChange{Article: "KIN-A-19", Section: "a", Content: "last"})
	d := DecideDocs("t", "e", h, []string{"KIN-A-19"})
	if len(d.Articles) != maxArticleChanges || d.Articles[0].Content != "last" {
		t.Fatalf("articles %d, first %+v", len(d.Articles), d.Articles[0])
	}
	if c := PlanComment(PlanNote{Revision: 1, Summary: "ping @admin", Risk: RiskL0, Steps: 1}); strings.Contains(c, "@admin") {
		t.Fatalf("mention kept: %q", c)
	}
}
