package main

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	approvals "github.com/tr1gl1de/kindevai/internal/approvals/domain"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// KIN-42 acceptance: the whole path of a YouTrack issue to a merge into dev on a Control Plane wired like `serve`
// (workflow, approvals, runner channel, outbox/inbox dispatchers, GitHub App and YouTrack adapters), with GitHub and
// YouTrack replaced by httptest servers and the runner with its fake CLI played through the runner channel's ports
// (assignments are read from the runner's command log, reports go in as the runner sends them).

const (
	acceptHead      = "bbbb2222"
	acceptPR        = 12
	acceptMergeSHA  = "mmmm9999"
	acceptHookToken = "acceptance-webhook-secret-0123"
	acceptIssue     = "KIN-7"
	acceptArticle   = "KIN-A-19"
)

// acceptGitHub is the GitHub REST API of one repository, acme/app, as the GitHub App adapter uses it.
type acceptGitHub struct {
	mu          sync.Mutex
	conclusion  string // check run conclusion of every head ("" = still running)
	mergeStatus int    // answer to the merge (0 = 200, merged)
	pr          map[string]any
	merges      []string // "<sha> <merge_method>"
	requests    []string
	protected   []string // requests naming main or master
}

func (f *acceptGitHub) set(fn func(f *acceptGitHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *acceptGitHub) snapshot() (merges, requests, protected []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.merges), slices.Clone(f.requests), slices.Clone(f.protected)
}

func (f *acceptGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, route)
	if r.Method != http.MethodGet && touchesProtected(r.URL.Path, body) {
		f.protected = append(f.protected, route+" "+string(body))
	}
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case route == "GET /app":
		reply(200, map[string]any{"slug": "kindevai"})
	case route == "GET /repos/acme/app/installation":
		reply(200, map[string]any{"id": 77})
	case route == "POST /app/installations/77/access_tokens":
		reply(201, map[string]any{"token": "ghs_acceptance", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case route == "GET /repos/acme/app":
		reply(200, map[string]any{"default_branch": "dev"})
	case strings.HasPrefix(route, "GET /repos/acme/app/branches/"):
		reply(200, map[string]any{"protected": true})
	case strings.HasPrefix(route, "GET /repos/acme/app/rules/branches/"):
		reply(200, []any{map[string]any{"type": "pull_request"}})
	case route == "GET /repos/acme/app/pulls":
		if f.pr == nil || f.pr["state"] != "open" {
			reply(200, []any{})
			return
		}
		reply(200, []any{f.pr})
	case route == "POST /repos/acme/app/pulls":
		var in struct{ Head, Base string }
		_ = json.Unmarshal(body, &in)
		if f.pr == nil {
			f.pr = map[string]any{"number": acceptPR, "html_url": "https://github.com/acme/app/pull/12", "state": "open",
				"merged": false, "head": map[string]any{"ref": in.Head, "sha": acceptHead}, "base": map[string]any{"ref": in.Base}}
		}
		reply(201, f.pr)
	case route == "GET /repos/acme/app/commits/"+acceptHead+"/check-runs":
		run := map[string]any{"name": "ci", "status": "in_progress"}
		if f.conclusion != "" {
			run = map[string]any{"name": "ci", "status": "completed", "conclusion": f.conclusion}
		}
		reply(200, map[string]any{"total_count": 1, "check_runs": []any{run}})
	case route == "GET /repos/acme/app/pulls/"+strconv.Itoa(acceptPR) && f.pr != nil:
		reply(200, f.pr)
	case route == "PUT /repos/acme/app/pulls/"+strconv.Itoa(acceptPR)+"/merge" && f.pr != nil:
		var in struct {
			SHA    string `json:"sha"`
			Method string `json:"merge_method"`
		}
		_ = json.Unmarshal(body, &in)
		f.merges = append(f.merges, in.SHA+" "+in.Method)
		if f.mergeStatus != 0 {
			reply(f.mergeStatus, map[string]any{"message": "Pull Request is not mergeable"})
			return
		}
		f.pr["merged"], f.pr["state"], f.pr["merge_commit_sha"] = true, "closed", acceptMergeSHA
		reply(200, map[string]any{"sha": acceptMergeSHA, "merged": true})
	default:
		reply(404, map[string]any{"message": "Not Found"})
	}
}

// touchesProtected reports a write naming main or master in a path segment or in a base/branch/ref field of the body
// (pull request creation, ref and contents writes). A merge names no branch: the merge gate's base check is covered
// by the retargeted pull request case of TestAcceptanceDevMovedBeforeTheMergeBlocksTheTask.
func touchesProtected(path string, body []byte) bool {
	protected := func(v string) bool {
		v = strings.ToLower(strings.TrimPrefix(v, "refs/heads/"))
		return v == "main" || v == "master"
	}
	if slices.ContainsFunc(strings.Split(path, "/"), protected) {
		return true
	}
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	for _, k := range []string{"base", "branch", "ref"} {
		if v, ok := fields[k].(string); ok && protected(v) {
			return true
		}
	}
	return false
}

// acceptYouTrack is the YouTrack REST API of issue KIN-7 and article KIN-A-19; the first downFor writes fail with 503.
type acceptYouTrack struct {
	mu       sync.Mutex
	downFor  int
	failed   int
	comments []string
	commands []string
	article  string
	updated  int64
}

func (f *acceptYouTrack) snapshot() (comments []string, article string, failed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments), f.article, f.failed
}

func (f *acceptYouTrack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.Method == http.MethodPost && f.failed < f.downFor {
		f.failed++
		reply(503, map[string]any{"error": "maintenance"})
		return
	}
	switch route := r.Method + " " + r.URL.Path; route {
	case "GET /api/issues/" + acceptIssue:
		ids := make([]any, len(f.comments))
		for i := range ids {
			ids[i] = map[string]any{"id": strconv.Itoa(i)}
		}
		reply(200, map[string]any{"comments": ids})
	case "GET /api/issues/" + acceptIssue + "/comments":
		out := make([]any, 0, len(f.comments))
		for _, c := range f.comments {
			out = append(out, map[string]any{"text": c})
		}
		reply(200, out)
	case "POST /api/issues/" + acceptIssue + "/comments":
		var in struct{ Text string }
		_ = json.Unmarshal(body, &in)
		f.comments = append(f.comments, in.Text)
		reply(200, map[string]any{"id": "c" + strconv.Itoa(len(f.comments))})
	case "POST /api/commands":
		f.commands = append(f.commands, string(body))
		reply(200, map[string]any{"id": "cmd"})
	case "GET /api/articles/" + acceptArticle:
		reply(200, map[string]any{"content": f.article, "updated": f.updated})
	case "POST /api/articles/" + acceptArticle:
		var in struct{ Content string }
		_ = json.Unmarshal(body, &in)
		f.article, f.updated = in.Content, f.updated+1
		reply(200, map[string]any{"id": acceptArticle})
	default:
		reply(404, map[string]any{"error": "Not Found"})
	}
}

// acceptanceEnv is the Control Plane with its outbox and inbox running against the fake GitHub and YouTrack.
type acceptanceEnv struct {
	cp       *controlPlane
	integ    *integrationsModule
	gh       *acceptGitHub
	yt       *acceptYouTrack
	runnerID string
	other    string // a second approved runner of the project
	pool     *onePool
}

func newAcceptanceEnv(t *testing.T, ytDownFor int) *acceptanceEnv {
	t.Helper()
	gh, yt := &acceptGitHub{}, &acceptYouTrack{downFor: ytDownFor, article: "# Auth\n\n## Login\n\nNot yet.\n", updated: 1}
	ghSrv, ytSrv := httptest.NewServer(gh), httptest.NewServer(yt)
	t.Cleanup(ghSrv.Close)
	t.Cleanup(ytSrv.Close)

	logger := slog.New(slog.DiscardHandler)
	inst := bootstrapWhenUnlocked(t, t.Context(), t.TempDir())
	t.Cleanup(func() { _ = inst.Close() })
	audit := newAudit(inst, logger)
	msg := newMessaging(inst, logger)
	integ := newIntegrations(inst, msg, audit, ghSrv.URL, logger)
	apv := newApprovals(inst, msg, audit, integ, logger)
	channel := newRunnerChannel(inst, apv.Workflow, runners.DefaultRetention(), logger)
	pool := &onePool{}
	apv.Workflow.Runners, apv.Workflow.Dispatcher, apv.Workflow.Agents = pool, runnerDispatcher{ch: channel}, reviewRouter{}
	attachScopeCheck(channel, apv.Approvals, apv.Workflow)
	wirePlanner(apv.Workflow, inst, msg, integ)
	wirePublishing(apv.Workflow, inst, msg, integ.GitHub)
	wireCI(apv.Workflow, apv.Approvals, msg, integ.GitHub)
	wireWriteBack(apv.Workflow, inst, msg, integ, "https://ai.example.com")
	cp := &controlPlane{inst: inst, wf: apv.Workflow, approvals: apv.Approvals, hub: runnersapp.NewHub()}
	runnerID, _ := cp.seed(t, "p1")
	other := cp.enrollRunner(t, "p1")
	pool.ids = []string{runnerID}
	ctx := t.Context()
	if _, err := cp.inst.DB.ExecContext(ctx, "UPDATE project_repositories SET test_command = 'make test' WHERE id = 'r1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := integ.Service.ConfigureYouTrack(ctx, ownerP, ytSrv.URL, "perm:acceptance-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := integ.GitHub.ConfigureGitHub(ctx, ownerP, integrationsapp.GitHubInput{AppID: "42", PrivateKey: rsaKeyPEM(t),
		WebhookSecret: acceptHookToken}); err != nil {
		t.Fatal(err)
	}
	stop := msg.start(ctx)
	t.Cleanup(stop) // runs before inst.Close (cleanups are last in, first out)
	return &acceptanceEnv{cp: cp, integ: integ, gh: gh, yt: yt, runnerID: runnerID, other: other, pool: pool}
}

// enrollRunner enrolls one more runner on a seeded Control Plane and approves it for projects.
func (cp *controlPlane) enrollRunner(t *testing.T, projects ...string) string {
	t.Helper()
	ctx := t.Context()
	svc := cp.runners()
	_, tok, err := svc.CreateToken(ctx, ownerP)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	res, err := svc.Enroll(ctx, runnersapp.EnrollRequest{Token: tok, PublicKey: base64.StdEncoding.EncodeToString(pub),
		Hostname: "box2", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(ctx, ownerP, res.Runner.ID, projects); err != nil {
		t.Fatal(err)
	}
	return res.Runner.ID
}

// assignsOf decodes every execution.assign in the runner's command log.
func assignsOf(t *testing.T, cp *controlPlane, runnerID string) []protocol.Assign {
	t.Helper()
	cmds, err := runnersdb.New(cp.inst.DB).CommandsAfter(t.Context(), runnerID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []protocol.Assign
	for _, c := range cmds {
		var a protocol.Assign
		if c.Kind == protocol.CommandAssign && json.Unmarshal(c.Body, &a) == nil {
			out = append(out, a)
		}
	}
	return out
}

func rsaKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// eventually polls cond until it holds (bounded by e2eWait).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(e2eWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// play runs one attempt of assignment a as the runner does: started, test evidence, the CLI's final answer, the
// worktree's changes, completed.
func (e *acceptanceEnv) play(t *testing.T, a protocol.Assign, answer string, tests *protocol.TestResult) {
	t.Helper()
	ctx, now := t.Context(), time.Now()
	ex, sc := runnerExecutions{e.cp.wf}, runnerScope{apv: e.cp.approvals, wf: e.cp.wf}
	if err := ex.StartAttempt(ctx, e.runnerID, a.Step, a.Fence, now, a.Adapter); err != nil {
		t.Fatal(err)
	}
	if tests != nil {
		if err := ex.RecordTests(ctx, e.runnerID, a.Step, a.Fence, *tests); err != nil {
			t.Fatal(err)
		}
	}
	if err := ex.RecordAgentReport(ctx, e.runnerID, a.Step, a.Fence, protocol.AgentReport{Session: "s", Result: answer}); err != nil {
		t.Fatal(err)
	}
	if err := sc.CheckChanges(ctx, e.runnerID, a.Step, a.Fence, protocol.WorkspaceChanges{BaseSHA: "aaaa1111", HeadSHA: acceptHead,
		Files: []protocol.ChangedFile{{Path: "api/login.go", Status: "A"}}, FilesTotal: 1}); err != nil {
		t.Fatal(err)
	}
	if err := ex.CompleteAttempt(ctx, e.runnerID, a.Step, a.Fence, now); err != nil {
		t.Fatal(err)
	}
}

// issueTask creates the task of YouTrack issue KIN-7 and submits a one-step plan of the given risk naming the
// Knowledge Base article it documents.
func (e *acceptanceEnv) issueTask(t *testing.T, risk workflow.Risk) workflow.Task {
	t.Helper()
	ctx := t.Context()
	task, err := e.cp.wf.CreateTask(ctx, "p1", "Add a login endpoint", acceptIssue, "owner1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cp.wf.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	plan := workflow.Plan{Risk: risk, MaxParallel: 1, Summary: "Add the login endpoint", Tests: []string{"login test"},
		Docs:  []string{acceptArticle},
		Steps: []workflow.PlanStep{{Key: "api", Title: "Add the endpoint", Repository: "acme/app", Scope: []string{"api/**"}}}}
	if _, err := e.cp.wf.SubmitPlan(ctx, task.ID, plan, "u1"); err != nil {
		t.Fatal(err)
	}
	return e.cp.detail(t, task.ID).Task
}

// implementAndPublish runs the READY task through the implementer (fake CLI with test evidence and a Knowledge Base
// section), the independent reviewer and the publisher, and waits until the pull request into dev is open.
func (e *acceptanceEnv) implementAndPublish(t *testing.T, taskID string) {
	t.Helper()
	ctx := t.Context()
	schedule := func() protocol.Assign {
		t.Helper()
		if err := e.cp.wf.Schedule(ctx); err != nil {
			t.Fatal(err)
		}
		return lastAssign(t, e.cp, e.runnerID)
	}
	impl := schedule()
	if impl.Role != "implementer" || impl.TestCommand != "make test" {
		t.Fatalf("implementer assignment %+v", impl)
	}
	e.play(t, impl, `{"summary":"added login","articles":[{"article":"`+acceptArticle+`","section":"Login",`+
		`"content":"POST /api/login signs a user in."}],"openQuestions":[]}`,
		&protocol.TestResult{Command: "make test", ExitCode: 0, DurationMS: 900, Output: "ok"})
	rev := schedule()
	if rev.Role != "reviewer" || rev.Adapter == impl.Adapter || rev.DiffHead != acceptHead {
		t.Fatalf("review assignment %+v (implementer %s)", rev, impl.Adapter)
	}
	e.play(t, rev, `{"verdict":"approve","summary":"good","findings":[]}`, nil)
	pub := schedule()
	if pub.Role != protocol.RolePublisher || pub.DiffHead != acceptHead {
		t.Fatalf("publish assignment %+v", pub)
	}
	ex, now := runnerExecutions{e.cp.wf}, time.Now()
	if err := ex.StartAttempt(ctx, e.runnerID, pub.Step, pub.Fence, now, pub.Adapter); err != nil {
		t.Fatal(err)
	}
	branch := "feature/" + taskID + "--" + pub.Step
	if err := ex.RecordPush(ctx, e.runnerID, pub.Step, pub.Fence, protocol.WorkspacePushed{Repository: "acme/app", Branch: branch,
		HeadSHA: acceptHead}); err != nil {
		t.Fatal(err)
	}
	if err := ex.CompleteAttempt(ctx, e.runnerID, pub.Step, pub.Fence, now); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pull request into dev", func() bool {
		arts, err := e.cp.wf.TaskChangeSet(ctx, taskID)
		return err == nil && len(arts) == 1 && arts[0].PRNumber == acceptPR && arts[0].PRBase == "dev" &&
			arts[0].State == workflow.ArtifactOpen
	})
	if d := e.cp.detail(t, taskID); d.Task.State != workflow.TaskVerifying {
		t.Fatalf("after publishing: %s", describe(d))
	}
}

// checkSuite delivers a signed GitHub check_suite webhook for the published branch; it reports the outcome.
func (e *acceptanceEnv) checkSuite(t *testing.T, taskID, delivery, conclusion string) integrationsapp.WebhookOutcome {
	t.Helper()
	arts, err := e.cp.wf.TaskChangeSet(t.Context(), taskID)
	if err != nil || len(arts) != 1 {
		t.Fatalf("change set %+v %v", arts, err)
	}
	body, _ := json.Marshal(map[string]any{"action": "completed", "repository": map[string]any{"full_name": "acme/app"},
		"installation": map[string]any{"id": 77}, "check_suite": map[string]any{"head_branch": arts[0].Branch,
			"head_sha": acceptHead, "status": "completed", "conclusion": conclusion,
			"pull_requests": []any{map[string]any{"number": acceptPR}}}})
	mac := hmac.New(sha256.New, []byte(acceptHookToken))
	mac.Write(body)
	out, err := e.integ.GitHub.ReceiveWebhook(t.Context(), integrationsapp.WebhookDelivery{ID: delivery, Event: "check_suite",
		Signature: "sha256=" + hex.EncodeToString(mac.Sum(nil)), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *acceptanceEnv) pendingApproval(t *testing.T, taskID string, scope approvals.Scope) approvalsapp.ListItem {
	t.Helper()
	items, err := e.cp.approvals.List(t.Context(), ownerP, approvalsapp.ListQuery{TaskID: taskID,
		States: []approvals.State{approvals.StatePending}})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Scope == scope {
			return it
		}
	}
	t.Fatalf("no pending %s approval among %+v", scope, items)
	return approvalsapp.ListItem{}
}

func (e *acceptanceEnv) approve(t *testing.T, a approvalsapp.ListItem) {
	t.Helper()
	if _, err := e.cp.approvals.Decide(t.Context(), ownerP, a.ID, approvalsapp.DecideInput{Approve: true, Revision: a.Revision,
		PlanHash: a.PlanHash}); err != nil {
		t.Fatalf("approving %s: %v", a.Scope, err)
	}
}

// completed waits for the task to complete and checks what reached GitHub and YouTrack: one merge commit of the
// reviewed head into dev, nothing aimed at main/master, the plan and final comments and the Knowledge Base section.
func (e *acceptanceEnv) completed(t *testing.T, taskID string) {
	t.Helper()
	e.cp.waitTask(t, taskID, func(d workflowapp.TaskDetail) bool { return d.Task.State == workflow.TaskCompleted })
	arts, err := e.cp.wf.TaskChangeSet(t.Context(), taskID)
	if err != nil || len(arts) != 1 || arts[0].Merge != workflow.MergeMerged || arts[0].MergeSHA != acceptMergeSHA {
		t.Fatalf("change set after the merge %+v %v", arts, err)
	}
	merges, _, protected := e.gh.snapshot()
	if len(merges) != 1 || merges[0] != acceptHead+" merge" {
		t.Fatalf("merges %v, want exactly one merge commit of the reviewed head", merges)
	}
	if len(protected) != 0 {
		t.Fatalf("requests naming main/master: %v", protected)
	}
	eventually(t, "the final YouTrack comment and the Knowledge Base section", func() bool {
		comments, article, _ := e.yt.snapshot()
		final := slices.ContainsFunc(comments, func(c string) bool { return strings.Contains(c, "github.com/acme/app/pull/12") })
		return final && strings.Contains(article, "POST /api/login signs a user in.")
	})
	eventually(t, "the plan comment with the task link (a write retried after an outage may come later)", func() bool {
		comments, _, _ := e.yt.snapshot()
		return slices.ContainsFunc(comments, func(c string) bool {
			return strings.Contains(c, "Add the login endpoint") && strings.Contains(c, "https://ai.example.com/tasks/"+taskID)
		})
	})
	_, article, _ := e.yt.snapshot()
	if !strings.HasPrefix(article, "# Auth") || strings.Contains(article, "Not yet.") {
		t.Fatalf("article %q: the section must be replaced, the rest kept", article)
	}
}

// AC (KIN-42): an L0 issue goes all the way: plan comment on the issue, implementer with test evidence, independent
// reviewer, publish, pull request into dev, green CI from the GitHub webhook (a duplicate delivery changes nothing),
// merge into dev by policy, final comment and the Knowledge Base section — while YouTrack is down for the first
// writes (they are retried; the task never waits for YouTrack).
func TestAcceptanceL0IssueIsMergedIntoDev(t *testing.T) {
	t.Parallel()
	e := newAcceptanceEnv(t, 2)
	task := e.issueTask(t, workflow.RiskL0)
	if task.State != workflow.TaskReady {
		t.Fatalf("L0 plan: %s", task.State)
	}
	e.implementAndPublish(t, task.ID)
	e.gh.set(func(f *acceptGitHub) { f.conclusion = "success" })
	if out := e.checkSuite(t, task.ID, "delivery-1", "success"); out != integrationsapp.WebhookAccepted {
		t.Fatalf("webhook: %s", out)
	}
	if out := e.checkSuite(t, task.ID, "delivery-1", "success"); out != integrationsapp.WebhookDuplicate {
		t.Fatalf("duplicate webhook: %s", out)
	}
	e.completed(t, task.ID)
	if _, _, failed := e.yt.snapshot(); failed != 2 {
		t.Fatalf("YouTrack outage: %d failed writes, want 2 (retried)", failed)
	}
}

// AC (KIN-42): an L2 issue waits for the Owner twice: the plan is executed only after its approval, and green CI
// never merges until the Owner approved the merge into dev (picked up by the CI reconciliation).
func TestAcceptanceL2IssueIsMergedOnlyAfterTheOwnersApprovals(t *testing.T) {
	t.Parallel()
	e := newAcceptanceEnv(t, 0)
	task := e.issueTask(t, workflow.RiskL2)
	if task.State != workflow.TaskAwaitingApproval {
		t.Fatalf("L2 plan: %s", task.State)
	}
	if err := e.cp.wf.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d := e.cp.detail(t, task.ID); d.Execution != nil {
		t.Fatalf("executed before the approval: %s", describe(d))
	}
	e.approve(t, e.pendingApproval(t, task.ID, approvals.ScopeExecute))
	e.implementAndPublish(t, task.ID)
	e.gh.set(func(f *acceptGitHub) { f.conclusion = "success" })
	if out := e.checkSuite(t, task.ID, "delivery-1", "success"); out != integrationsapp.WebhookAccepted {
		t.Fatalf("webhook: %s", out)
	}
	eventually(t, "the merge held for integrate_dev", func() bool {
		arts, err := e.cp.wf.TaskChangeSet(t.Context(), task.ID)
		return err == nil && len(arts) == 1 && arts[0].Merge == workflow.MergeWaiting && strings.Contains(arts[0].MergeError, "integrate_dev")
	})
	if merges, _, _ := e.gh.snapshot(); len(merges) != 0 {
		t.Fatalf("merged without the Owner's approval: %v", merges)
	}
	e.approve(t, e.pendingApproval(t, task.ID, approvals.ScopeIntegrateDev))
	if err := e.cp.wf.ReconcileCI(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.completed(t, task.ID)
}

// AC (KIN-42, KIN-A-25 "dev moved before the merge"): when dev moved so that the pull request no longer merges, or
// the pull request's head moved, nothing is merged blindly: the task is BLOCKED (reason ci) for the Owner. A pull
// request someone retargeted at main between publishing and the merge is never even sent to the merge API.
func TestAcceptanceDevMovedBeforeTheMergeBlocksTheTask(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		status   int    // GitHub's answer to the merge
		retarget string // base the pull request has when the merge gate reads it
		merges   int
	}{
		"conflict with the new dev":  {status: http.StatusMethodNotAllowed, merges: 1},
		"head moved":                 {status: http.StatusConflict, merges: 1},
		"pull request moved to main": {retarget: "main"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newAcceptanceEnv(t, 0)
			task := e.issueTask(t, workflow.RiskL0)
			e.implementAndPublish(t, task.ID)
			e.gh.set(func(f *acceptGitHub) {
				f.conclusion, f.mergeStatus = "success", c.status
				if c.retarget != "" {
					f.pr["base"] = map[string]any{"ref": c.retarget}
				}
			})
			e.checkSuite(t, task.ID, "delivery-1", "success")
			d := e.cp.waitTask(t, task.ID, func(d workflowapp.TaskDetail) bool { return d.Task.State == workflow.TaskBlocked })
			if d.Task.BlockReason != workflow.WaitCI {
				t.Fatalf("blocked: %s", describe(d))
			}
			arts, _ := e.cp.wf.TaskChangeSet(t.Context(), task.ID)
			if len(arts) != 1 || arts[0].Merge != workflow.MergeFailed || arts[0].MergeSHA != "" {
				t.Fatalf("change set %+v", arts)
			}
			if merges, _, _ := e.gh.snapshot(); len(merges) != c.merges {
				t.Fatalf("merge attempts %v, want %d", merges, c.merges)
			}
		})
	}
}

// AC (KIN-42, KIN-A-25 "two runners race"): two runners eligible for one READY step and concurrent scheduler passes
// give the step to exactly one of them; the other runner's reports for it are refused and change nothing. The
// passes may be serialised inside the Control Plane; the test pins the outcome (one claim, fenced), not the timing.
func TestAcceptanceTwoRunnersCompetingForOneStep(t *testing.T) {
	t.Parallel()
	e := newAcceptanceEnv(t, 0)
	e.pool.mu.Lock()
	e.pool.ids = []string{e.runnerID, e.other}
	e.pool.mu.Unlock()
	task := e.issueTask(t, workflow.RiskL0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := e.cp.wf.Schedule(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	assigned := map[string]protocol.Assign{}
	for _, id := range []string{e.runnerID, e.other} {
		for _, a := range assignsOf(t, e.cp, id) {
			assigned[id+" "+a.Step+" "+strconv.FormatInt(a.Fence, 10)] = a
		}
	}
	if len(assigned) != 1 {
		t.Fatalf("assignments %v, want exactly one", assigned)
	}
	var winner, loser string
	var a protocol.Assign
	for k, v := range assigned {
		winner, a = strings.Fields(k)[0], v
	}
	loser = e.other
	if winner == e.other {
		loser = e.runnerID
	}
	ex := runnerExecutions{e.cp.wf}
	if err := ex.StartAttempt(t.Context(), loser, a.Step, a.Fence, time.Now(), a.Adapter); err == nil {
		t.Fatal("the runner that lost the race started the step")
	}
	if err := ex.StartAttempt(t.Context(), winner, a.Step, a.Fence, time.Now(), a.Adapter); err != nil {
		t.Fatalf("the winner: %v", err)
	}
	if d := e.cp.detail(t, task.ID); d.Execution == nil || len(d.Execution.Steps[0].Attempts) != 1 ||
		d.Execution.Steps[0].Attempts[0].RunnerID != winner {
		t.Fatalf("attempts: %s", describe(d))
	}
}
