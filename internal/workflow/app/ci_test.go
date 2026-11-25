package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// fakeCI is GitHub's checks and merge API: a CI status per head and the merges asked for.
type fakeCI struct {
	mu       sync.Mutex
	status   map[string]CIStatus
	statErr  error
	mergeErr error
	merges   []MergeRequest
}

func (f *fakeCI) set(head string, s CIStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = map[string]CIStatus{}
	}
	f.status[head] = s
}

func (f *fakeCI) CIStatus(_ context.Context, _, _, head string) (CIStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statErr != nil {
		return CIStatus{}, f.statErr
	}
	if s, ok := f.status[head]; ok {
		return s, nil
	}
	return CIStatus{State: domain.CIPending}, nil
}

func (f *fakeCI) MergePullRequest(_ context.Context, r MergeRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, r)
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	return "merge-" + r.HeadSHA, nil
}

func (f *fakeCI) merged() []MergeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.merges)
}

type fakeGate struct {
	mu       sync.Mutex
	approved bool
}

func (g *fakeGate) IntegrateDevApproved(context.Context, string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.approved, nil
}

func (g *fakeGate) set(v bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.approved = v
}

type ciEnv struct {
	publishEnv
	ci   *fakeCI
	gate *fakeGate
	done map[string]bool // outbox keys delivered
}

func newCIEnv() ciEnv {
	p := newPublishEnv()
	c := ciEnv{publishEnv: p, ci: &fakeCI{}, gate: &fakeGate{approved: true}, done: map[string]bool{}}
	p.svc.CI, p.svc.Integrate = c.ci, c.gate
	return c
}

// drain delivers every new message of the workflow's GitHub topics once, until none is left; it returns the
// last handler error.
func (e ciEnv) drain(t *testing.T) error {
	t.Helper()
	var last error
	for {
		e.out.mu.Lock()
		msgs := slices.Clone(e.out.msgs)
		e.out.mu.Unlock()
		progressed := false
		for _, m := range msgs {
			if e.done[m.Key] {
				continue
			}
			var h func(context.Context, messaging.Delivery) error
			switch m.Topic {
			case TopicOpenPullRequest:
				h = e.svc.HandleOpenPullRequest
			case TopicCICheck:
				h = e.svc.HandleCICheck
			case TopicMerge:
				h = e.svc.HandleMerge
			default:
				continue
			}
			e.done[m.Key], progressed = true, true
			if err := h(t.Context(), messaging.Delivery{Topic: m.Topic, Key: m.Key, Payload: m.Payload}); err != nil {
				last = err
			}
		}
		if !progressed {
			return last
		}
	}
}

func (e ciEnv) topics() []string {
	e.out.mu.Lock()
	defer e.out.mu.Unlock()
	var out []string
	for _, m := range e.out.msgs {
		out = append(out, m.Topic)
	}
	return out
}

// opened takes a one-step L1 task to VERIFYING with its pull request open (head h1).
func (e ciEnv) opened(t *testing.T) (domain.Task, domain.GitArtifact) {
	t.Helper()
	task, a := e.reviewed(t, onlyStep())
	if err := e.push(t, a, domain.Push{Repository: "api", Branch: "feature/" + task.ID + "--" + a.StepID, HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	return task, e.artifact(t, task.ID)
}

func (e ciEnv) artifact(t *testing.T, taskID string) domain.GitArtifact {
	t.Helper()
	arts, err := e.svc.TaskChangeSet(t.Context(), taskID)
	if err != nil || len(arts) != 1 {
		t.Fatalf("change set %+v %v", arts, err)
	}
	return arts[0]
}

// ciEvent is a completed check suite on the artifact's branch, as the GitHub webhook handler reports it.
func (e ciEnv) ciEvent(t *testing.T, a domain.GitArtifact, delivery string) {
	t.Helper()
	owned, err := e.svc.CIEvent(t.Context(), a.Repository, a.Branch, a.HeadSHA, delivery)
	if err != nil || !owned {
		t.Fatalf("CI event: owned %v %v", owned, err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
}

// Acceptance (KIN-37, KIN-60 follow-up): CI of a pull request the platform opened goes to its execution (a CI check
// is queued), not to the Alert Center; CI of any other branch is not the platform's.
func TestCIOfAPlatformPullRequestGoesToItsExecution(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	_, a := e.opened(t)
	owned, err := e.svc.CIEvent(t.Context(), a.Repository, a.Branch, a.HeadSHA, "d1")
	if err != nil || !owned || !slices.Contains(e.topics(), TopicCICheck) {
		t.Fatalf("owned %v %v topics %v", owned, err, e.topics())
	}
	if owned, err := e.svc.CIEvent(t.Context(), a.Repository, "feature/someone-else", "x", "d2"); err != nil || owned {
		t.Fatalf("a foreign branch: %v %v", owned, err)
	}
}

// Acceptance (KIN-37): green CI on an L0/L1 task merges its pull request into dev with the expected head (merge
// commit) and the task is COMPLETED.
func TestGreenCIMergesTheL1TaskIntoDevAndCompletesIt(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	task, a := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, a, "d1")
	m := e.ci.merged()
	if len(m) != 1 || m[0].HeadSHA != "h1" || m[0].Number != a.PRNumber || m[0].Base != "dev" || m[0].Repository != "api" {
		t.Fatalf("merges %+v", m)
	}
	got := e.artifact(t, task.ID)
	tk, _ := e.state(t, task.ID)
	if got.Merge != domain.MergeMerged || got.MergeSHA != "merge-h1" || got.CI != domain.CIPassed || tk.State != domain.TaskCompleted {
		t.Fatalf("artifact %+v task %s", got, tk.State)
	}
	if k := e.audit.kinds(); !slices.Contains(k, KindMerged) {
		t.Fatalf("audit %v", k)
	}
}

// Acceptance (KIN-37): without an approved integrate_dev (L2/L3) green CI never merges; the merge waits, visibly,
// and goes ahead once the Owner approved (picked up by the reconciliation).
func TestWithoutIntegrateDevApprovalGreenCINeverMerges(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	e.gate.set(false)
	task, a := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, a, "d1")
	if err := e.svc.ReconcileCI(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if m := e.ci.merged(); len(m) != 0 {
		t.Fatalf("merged without approval: %+v", m)
	}
	got := e.artifact(t, task.ID)
	tk, _ := e.state(t, task.ID)
	if got.Merge != domain.MergeWaiting || !strings.Contains(got.MergeError, "integrate_dev") || tk.State != domain.TaskVerifying {
		t.Fatalf("artifact %+v task %s", got, tk.State)
	}
	held := 0
	for _, k := range e.audit.kinds() {
		if k == KindMergeHeld {
			held++
		}
	}
	if k := e.audit.kinds(); held != 1 {
		t.Fatalf("audit (held once) %v", k)
	}
	e.gate.set(true)
	e.clock.Add(2 * ReconcileCIEvery) // the next reconciliation pass
	if err := e.svc.ReconcileCI(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted || len(e.ci.merged()) != 1 {
		t.Fatalf("after approval: task %s merges %+v", tk.State, e.ci.merged())
	}
}

// Acceptance (KIN-37): red CI starts a fix round in the same execution: the step implements again on the runner
// that holds its branch, told which checks failed; the new reviewed head is pushed and its CI followed.
func TestRedCIStartsAFixRoundInTheSameExecution(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	task, a := e.opened(t)
	_, before := e.state(t, task.ID)
	e.ci.set("h1", CIStatus{State: domain.CIFailed, Detail: "test (failure): TestLogin"})
	e.ciEvent(t, a, "d1")
	tk, ex := e.state(t, task.ID)
	if tk.State != domain.TaskRunning || ex.ID != before.ID || ex.State != domain.ExecutionRunning || ex.Steps[0].CIRound != 1 {
		t.Fatalf("task %s execution %s (%s, was %s) ci round %d", tk.State, ex.State, ex.ID, before.ID, ex.Steps[0].CIRound)
	}
	if got := e.artifact(t, task.ID); got.CI != domain.CIFailed || got.CIDetail != "test (failure): TestLogin" {
		t.Fatalf("artifact %+v", got)
	}
	e.schedule(t)
	as := e.disp.takeAssigned()
	if len(as) != 1 || as[0].Role != domain.StepRoleImplementer || !strings.Contains(as[0].Prompt, "TestLogin") {
		t.Fatalf("fix assignment %+v", as)
	}
	if k := e.audit.kinds(); !slices.Contains(k, KindCIFailed) || !slices.Contains(k, KindCIFixStarted) {
		t.Fatalf("audit %v", k)
	}
	fixed := domain.WorkChanges{BaseSHA: "b0", HeadSHA: "h2", Files: []string{"src/a.go"}, Total: 1}
	runAssigned(t, e.schedEnv, task.ID, "only", "r1", `{"summary":"fixed"}`, nil, fixed)
	e.schedule(t)
	e.disp.takeAssigned()
	runAssigned(t, e.schedEnv, task.ID, "only", "r1", approve, nil, fixed)
	e.schedule(t)
	pub := e.disp.takeAssigned()
	if len(pub) != 1 || pub[0].DiffHead != "h2" {
		t.Fatalf("publish %+v", pub)
	}
	if err := e.push(t, pub[0], domain.Push{Repository: "api", Branch: a.Branch, HeadSHA: "h2"}); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	got := e.artifact(t, task.ID)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskVerifying || got.HeadSHA != "h2" || got.CI != "" ||
		got.PRNumber != a.PRNumber || got.State != domain.ArtifactOpen {
		t.Fatalf("after the fix: task %s artifact %+v", tk.State, got)
	}
	e.ci.set("h2", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, got, "d2")
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted {
		t.Fatalf("task %s", tk.State)
	}
}

// Acceptance (KIN-37): once the CI fix rounds are used up the task is BLOCKED (reason ci, an alert); CI passing on
// the head later (a re-run) resumes it and the merge goes ahead.
func TestExhaustedCIFixBudgetBlocksTheTask(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	task, a := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIFailed, Detail: "lint (failure)"})
	for i := range domain.MaxCIFixRounds {
		e.ciEvent(t, a, "red"+string(rune('a'+i)))
		for _, answer := range []string{`{"summary":"again"}`, approve} {
			e.schedule(t)
			e.disp.takeAssigned()
			runAssigned(t, e.schedEnv, task.ID, "only", "r1", answer, nil, work)
		}
		e.schedule(t)
		pub := e.disp.takeAssigned()
		if len(pub) != 1 {
			t.Fatalf("round %d publish %+v", i+1, pub)
		}
		if err := e.push(t, pub[0], domain.Push{Repository: "api", Branch: a.Branch, HeadSHA: "h1"}); err != nil {
			t.Fatal(err)
		}
		if err := e.drain(t); err != nil {
			t.Fatal(err)
		}
	}
	e.ciEvent(t, a, "red-last")
	tk, _ := e.state(t, task.ID)
	if tk.State != domain.TaskBlocked || tk.BlockReason != domain.WaitCI || tk.BlockedFrom != domain.TaskVerifying {
		t.Fatalf("task %s %s from %s", tk.State, tk.BlockReason, tk.BlockedFrom)
	}
	if !slices.Contains(e.topics(), TopicTaskBlocked) || !slices.Contains(e.audit.kinds(), KindCIBudgetExhausted) {
		t.Fatalf("topics %v audit %v", e.topics(), e.audit.kinds())
	}
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, a, "rerun")
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted {
		t.Fatalf("after a green re-run: %s", tk.State)
	}
}

// Acceptance (KIN-37): when the pull request's head moved (or GitHub refuses the merge) nothing is merged blindly:
// the merge is recorded as failed, audited, and the task waits for the Owner (BLOCKED, reason ci).
func TestAMovedHeadIsNeverMergedBlindly(t *testing.T) {
	t.Parallel()
	for name, merr := range map[string]error{
		"head moved":     ErrHeadMoved,
		"protected base": messaging.Permanent(domain.ErrProtectedBranch),
		"not mergeable":  messaging.Permanent(errors.New("GitHub refused the merge (HTTP 405)")),
	} {
		e := newCIEnv()
		e.ci.mergeErr = merr
		task, a := e.opened(t)
		e.ci.set("h1", CIStatus{State: domain.CIPassed})
		e.ciEvent(t, a, "d1")
		got := e.artifact(t, task.ID)
		tk, _ := e.state(t, task.ID)
		if got.Merge != domain.MergeFailed || got.MergeError == "" || tk.State != domain.TaskBlocked || tk.BlockReason != domain.WaitCI {
			t.Errorf("%s: artifact %+v task %s", name, got, tk.State)
		}
		want := KindMergeFailed
		if errors.Is(merr, domain.ErrProtectedBranch) {
			want = KindMergeRefused
		}
		if !slices.Contains(e.audit.kinds(), want) {
			t.Errorf("%s: audit %v", name, e.audit.kinds())
		}
	}
}

// A transient GitHub failure of the merge is retried by the outbox; the task stays VERIFYING.
func TestATransientMergeFailureIsRetried(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	e.ci.mergeErr = errors.New("github: 502")
	task, a := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	if _, err := e.svc.CIEvent(t.Context(), a.Repository, a.Branch, a.HeadSHA, "d1"); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err == nil || messaging.IsPermanent(err) {
		t.Fatalf("merge delivery: %v", err)
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskVerifying || e.artifact(t, task.ID).Merge == domain.MergeMerged {
		t.Fatalf("task %s", tk.State)
	}
}

// Acceptance (KIN-37, multi-repo): the change set merges in order, one repository after the other (a failed merge
// stopping the rest: domain TestNextMergeFollowsTheChangeSetOrderAndStopsOnFailure).
func TestMultiRepoChangeSetMergesInOrder(t *testing.T) {
	t.Parallel()
	e := newCIEnv()
	plan := domain.Plan{Risk: domain.RiskL1, MaxParallel: 2, Summary: "s", Steps: []domain.PlanStep{
		{Key: "only", Title: "API", Repository: "api", Scope: []string{"src/"}},
		{Key: "web", Title: "Web", Repository: "web", Scope: []string{"src/"}}}}
	task := e.task(t, plan)
	e.pool.set("p1", cliRunner("r1", "claude", "codex"))
	for range 3 {
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
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	arts, _ := e.svc.TaskChangeSet(t.Context(), task.ID)
	if len(arts) != 2 {
		t.Fatalf("change set %+v", arts)
	}
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, arts[1], "d-web") // the later one first: it must wait for the first
	if m := e.ci.merged(); len(m) != 0 {
		t.Fatalf("merged out of order: %+v", m)
	}
	e.ciEvent(t, arts[0], "d-api")
	m := e.ci.merged()
	if len(m) != 2 || m[0].Repository != arts[0].Repository || m[1].Repository != arts[1].Repository {
		t.Fatalf("merge order %+v", m)
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted {
		t.Fatalf("task %s", tk.State)
	}
}
