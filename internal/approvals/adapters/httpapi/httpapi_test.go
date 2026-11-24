package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/adapters/httpapi"
	approvalsdb "github.com/tr1gl1de/kindevai/internal/approvals/adapters/sqlite"
	bridge "github.com/tr1gl1de/kindevai/internal/approvals/adapters/workflow"
	"github.com/tr1gl1de/kindevai/internal/approvals/app"
	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	wfdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	wfapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	wf "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

var people = map[string]auth.Principal{
	"owner": {UserID: "u-owner", Login: "owner", Owner: true},
	"alice": {UserID: "u-alice", Login: "alice"}, // project admin of p1
	"bob":   {UserID: "u-bob", Login: "bob"},     // member of p1
}

type projects struct{}

func (projects) Visible(_ context.Context, p auth.Principal) ([]app.ProjectRef, error) {
	switch {
	case p.Owner:
		return []app.ProjectRef{{ID: "p1", Name: "Alpha", Role: app.RoleOwner}}, nil
	case p.Login == "alice":
		return []app.ProjectRef{{ID: "p1", Name: "Alpha", Role: app.RoleProjectAdmin}}, nil
	case p.Login == "bob":
		return []app.ProjectRef{{ID: "p1", Name: "Alpha", Role: app.RoleMember}}, nil
	}
	return nil, nil
}

type repos struct{}

func (repos) Repositories(_ context.Context, id string) ([]string, error) {
	if id != "p1" {
		return nil, wf.ErrNotFound
	}
	return []string{"api"}, nil
}

type stack struct {
	h   http.Handler
	d   *sql.DB
	wf  *wfapp.Service
	apv *app.Service
}

func newStack(t *testing.T) stack {
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
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u-owner', 'owner', 'h', 'owner', 'x'), ('u-alice', 'alice', 'h', 'user', 'x'), ('u-bob', 'bob', 'h', 'user', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	var n atomic.Int64
	newID := func() string { return fmt.Sprintf("id%d", n.Add(1)) }
	tx := db.Transactor{DB: d}
	apv := &app.Service{Repo: approvalsdb.New(d), Projects: projects{}, Tx: tx,
		Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Outbox: messaging.NewOutbox(d, messaging.Config{}),
		Now: time.Now, NewID: newID}
	w := &wfapp.Service{Repo: wfdb.New(d), Projects: repos{}, Policy: bridge.Hooks{Svc: apv}, Events: bridge.Hooks{Svc: apv},
		Tx: tx, Now: time.Now, NewID: newID}
	apv.Workflow = bridge.Port{Svc: w}
	mux := http.NewServeMux()
	(&httpapi.Handler{Service: apv}).Register(mux)
	h := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if p, ok := people[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(rw, r)
	})
	return stack{h: h, d: d, wf: w, apv: apv}
}

func (s stack) do(t *testing.T, user, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Test-User", user)
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func (s stack) list(t *testing.T, user, query string) []map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/approvals"+query, nil)
	r.Header.Set("X-Test-User", user)
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func plan(risk wf.Risk, summary string, scope ...string) wf.Plan {
	return wf.Plan{Risk: risk, MaxParallel: 1, Summary: summary, Steps: []wf.PlanStep{
		{Key: "backend", Title: "API", Repository: "api", Scope: scope}}}
}

func pendingExecute(t *testing.T, items []map[string]any) map[string]any {
	t.Helper()
	for _, it := range items {
		if it["scope"] == "execute" && it["state"] == "pending" {
			return it
		}
	}
	t.Fatalf("no pending execute approval in %v", items)
	return nil
}

func count(t *testing.T, d *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The Approval Center end to end on SQLite: the policy raises the AI's L1 to L2, the task waits for the Owner,
// a newer revision makes the old approval stale (409), only the Owner decides (403), decisions are audited.
func TestApprovalCenterFlow(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	ctx := t.Context()
	task, err := s.wf.CreateTask(ctx, "p1", "Add billing tables", "", "u-bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.wf.StartAnalysis(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	r1, err := s.wf.SubmitPlan(ctx, task.ID, plan(wf.RiskL1, "v1", "db/migrations"), "planner")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Risk() != wf.RiskL2 {
		t.Fatalf("effective risk = %s, want the policy's L2", r1.Risk())
	}
	if got, _ := s.wf.Task(ctx, task.ID); got.State != wf.TaskAwaitingApproval {
		t.Fatalf("task = %s", got.State)
	}

	pend := s.list(t, "owner", "?state=pending")
	if len(pend) != 2 {
		t.Fatalf("pending = %v", pend)
	}
	old := pendingExecute(t, pend)
	if old["risk"] != "L2" || old["proposedRisk"] != "L1" || old["projectName"] != "Alpha" {
		t.Fatalf("approval = %v", old)
	}
	if got := s.list(t, "alice", ""); len(got) != 2 {
		t.Fatalf("project admin sees %d", len(got))
	}
	if got := s.list(t, "bob", ""); len(got) != 0 {
		t.Fatalf("member sees %d", len(got))
	}
	if w, _ := s.do(t, "owner", http.MethodGet, "/api/approvals?state=weird", nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad filter = %d", w.Code)
	}

	approve1 := map[string]any{"planRevision": 1, "planHash": r1.Hash(), "comment": ""}
	for _, u := range []string{"alice", "bob"} {
		if w, _ := s.do(t, u, http.MethodPost, "/api/approvals/"+old["id"].(string)+"/approve", approve1); w.Code != http.StatusForbidden {
			t.Fatalf("%s approve = %d", u, w.Code)
		}
	}

	// The plan chat produces revision 2: revision 1 can no longer be approved.
	r2, err := s.wf.SubmitPlan(ctx, task.ID, plan(wf.RiskL1, "v2", "db/migrations", "docs"), "chat")
	if err != nil {
		t.Fatal(err)
	}
	w, body := s.do(t, "owner", http.MethodPost, "/api/approvals/"+old["id"].(string)+"/approve", approve1)
	if w.Code != http.StatusConflict || body["error"] != "stale_revision" {
		t.Fatalf("approve old revision = %d %v", w.Code, body)
	}
	cur := pendingExecute(t, s.list(t, "owner", "?state=pending"))
	w, body = s.do(t, "owner", http.MethodPost, "/api/approvals/"+cur["id"].(string)+"/approve", approve1)
	if w.Code != http.StatusConflict || body["error"] != "stale_revision" {
		t.Fatalf("approve the new approval with the old revision = %d %v", w.Code, body)
	}

	w, body = s.do(t, "alice", http.MethodGet, "/api/approvals/"+cur["id"].(string), nil)
	if w.Code != http.StatusOK || body["canDecide"] != false || body["previous"] == nil || len(body["changes"].([]any)) == 0 {
		t.Fatalf("detail for the project admin = %d %v", w.Code, body)
	}
	if w, _ := s.do(t, "bob", http.MethodGet, "/api/approvals/"+cur["id"].(string), nil); w.Code != http.StatusNotFound {
		t.Fatalf("member detail = %d", w.Code)
	}
	if _, body := s.do(t, "owner", http.MethodGet, "/api/approvals/"+cur["id"].(string), nil); body["canDecide"] != true {
		t.Fatal("the Owner can decide")
	}

	reject := map[string]any{"planRevision": 2, "planHash": r2.Hash(), "comment": " "}
	if w, body := s.do(t, "owner", http.MethodPost, "/api/approvals/"+cur["id"].(string)+"/reject", reject); w.Code != http.StatusUnprocessableEntity || body["field"] != "comment" {
		t.Fatalf("reject without comment = %d %v", w.Code, body)
	}
	approve2 := map[string]any{"planRevision": 2, "planHash": r2.Hash(), "comment": "migration reviewed"}
	w, body = s.do(t, "owner", http.MethodPost, "/api/approvals/"+cur["id"].(string)+"/approve", approve2)
	if w.Code != http.StatusOK || body["state"] != "approved" || body["approver"] != "owner" {
		t.Fatalf("approve = %d %v", w.Code, body)
	}
	if got, _ := s.wf.Task(ctx, task.ID); got.State != wf.TaskReady {
		t.Fatalf("task after approval = %s", got.State)
	}
	if w, body := s.do(t, "owner", http.MethodPost, "/api/approvals/"+cur["id"].(string)+"/approve", approve2); w.Code != http.StatusConflict || body["error"] != "already_decided" {
		t.Fatalf("second decision = %d %v", w.Code, body)
	}

	// integrate_dev is separate and waits for verification.
	var integ map[string]any
	for _, it := range s.list(t, "owner", "?state=pending") {
		if it["scope"] == "integrate_dev" {
			integ = it
		}
	}
	if integ == nil || integ["planRevision"].(float64) != 2 {
		t.Fatalf("integrate_dev request = %v", integ)
	}
	if w, body := s.do(t, "owner", http.MethodPost, "/api/approvals/"+integ["id"].(string)+"/approve", approve2); w.Code != http.StatusConflict || body["error"] != "not_ready" {
		t.Fatalf("integrate_dev before verification = %d %v", w.Code, body)
	}

	if n := count(t, s.d, "SELECT count(*) FROM audit_events WHERE kind = 'approval.approved'"); n != 1 {
		t.Fatalf("approved audit events = %d", n)
	}
	if n := count(t, s.d, "SELECT count(*) FROM audit_events WHERE kind = 'approval.superseded'"); n != 2 {
		t.Fatalf("superseded audit events = %d", n)
	}
	if n := count(t, s.d, "SELECT count(*) FROM outbox_events WHERE topic = 'telegram.approval_requested'"); n != 2 {
		t.Fatalf("notifications = %d", n)
	}
}

func TestRiskPolicyAPI(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	w, body := s.do(t, "alice", http.MethodGet, "/api/projects/p1/risk-policy", nil)
	if w.Code != http.StatusOK || body["custom"] != false || len(body["rules"].([]any)) != 4 {
		t.Fatalf("default policy = %d %v", w.Code, body)
	}
	if w, _ := s.do(t, "bob", http.MethodGet, "/api/projects/p1/risk-policy", nil); w.Code != http.StatusForbidden {
		t.Fatalf("member read = %d", w.Code)
	}
	req := map[string]any{"rules": []map[string]any{{"category": "paths", "risk": "L3", "patterns": []string{"billing/**"}}}}
	if w, _ := s.do(t, "alice", http.MethodPut, "/api/projects/p1/risk-policy", req); w.Code != http.StatusForbidden {
		t.Fatalf("project admin write = %d", w.Code)
	}
	w, body = s.do(t, "owner", http.MethodPut, "/api/projects/p1/risk-policy", req)
	if w.Code != http.StatusOK || body["custom"] != true {
		t.Fatalf("owner write = %d %v", w.Code, body)
	}
	bad := map[string]any{"rules": []map[string]any{{"category": "paths", "risk": "L3", "patterns": []string{"../x"}}}}
	if w, body := s.do(t, "owner", http.MethodPut, "/api/projects/p1/risk-policy", bad); w.Code != http.StatusUnprocessableEntity || body["field"] != "rules[0].patterns" {
		t.Fatalf("bad pattern = %d %v", w.Code, body)
	}
	if w, _ := s.do(t, "owner", http.MethodGet, "/api/projects/p9/risk-policy", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown project = %d", w.Code)
	}

	// The new rule takes effect for the next plan.
	ctx := t.Context()
	task, _ := s.wf.CreateTask(ctx, "p1", "Invoices", "", "u-bob")
	_, _ = s.wf.StartAnalysis(ctx, task.ID)
	rev, err := s.wf.SubmitPlan(ctx, task.ID, plan(wf.RiskL0, "s", "billing/invoice.go"), "planner")
	if err != nil || rev.Risk() != wf.RiskL3 {
		t.Fatalf("revision = %v, %v", rev.Risk(), err)
	}
	if n := count(t, s.d, "SELECT count(*) FROM audit_events WHERE kind = 'risk_policy.updated'"); n != 1 {
		t.Fatalf("policy audit events = %d", n)
	}
}

// A runner reports changes beyond the approved scope: the step stops and waits for the Owner (SQLite end to end).
func TestScopeViolationStopsTheStep(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	ctx := t.Context()
	task, _ := s.wf.CreateTask(ctx, "p1", "Docs", "", "u-bob")
	_, _ = s.wf.StartAnalysis(ctx, task.ID)
	if _, err := s.wf.SubmitPlan(ctx, task.ID, plan(wf.RiskL0, "s", "docs"), "planner"); err != nil {
		t.Fatal(err)
	}
	ex, err := s.wf.StartExecution(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := ex.StepByKey("backend").ID
	l, err := s.wf.Claim(ctx, step, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.wf.StartStep(ctx, step, "r1", l.Fence); err != nil {
		t.Fatal(err)
	}
	// The runner gateway (KIN-23) calls the use case with the authenticated runner's report.
	if _, err := s.apv.CheckChanges(ctx, app.StepChanges{StepID: step, RunnerID: "r1", Fence: l.Fence,
		Changed: []string{"docs/a.md", "internal/auth/x.go"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.wf.Execution(ctx, ex.ID)
	if st := got.Step(step); st.State != wf.StepWaiting || st.WaitReason != wf.WaitApproval {
		t.Fatalf("step = %+v", st)
	}
	req := pendingExecute(t, s.list(t, "owner", "?state=pending"))
	if req["reason"] != "scope_exceeded" || req["stepKey"] != "backend" {
		t.Fatalf("scope request = %v", req)
	}
	w, body := s.do(t, "owner", http.MethodPost, "/api/approvals/"+req["id"].(string)+"/approve",
		map[string]any{"planRevision": 1, "planHash": req["planHash"], "comment": ""})
	if w.Code != http.StatusOK {
		t.Fatalf("approve scope = %d %v", w.Code, body)
	}
	got, _ = s.wf.Execution(ctx, ex.ID)
	if got.Step(step).State != wf.StepReady {
		t.Fatalf("step after approval = %s", got.Step(step).State)
	}
}
