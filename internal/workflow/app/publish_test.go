package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type memChangeSets struct {
	mu   sync.Mutex
	arts []domain.GitArtifact
}

func (m *memChangeSets) SaveArtifact(_ context.Context, a domain.GitArtifact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, x := range m.arts {
		if x.ExecutionID == a.ExecutionID && x.Repository == a.Repository && x.Branch == a.Branch {
			m.arts[i] = a
			return nil
		}
	}
	m.arts = append(m.arts, a)
	return nil
}

func (m *memChangeSets) Artifact(_ context.Context, ex, repo, branch string) (domain.GitArtifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.arts {
		if x.ExecutionID == ex && x.Repository == repo && x.Branch == branch {
			return x, nil
		}
	}
	return domain.GitArtifact{}, domain.ErrNotFound
}

func (m *memChangeSets) TaskArtifacts(_ context.Context, taskID string) ([]domain.GitArtifact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.GitArtifact
	for _, x := range m.arts {
		if x.TaskID == taskID {
			out = append(out, x)
		}
	}
	return out, nil
}

// fakeGitHub keeps the open pull requests per repository and branch, like GitHub: opening one twice reuses it.
type fakeGitHub struct {
	mu    sync.Mutex
	base  string
	err   error
	open  map[string]PullRequest
	calls []PullRequestRequest
}

func (f *fakeGitHub) OpenPullRequest(_ context.Context, r PullRequestRequest) (PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	if f.err != nil {
		return PullRequest{}, f.err
	}
	if f.open == nil {
		f.open = map[string]PullRequest{}
	}
	k := r.Repository + "/" + r.Branch
	if pr, ok := f.open[k]; ok {
		pr.Reused = true
		return pr, nil
	}
	pr := PullRequest{Number: 40 + len(f.open), URL: "https://github.com/acme/" + r.Repository + "/pull/" + r.Branch, Base: f.base}
	f.open[k] = pr
	return pr, nil
}

type memAudit struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (a *memAudit) Record(_ context.Context, e auditapp.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *memAudit) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Kind)
	}
	return out
}

type publishEnv struct {
	schedEnv
	out   *alertOutbox
	sets  *memChangeSets
	gh    *fakeGitHub
	audit *memAudit
}

func newPublishEnv() publishEnv {
	e, _ := newRoutedEnv()
	p := publishEnv{schedEnv: e, out: &alertOutbox{}, sets: &memChangeSets{}, gh: &fakeGitHub{base: "dev"}, audit: &memAudit{}}
	e.svc.Outbox, e.svc.ChangeSets, e.svc.PullRequests, e.svc.Audit = p.out, p.sets, p.gh, p.audit
	return p
}

const approve = `{"verdict":"approve","summary":"good","findings":[]}`

// reviewed runs the task's only step through implementation (b0..h1) and an approving review on r1 and returns the
// publish assignment.
func (e publishEnv) reviewed(t *testing.T, plan domain.Plan) (domain.Task, Assignment) {
	t.Helper()
	task := e.task(t, plan)
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	for _, answer := range []string{`{"summary":"did it"}`, approve} {
		e.schedule(t)
		e.disp.takeAssigned()
		runAssigned(t, e.schedEnv, task.ID, "only", "r1", answer, nil, work)
	}
	e.pool.set("p1", cliRunner("r2", "claude", "codex"), cliRunner("r1", "claude", "codex"))
	e.schedule(t)
	got := e.disp.takeAssigned()
	if len(got) != 1 {
		t.Fatalf("publish assignments %+v", got)
	}
	return task, got[0]
}

// push reports the publish attempt's push and completes it.
func (e publishEnv) push(t *testing.T, a Assignment, p domain.Push) error {
	t.Helper()
	ctx := t.Context()
	if err := e.svc.StartStep(ctx, a.StepID, "r1", a.Fence); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RecordPush(ctx, a.StepID, "r1", a.Fence, p); err != nil {
		return err
	}
	return e.svc.CompleteStep(ctx, a.StepID, "r1", a.Fence)
}

func (e publishEnv) deliver(t *testing.T) error {
	t.Helper()
	e.out.mu.Lock()
	msgs := slices.Clone(e.out.msgs)
	e.out.mu.Unlock()
	var last error
	for _, m := range msgs {
		if m.Topic == TopicOpenPullRequest {
			last = e.svc.HandleOpenPullRequest(t.Context(), messaging.Delivery{Topic: m.Topic, Key: m.Key, Payload: m.Payload})
		}
	}
	return last
}

// Acceptance (KIN-36): the approved work is pushed by the runner holding the branch (the reviewed head, no CLI);
// the push is recorded in the execution's change set with its head SHA and the pull request into the integration
// branch is opened through the outbox, idempotently (a redelivery reuses the open pull request).
func TestApprovedWorkIsPushedAndItsPullRequestOpenedThroughTheOutbox(t *testing.T) {
	t.Parallel()
	e := newPublishEnv()
	task, a := e.reviewed(t, onlyStep())
	if a.Agent != domain.AgentPublish || a.Role != domain.StepRolePublisher || a.DiffHead != "h1" || a.Prompt != "" {
		t.Fatalf("publish assignment %+v", a)
	}
	branch := "feature/" + task.ID + "--" + a.StepID
	if err := e.push(t, a, domain.Push{Repository: "api", Branch: branch, HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	tk, ex := e.state(t, task.ID)
	if ex.State != domain.ExecutionSucceeded || tk.State != domain.TaskVerifying {
		t.Fatalf("after publish: execution %s task %s", ex.State, tk.State)
	}
	arts, _ := e.svc.TaskChangeSet(t.Context(), task.ID)
	if len(arts) != 1 || arts[0].State != domain.ArtifactOpening || arts[0].HeadSHA != "h1" || arts[0].Branch != branch {
		t.Fatalf("change set %+v", arts)
	}
	for range 2 { // at-least-once delivery
		if err := e.deliver(t); err != nil {
			t.Fatal(err)
		}
	}
	arts, _ = e.svc.TaskChangeSet(t.Context(), task.ID)
	if a := arts[0]; a.State != domain.ArtifactOpen || a.PRNumber != 40 || a.PRBase != "dev" || a.PRURL == "" {
		t.Fatalf("artifact %+v", a)
	}
	if len(e.gh.open) != 1 || e.gh.calls[0].Branch != branch || e.gh.calls[0].ProjectID != "p1" {
		t.Fatalf("pull requests %+v calls %+v", e.gh.open, e.gh.calls)
	}
	if k := e.audit.kinds(); !slices.Equal(k, []string{KindPullRequestOpened, KindPullRequestReused}) {
		t.Fatalf("audit %v", k)
	}
}

// Acceptance (KIN-36): a push to main/master is refused in the application path and audited; the attempt fails and
// nothing is published.
func TestAPushReportToAProtectedBranchIsRefusedAndAudited(t *testing.T) {
	t.Parallel()
	e := newPublishEnv()
	task, a := e.reviewed(t, onlyStep())
	if err := e.push(t, a, domain.Push{Repository: "api", Branch: "main", HeadSHA: "h1"}); err == nil {
		t.Fatal("completed after a refused push")
	}
	_, ex := e.state(t, task.ID)
	if st := ex.Steps[0]; st.State == domain.StepSucceeded || st.Attempts[len(st.Attempts)-1].State != domain.AttemptFailed {
		t.Fatalf("step %s", st.State)
	}
	if arts, _ := e.svc.TaskChangeSet(t.Context(), task.ID); len(arts) != 0 {
		t.Fatalf("published %+v", arts)
	}
	if k := e.audit.kinds(); !slices.Equal(k, []string{KindPushRefused}) {
		t.Fatalf("audit %v", k)
	}
}

// Acceptance (KIN-36): a pull request whose base would be main/master is refused and audited; the artifact shows
// why and the message is not retried.
func TestAPullRequestIntoAProtectedBaseIsRefusedAndAudited(t *testing.T) {
	t.Parallel()
	for _, gh := range []*fakeGitHub{{base: "main"}, {err: messaging.Permanent(domain.ErrProtectedBranch)}} {
		e := newPublishEnv()
		e.svc.PullRequests = gh
		task, a := e.reviewed(t, onlyStep())
		if err := e.push(t, a, domain.Push{Repository: "api", Branch: "feature/" + task.ID + "--" + a.StepID, HeadSHA: "h1"}); err != nil {
			t.Fatal(err)
		}
		if err := e.deliver(t); !messaging.IsPermanent(err) || !errors.Is(err, domain.ErrProtectedBranch) {
			t.Fatalf("delivery: %v", err)
		}
		arts, _ := e.svc.TaskChangeSet(t.Context(), task.ID)
		if arts[0].State != domain.ArtifactFailed || arts[0].PRNumber != 0 || arts[0].Error == "" {
			t.Fatalf("artifact %+v", arts[0])
		}
		if k := e.audit.kinds(); !slices.Equal(k, []string{KindPullRequestRefused}) {
			t.Fatalf("audit %v", k)
		}
	}
}

// A transient GitHub failure is retried by the outbox: the artifact stays opening.
func TestATransientPullRequestFailureIsRetried(t *testing.T) {
	t.Parallel()
	e := newPublishEnv()
	e.gh.err = errors.New("github: 502")
	task, a := e.reviewed(t, onlyStep())
	if err := e.push(t, a, domain.Push{Repository: "api", Branch: "feature/" + task.ID + "--" + a.StepID, HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.deliver(t); err == nil || messaging.IsPermanent(err) {
		t.Fatalf("delivery: %v", err)
	}
	if arts, _ := e.svc.TaskChangeSet(t.Context(), task.ID); arts[0].State != domain.ArtifactOpening {
		t.Fatalf("artifact %+v", arts[0])
	}
}

// Acceptance (KIN-36, multi-repo): each repository's published branch gets its own pull request in the one change
// set of the execution.
func TestEveryRepositoryOfTheExecutionGetsItsPullRequest(t *testing.T) {
	t.Parallel()
	e := newPublishEnv()
	plan := domain.Plan{Risk: domain.RiskL1, MaxParallel: 2, Summary: "s", Steps: []domain.PlanStep{
		{Key: "only", Title: "API", Repository: "api", Scope: []string{"src/"}},
		{Key: "web", Title: "Web", Repository: "web", Scope: []string{"src/"}}}}
	task := e.task(t, plan)
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	for range 3 { // implement, review, publish — both steps in parallel
		e.schedule(t)
		for _, a := range e.disp.takeAssigned() {
			switch a.Role {
			case domain.StepRolePublisher:
				if err := e.push(t, a, domain.Push{Repository: a.Repository, Branch: "feature/" + task.ID + "--" + a.StepID,
					HeadSHA: a.DiffHead}); err != nil {
					t.Fatal(err)
				}
			case domain.StepRoleReviewer:
				runAssigned(t, e.schedEnv, task.ID, a.StepKey, "r1", approve, nil, work)
			default:
				runAssigned(t, e.schedEnv, task.ID, a.StepKey, "r1", `{"summary":"x"}`, nil, work)
			}
		}
	}
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	arts, _ := e.svc.TaskChangeSet(t.Context(), task.ID)
	if len(arts) != 2 || arts[0].ExecutionID != arts[1].ExecutionID || arts[0].Repository == arts[1].Repository ||
		arts[0].State != domain.ArtifactOpen || arts[1].State != domain.ArtifactOpen || arts[0].PRNumber == arts[1].PRNumber {
		t.Fatalf("change set %+v", arts)
	}
}
