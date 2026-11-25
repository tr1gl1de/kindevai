package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type repos struct{}

func (repos) Repositories(_ context.Context, id string) ([]string, error) {
	if id != "p1" {
		return nil, domain.ErrNotFound
	}
	return []string{"acme/app"}, nil
}

type asProposed struct{}

func (asProposed) EffectiveRisk(_ context.Context, _ string, p domain.Plan) (domain.Risk, error) {
	return p.Risk, nil
}

type access map[string]map[string]string

func (a access) Visible(_ context.Context, p auth.Principal) (map[string]string, error) {
	return a[p.UserID], nil
}

type pool struct{}

func (pool) Candidates(context.Context, string) ([]app.Candidate, error) { return nil, nil }

type nopDispatch struct{}

func (nopDispatch) Assign(context.Context, string, app.Assignment) error        { return nil }
func (nopDispatch) Cancel(context.Context, string, string, int64, string) error { return nil }

func newServer(t *testing.T) (*httptest.Server, *app.Service) {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('member', 'member', 'h', 'user', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	var n atomic.Int64
	svc := &app.Service{Repo: sqlite.New(d), Projects: repos{}, Policy: asProposed{}, Tx: db.Transactor{DB: d},
		Now: time.Now, NewID: func() string { return "id" + strings.Repeat("x", int(n.Add(1))) },
		Runners: pool{}, Dispatcher: nopDispatch{},
		Access: access{"owner": {"p1": app.RoleOwner}, "member": {"p1": "member"}}}
	mux := http.NewServeMux()
	(&Handler{Service: svc}).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get("X-User"); u != "" {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: u, Owner: u == "owner"}))
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, svc
}

func call(t *testing.T, srv *httptest.Server, user, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// Task list and details through the API: create, mock run, the scheduler's BLOCKED (waiting for a runner) is
// visible, cancel.
func TestTaskAPI(t *testing.T) {
	t.Parallel()
	srv, svc := newServer(t)
	var d DetailView
	if c := call(t, srv, "owner", "POST", "/api/tasks", `{"projectId":"p1","title":"Try it"}`, &d); c != http.StatusCreated || d.Task.State != "CREATED" {
		t.Fatalf("create %d %+v", c, d)
	}
	id := d.Task.ID
	if c := call(t, srv, "member", "POST", "/api/tasks/"+id+"/mock-run", `{"scenario":"success"}`, nil); c != http.StatusForbidden {
		t.Fatalf("member mock run %d", c)
	}
	var e map[string]string
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/mock-run", `{"scenario":"bogus"}`, &e); c != http.StatusUnprocessableEntity || e["message"] != "scenario_unknown" {
		t.Fatalf("bad scenario %d %v", c, e)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/mock-run", `{"scenario":"success"}`, &d); c != http.StatusAccepted || d.Task.State != "READY" || d.Revision == nil || d.Revision.CreatedBy != app.MockPlanner {
		t.Fatalf("mock run %d %+v", c, d)
	}
	if err := svc.Schedule(t.Context()); err != nil { // no runner at all
		t.Fatal(err)
	}
	if c := call(t, srv, "member", "GET", "/api/tasks/"+id, "", &d); c != http.StatusOK || d.Task.State != "BLOCKED" ||
		d.Task.BlockReason != "runner" || d.Task.Reason != app.DetailNoRunner || d.CanManage {
		t.Fatalf("detail %d %+v", c, d.Task)
	}
	if s := d.Execution.Steps; len(s) != 1 || s[0].State != "WAITING" || s[0].WaitReason != "runner" || s[0].Key != "success" {
		t.Fatalf("steps %+v", s)
	}
	var l struct{ Tasks []TaskView }
	if c := call(t, srv, "member", "GET", "/api/tasks?projectId=p1", "", &l); c != http.StatusOK || len(l.Tasks) != 1 {
		t.Fatalf("list %d %+v", c, l)
	}
	if c := call(t, srv, "", "GET", "/api/tasks", "", nil); c != http.StatusUnauthorized {
		t.Fatalf("anonymous %d", c)
	}
	if c := call(t, srv, "stranger", "GET", "/api/tasks/"+id, "", nil); c != http.StatusNotFound {
		t.Fatalf("stranger %d", c)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/cancel", `{"reason":""}`, nil); c != http.StatusUnprocessableEntity {
		t.Fatalf("cancel without reason %d", c)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/cancel", `{"reason":"enough"}`, &d); c != http.StatusOK || d.Task.State != "CANCELLED" || d.Execution.State != "CANCELLED" {
		t.Fatalf("cancel %d %+v", c, d.Task)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/mock-run", `{"scenario":"success"}`, nil); c != http.StatusConflict {
		t.Fatalf("mock run of a cancelled task %d", c)
	}
}

type logOf []app.AttemptEvent

func (l logOf) AttemptEvents(_ context.Context, _ string, _, after int64, limit int) ([]app.AttemptEvent, error) {
	var out []app.AttemptEvent
	for _, e := range l {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (l logOf) AttemptWorkspace(_ context.Context, step string, fence int64) (app.AttemptWorkspace, error) {
	if len(l) == 0 {
		return app.AttemptWorkspace{}, nil
	}
	return app.AttemptWorkspace{
		Prepared: &app.WorkspacePrepared{Repository: "acme/app", Branch: "feature/t--" + step, Base: "dev", BaseSHA: "b1",
			Instructions: []app.InstructionFile{{Path: "<img src=x>/AGENTS.md", SHA256: "aa", Size: 12}, {Path: "CLAUDE.md", SHA256: "bb", Symlink: true}}},
		Changes: &app.WorkspaceChanges{Branch: "feature/t--" + step, BaseSHA: "b1", HeadSHA: "h" + strconv.FormatInt(fence, 10),
			Files: []app.ChangedFile{{Path: "a.go", Status: "A"}, {Path: "b.go", Status: "D"}}, FilesTotal: 3, Truncated: true,
			InstructionsChanged: []app.InstructionChange{{Path: "CLAUDE.md", Before: "bb"}}},
	}, nil
}

type onePool struct{}

func (onePool) Candidates(context.Context, string) ([]app.Candidate, error) {
	return []app.Candidate{{ID: "r1", DiskHeavyAllowed: true}}, nil
}

// The list filters by state and carries agent time per task and in total; the details carry the attempt's agent,
// work period and status, the step's and task's agent time and the calendar duration; the events endpoint pages an
// attempt's events; retry plans a failed task again (KIN-25).
func TestTaskTimeEventsAndRetryAPI(t *testing.T) {
	t.Parallel()
	srv, svc := newServer(t)
	svc.Runners = onePool{}
	svc.Log = logOf{{Seq: 1, Kind: "execution.started"}, {Seq: 2, Kind: "execution.output", Stream: "stdout", Data: "<b>hi</b>"},
		{Seq: 3, Kind: "execution.failed", Reason: "exit 1"}}
	ctx := t.Context()
	var d DetailView
	call(t, srv, "owner", "POST", "/api/tasks", `{"projectId":"p1","title":"Timed"}`, &d)
	id := d.Task.ID
	call(t, srv, "owner", "POST", "/api/tasks/"+id+"/mock-run", `{"scenario":"fail"}`, nil)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	for i := range 2 {
		if err := svc.Schedule(ctx); err != nil {
			t.Fatal(err)
		}
		call(t, srv, "member", "GET", "/api/tasks/"+id, "", &d)
		s := d.Execution.Steps[0]
		if err := svc.StartStepAt(ctx, s.ID, "r1", s.Fence, at, "mock"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if c := call(t, srv, "member", "GET", "/api/tasks?state=RUNNING", "", &struct{}{}); c != http.StatusOK {
				t.Fatalf("filter %d", c)
			}
		}
		if err := svc.RecordAgentReport(ctx, s.ID, "r1", s.Fence, domain.AgentReport{Session: "sess-" + strconv.Itoa(i),
			Version: "2.1.0", Usage: domain.AgentUsage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.25, Turns: 2}}); err != nil {
			t.Fatal(err)
		}
		if err := svc.FailStepAt(ctx, s.ID, "r1", s.Fence, "exit 1", at.Add(1500*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if c := call(t, srv, "member", "GET", "/api/tasks/"+id, "", &d); c != http.StatusOK || d.Task.State != "FAILED" {
		t.Fatalf("detail %d %+v", c, d.Task)
	}
	a := d.Execution.Steps[0].Attempts[1]
	if a.Agent != "mock" || a.WorkStatus != "complete" || a.WorkMs != 1500 || a.WorkStartedAt == nil || !a.WorkStartedAt.Equal(at) {
		t.Fatalf("attempt %+v", a)
	}
	if a.Session != "sess-1" || a.CLIVersion != "2.1.0" || a.Usage == nil || a.Usage.InputTokens != 100 || a.Usage.Turns != 2 {
		t.Fatalf("attempt usage %+v %+v", a, a.Usage)
	}
	if u := d.Execution.Usage; u == nil || u.InputTokens != 200 || u.OutputTokens != 20 || u.CostUSD != 0.5 || u.Turns != 4 {
		t.Fatalf("execution usage %+v", u)
	}
	if st := d.Execution.Steps[0].AgentTime; st.Ms != 3000 || st.Status != "complete" {
		t.Fatalf("step time %+v", st)
	}
	if d.Time.AgentMs != 3000 || d.Time.AgentStatus != "complete" || !d.Time.Started || d.Time.Running {
		t.Fatalf("task time %+v", d.Time)
	}
	var l ListView
	if c := call(t, srv, "member", "GET", "/api/tasks?projectId=p1&state=FAILED,CANCELLED", "", &l); c != http.StatusOK ||
		len(l.Tasks) != 1 || l.Tasks[0].Time.AgentMs != 3000 || l.Total.Ms != 3000 || l.Total.Status != "complete" {
		t.Fatalf("list %d %+v", c, l)
	}
	if c := call(t, srv, "member", "GET", "/api/tasks?state=READY", "", &l); c != http.StatusOK || len(l.Tasks) != 0 || l.Total.Status != "none" {
		t.Fatalf("empty filter %d %+v", c, l)
	}
	var e map[string]string
	if c := call(t, srv, "member", "GET", "/api/tasks?state=bogus", "", &e); c != http.StatusUnprocessableEntity || e["field"] != "state" {
		t.Fatalf("bad state %d %v", c, e)
	}
	s := d.Execution.Steps[0]
	path := "/api/tasks/" + id + "/steps/" + s.ID + "/attempts/" + strconv.FormatInt(a.Fence, 10) + "/events"
	var p EventPageView
	if c := call(t, srv, "member", "GET", path+"?after=1", "", &p); c != http.StatusOK || len(p.Events) != 2 ||
		p.Events[0].Data != "<b>hi</b>" || p.Events[1].Reason != "exit 1" || p.Next != 3 || p.More {
		t.Fatalf("events %d %+v", c, p)
	}
	for _, bad := range []string{path + "?after=-1", path + "?after=x", "/api/tasks/" + id + "/steps/" + s.ID + "/attempts/x/events"} {
		if c := call(t, srv, "member", "GET", bad, "", nil); c != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", bad, c)
		}
	}
	if c := call(t, srv, "stranger", "GET", path, "", nil); c != http.StatusNotFound {
		t.Fatalf("stranger events %d", c)
	}
	if c := call(t, srv, "member", "POST", "/api/tasks/"+id+"/retry", `{}`, nil); c != http.StatusForbidden {
		t.Fatalf("member retry %d", c)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/retry", `{}`, &d); c != http.StatusOK || d.Task.State != "READY" || d.Task.PlanRevision != 2 {
		t.Fatalf("retry %d %+v", c, d.Task)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/retry", `{}`, nil); c != http.StatusConflict {
		t.Fatalf("retry of a ready task %d", c)
	}
}

// An attempt's workspace endpoint serves the latest workspace.prepared and workspace.changes reports as typed data
// (paths stay text) to whoever may see the task; an attempt without reports has nulls (KIN-70).
func TestAttemptWorkspaceAPI(t *testing.T) {
	t.Parallel()
	srv, svc := newServer(t)
	svc.Runners = onePool{}
	ctx := t.Context()
	var d DetailView
	call(t, srv, "owner", "POST", "/api/tasks", `{"projectId":"p1","title":"Workspace"}`, &d)
	id := d.Task.ID
	call(t, srv, "owner", "POST", "/api/tasks/"+id+"/mock-run", `{"scenario":"success"}`, nil)
	if err := svc.Schedule(ctx); err != nil {
		t.Fatal(err)
	}
	call(t, srv, "member", "GET", "/api/tasks/"+id, "", &d)
	s := d.Execution.Steps[0]
	if err := svc.StartStepAt(ctx, s.ID, "r1", s.Fence, time.Now(), "mock"); err != nil {
		t.Fatal(err)
	}
	path := "/api/tasks/" + id + "/steps/" + s.ID + "/attempts/" + strconv.FormatInt(s.Fence, 10) + "/workspace"
	var raw map[string]any
	if c := call(t, srv, "member", "GET", path, "", &raw); c != http.StatusOK || raw["prepared"] != nil || raw["changes"] != nil {
		t.Fatalf("no reports %d %v", c, raw)
	}
	svc.Log = logOf{{Seq: 1, Kind: "workspace.prepared"}}
	var w WorkspaceView
	if c := call(t, srv, "member", "GET", path, "", &w); c != http.StatusOK || w.Prepared == nil || w.Changes == nil {
		t.Fatalf("workspace %d %+v", c, w)
	}
	p, ch := w.Prepared, w.Changes
	if p.Repository != "acme/app" || p.Branch != "feature/t--"+s.ID || p.Base != "dev" || p.BaseSHA != "b1" || len(p.Instructions) != 2 ||
		p.Instructions[0].Path != "<img src=x>/AGENTS.md" || p.Instructions[0].Size != 12 || !p.Instructions[1].Symlink {
		t.Fatalf("prepared %+v", p)
	}
	if ch.HeadSHA != "h"+strconv.FormatInt(s.Fence, 10) || len(ch.Files) != 2 || ch.Files[1].Status != "D" || ch.FilesTotal != 3 ||
		!ch.Truncated || len(ch.InstructionsChanged) != 1 || ch.InstructionsChanged[0].Before != "bb" || ch.InstructionsChanged[0].After != "" {
		t.Fatalf("changes %+v", ch)
	}
	if c := call(t, srv, "member", "GET", "/api/tasks/"+id+"/steps/"+s.ID+"/attempts/x/workspace", "", nil); c != http.StatusUnprocessableEntity {
		t.Fatalf("bad fence %d", c)
	}
	if c := call(t, srv, "stranger", "GET", path, "", nil); c != http.StatusNotFound {
		t.Fatalf("stranger workspace %d", c)
	}
	if c := call(t, srv, "member", "GET", "/api/tasks/"+id+"/steps/"+s.ID+"/attempts/99/workspace", "", nil); c != http.StatusNotFound {
		t.Fatalf("unknown attempt %d", c)
	}
}

type plannerRoute struct{}

func (plannerRoute) Route(context.Context, string, string, string) (app.AgentRoute, error) {
	return app.AgentRoute{Chain: []domain.AgentChoice{{Agent: "claude"}}}, nil
}

type plannerPool struct{}

func (plannerPool) Candidates(context.Context, string) ([]app.Candidate, error) {
	return []app.Candidate{{ID: "r1", Tools: []string{"claude"}, DiskHeavyAllowed: true}}, nil
}

// KIN-34: POST /plan starts the planner; the task page shows the planning run and the plan's steps and impact.
func TestPlanAPI(t *testing.T) {
	t.Parallel()
	srv, svc := newServer(t)
	var d DetailView
	call(t, srv, "owner", "POST", "/api/tasks", `{"projectId":"p1","title":"Login"}`, &d)
	id := d.Task.ID
	var e map[string]string
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/plan", "", &e); c != http.StatusServiceUnavailable || e["error"] != "planner_unavailable" {
		t.Fatalf("planner off %d %v", c, e)
	}
	svc.Planning, svc.Agents, svc.Runners = svc.Repo.(*sqlite.Repository), plannerRoute{}, plannerPool{}
	if c := call(t, srv, "member", "POST", "/api/tasks/"+id+"/plan", "", nil); c != http.StatusForbidden {
		t.Fatalf("member plan %d", c)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/plan", "", &d); c != http.StatusAccepted || d.Task.State != "ANALYZING" ||
		d.Planning == nil || d.Planning.State != "QUEUED" || d.Planning.MaxAttempts != domain.MaxPlanningAttempts {
		t.Fatalf("plan %d %+v %+v", c, d.Task, d.Planning)
	}
	if c := call(t, srv, "owner", "POST", "/api/tasks/"+id+"/plan", "", nil); c != http.StatusConflict {
		t.Fatalf("plan twice %d", c)
	}
	if err := svc.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	call(t, srv, "owner", "GET", "/api/tasks/"+id, "", &d)
	p := d.Planning
	if p.State != "RUNNING" || p.Agent != "claude" || p.RunnerID != "r1" || p.Attempt != 1 || !strings.HasPrefix(p.StepID, domain.PlanningStepPrefix) {
		t.Fatalf("running %+v", p)
	}
	ctx := t.Context()
	plan := `{"summary":"s","risk":"L1","maxParallel":1,"tests":["unit"],"steps":[{"key":"a","title":"A","repository":"acme/app","scope":["x/"],"dependsOn":[],"capabilities":[]}]}`
	if err := svc.RecordPlanningReport(ctx, p.StepID, "r1", 1, domain.AgentReport{Session: "sess"}, plan); err != nil {
		t.Fatal(err)
	}
	if err := svc.CompletePlanningAttempt(ctx, p.StepID, "r1", 1); err != nil {
		t.Fatal(err)
	}
	call(t, srv, "member", "GET", "/api/tasks/"+id, "", &d)
	if d.Task.State != "READY" || d.Planning.State != "SUCCEEDED" || !d.Planning.HasSession || d.Revision == nil ||
		len(d.Revision.Steps) != 1 || d.Revision.Steps[0].Scope[0] != "x/" || d.Revision.Tests[0] != "unit" ||
		d.Revision.Contracts == nil || d.Revision.CreatedBy != "planner/claude" {
		t.Fatalf("planned %+v %+v %+v", d.Task, d.Planning, d.Revision)
	}
	var page EventPageView
	if c := call(t, srv, "member", "GET", "/api/tasks/"+id+"/steps/"+p.StepID+"/attempts/1/events", "", &page); c != http.StatusOK {
		t.Fatalf("planner attempt events %d", c)
	}
	if c := call(t, srv, "member", "GET", "/api/tasks/"+id+"/steps/"+p.StepID+"/attempts/2/events", "", nil); c != http.StatusNotFound {
		t.Fatalf("unknown planner attempt %d", c)
	}
}
