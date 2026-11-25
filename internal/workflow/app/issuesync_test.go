package app

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// fakeIssueSync records the write-back requests (they would be outbox messages in the caller's transaction).
type fakeIssueSync struct {
	mu       sync.Mutex
	comments map[string]string // key -> text
	states   []string          // key=state
	articles []string          // key article section
	pub      Publication
}

func (f *fakeIssueSync) Comment(_ context.Context, key, _, _, issueID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[string]string{}
	}
	f.comments[key] = issueID + "\n" + text
	return nil
}

func (f *fakeIssueSync) State(_ context.Context, key, _, _, _, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, key+"="+state)
	return nil
}

func (f *fakeIssueSync) Article(_ context.Context, key, _, _, _, article, section, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.articles = append(f.articles, key+" "+article+" "+section)
	return nil
}

func (f *fakeIssueSync) Publication(context.Context, string) (Publication, error) { return f.pub, nil }

func (f *fakeIssueSync) comment(prefix, suffix string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.comments {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, suffix) {
			return v, true
		}
	}
	return "", false
}

func (f *fakeIssueSync) stateValues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.states {
		out = append(out, s[strings.LastIndex(s, "=")+1:])
	}
	return out
}

type memDocs struct {
	mu   sync.Mutex
	docs []domain.DocsDecision
}

func (m *memDocs) SaveDocs(_ context.Context, d domain.DocsDecision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs = append(m.docs, d)
	return nil
}

func (m *memDocs) LatestDocs(_ context.Context, taskID string) (domain.DocsDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.docs) - 1; i >= 0; i-- {
		if m.docs[i].TaskID == taskID {
			return m.docs[i], nil
		}
	}
	return domain.DocsDecision{}, domain.ErrNotFound
}

func newSyncEnv() (ciEnv, *fakeIssueSync, *memDocs) {
	e := newCIEnv()
	s, d := &fakeIssueSync{}, &memDocs{}
	e.svc.IssueSync, e.svc.Docs, e.svc.TaskLinkBase = s, d, "https://kd.example.com"
	e.svc.Access = fakeAccess{"u1": {"p1": RoleOwner}}
	return e, s, d
}

// completeIssueTask takes a one-step task of issue KIN-7 through implementation (answer), review, publication and
// green CI to COMPLETED.
func (e ciEnv) completeIssueTask(t *testing.T, p domain.Plan, answer string) domain.Task {
	t.Helper()
	ctx := t.Context()
	task, err := e.svc.CreateTask(ctx, "p1", "Login", "KIN-7", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SubmitPlan(ctx, task.ID, p, "planner"); err != nil {
		t.Fatal(err)
	}
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	for _, a := range []string{answer, approve} {
		e.schedule(t)
		e.disp.takeAssigned()
		runAssigned(t, e.schedEnv, task.ID, "only", "r1", a, nil, work)
	}
	e.pool.set("p1", cliRunner("r2", "claude", "codex"), cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	got := e.disp.takeAssigned()
	if len(got) != 1 {
		t.Fatalf("publish assignments %+v", got)
	}
	if err := e.push(t, got[0], domain.Push{Repository: "api", Branch: "feature/" + task.ID + "--" + got[0].StepID,
		HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, e.artifact(t, task.ID), "d1")
	tk, _ := e.state(t, task.ID)
	if tk.State != domain.TaskCompleted {
		t.Fatalf("task %s", tk.State)
	}
	return tk
}

func docsPlan() domain.Plan {
	p := onlyStep()
	p.Docs = []string{"KIN-A-19 (YouTrack section)"}
	return p
}

// Acceptance (KIN-38 AC1, AC3): an issue's task tells YouTrack — through the write-back, keyed per change — the
// accepted plan with its link, every state change, and at the end the pull request, the agent time and the
// documentation; the implementer's article changes are written only within the approved plan's docs scope.
func TestIssueTaskWritesBackPlanStatesAndOutcome(t *testing.T) {
	t.Parallel()
	e, s, docs := newSyncEnv()
	answer := `{"summary":"did it","articles":[{"article":"KIN-A-19","section":"KIN-38","content":"Write-back."},` +
		`{"article":"KIN-A-12","section":"Rules","content":"x"}]}`
	task := e.completeIssueTask(t, docsPlan(), answer)

	plan, ok := s.comment("task/"+task.ID+"/plan/", "1")
	if !ok || !strings.HasPrefix(plan, "KIN-7\n") || !strings.Contains(plan, "plan revision 1") ||
		!strings.Contains(plan, "https://kd.example.com/tasks/"+task.ID) {
		t.Fatalf("plan comment %q (all %v)", plan, s.comments)
	}
	states := s.stateValues()
	for _, want := range []string{"ANALYZING", "READY", "RUNNING", "VERIFYING", "COMPLETED"} {
		if !slices.Contains(states, want) {
			t.Fatalf("states %v miss %s", states, want)
		}
	}
	final, ok := s.comment("task/"+task.ID+"/", "/final")
	pr := e.artifact(t, task.ID).PRURL
	if !ok || pr == "" || !strings.Contains(final, pr) || !strings.Contains(final, "Agent time") ||
		!strings.Contains(final, "KIN-A-19") || !strings.Contains(final, "KIN-A-12") {
		t.Fatalf("final comment %q", final)
	}
	if len(s.articles) != 1 || !strings.HasSuffix(s.articles[0], " KIN-A-19 KIN-38") ||
		!strings.HasPrefix(s.articles[0], "task/"+task.ID+"/") {
		t.Fatalf("articles %v", s.articles)
	}
	if len(docs.docs) != 1 || docs.docs[0].Status != domain.DocsUpdated || !slices.Equal(docs.docs[0].Rejected, []string{"KIN-A-12"}) {
		t.Fatalf("docs %+v", docs.docs)
	}
}

// Acceptance (KIN-38 AC4): DOCS_NOT_APPLICABLE is stored with its reason and reported.
func TestDocsNotApplicableIsStoredWithItsReason(t *testing.T) {
	t.Parallel()
	e, s, docs := newSyncEnv()
	task := e.completeIssueTask(t, onlyStep(), `{"summary":"did it","docsNotApplicable":"internal refactoring"}`)
	if len(docs.docs) != 1 || docs.docs[0].Status != domain.DocsNotApplicable || docs.docs[0].Reason != "internal refactoring" {
		t.Fatalf("docs %+v", docs.docs)
	}
	final, _ := s.comment("task/"+task.ID+"/", "/final")
	if !strings.Contains(final, "DOCS_NOT_APPLICABLE — internal refactoring") || len(s.articles) != 0 {
		t.Fatalf("final %q articles %v", final, s.articles)
	}
	d, err := e.svc.TaskDetail(t.Context(), auth.Principal{UserID: "u1"}, task.ID)
	if err != nil || d.Docs == nil || d.Docs.Reason != "internal refactoring" {
		t.Fatalf("detail docs %+v %v", d.Docs, err)
	}
}

// A plan that needs approval says where to approve it; a task without an issue writes nothing back.
func TestPlanWaitingForApprovalLinksTheApproval(t *testing.T) {
	t.Parallel()
	e, s, _ := newSyncEnv()
	ctx := t.Context()
	task, err := e.svc.CreateTask(ctx, "p1", "Login", "KIN-8", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	p := onlyStep()
	p.Risk = domain.RiskL2
	if _, err := e.svc.SubmitPlan(ctx, task.ID, p, "planner"); err != nil {
		t.Fatal(err)
	}
	c, ok := s.comment("task/"+task.ID+"/plan/", "1")
	if !ok || !strings.Contains(c, "waiting for approval") || !strings.Contains(c, "Review and approve: https://kd.example.com/tasks/"+task.ID) {
		t.Fatalf("plan comment %q", c)
	}

	before := len(s.stateValues())
	manual, err := e.svc.CreateTask(ctx, "p1", "Manual", "", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartAnalysis(ctx, manual.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.stateValues()) != before {
		t.Fatalf("a task without an issue wrote back: %v", s.states)
	}
}

// Acceptance (KIN-38 AC5): the task page shows the pending ("awaiting publication") and failed writes.
func TestTaskDetailShowsThePublication(t *testing.T) {
	t.Parallel()
	e, s, _ := newSyncEnv()
	s.pub = Publication{Pending: 2, LastError: "http_503"}
	task, err := e.svc.CreateTask(t.Context(), "p1", "Login", "KIN-9", "u1")
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.svc.TaskDetail(t.Context(), auth.Principal{UserID: "u1"}, task.ID)
	if err != nil || d.Publication == nil || *d.Publication != s.pub {
		t.Fatalf("publication %+v %v", d.Publication, err)
	}
}

// Review finding: the documentation decision of a task without a YouTrack issue is stored as well.
func TestDocsDecisionIsStoredWithoutAnIssue(t *testing.T) {
	t.Parallel()
	e, s, docs := newSyncEnv()
	task, a := e.reviewed(t, onlyStep())
	if err := e.push(t, a, domain.Push{Repository: "api", Branch: "feature/" + task.ID + "--" + a.StepID, HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, e.artifact(t, task.ID), "d1")
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted {
		t.Fatalf("task %s", tk.State)
	}
	if len(docs.docs) != 1 || docs.docs[0].Status != domain.DocsUnstated || len(s.states)+len(s.comments) != 0 {
		t.Fatalf("docs %+v, writes %v %v", docs.docs, s.states, s.comments)
	}
}
