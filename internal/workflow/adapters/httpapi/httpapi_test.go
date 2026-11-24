package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
