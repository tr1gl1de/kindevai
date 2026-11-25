package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// httpSession is an HTTP client with its own cookie jar against a served Control Plane.
type httpSession struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newHTTPSession(t *testing.T, base string) *httpSession {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &httpSession{t: t, base: base, c: &http.Client{Jar: jar}}
}

func (s *httpSession) do(method, path, body string) (int, string) {
	s.t.Helper()
	req, _ := http.NewRequestWithContext(s.t.Context(), method, s.base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.c.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// must calls the endpoint, expects status want and decodes the answer's "id" (if any).
func (s *httpSession) must(want int, method, path, body string) map[string]any {
	s.t.Helper()
	code, out := s.do(method, path, body)
	if code != want {
		s.t.Fatalf("%s %s = %d %s, want %d", method, path, code, out, want)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	return m
}

// AC (KIN-42, KIN-A-25 cross-project isolation): an admin of project Alpha reaches nothing of project Beta through
// any project- or task-scoped endpoint of the served Control Plane — reads answer 404 like a missing project and
// writes are refused (403/404) without changing anything, and no answer names Beta's task.
func TestCrossProjectIsolationOfEveryScopedEndpoint(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	base, stop := startServe(t, dataDir)
	defer stop()
	tok, err := os.ReadFile(filepath.Join(dataDir, "secrets", "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	owner := newHTTPSession(t, base)
	owner.must(201, "POST", "/api/setup", `{"token":"`+strings.TrimSpace(string(tok))+
		`","login":"owner","password":"correct horse battery","platformName":"Acme"}`)
	owner.must(200, "POST", "/api/auth/login", `{"login":"owner","password":"correct horse battery"}`)
	alpha := owner.must(201, "POST", "/api/projects", `{"name":"Alpha","slug":"alpha"}`)["id"].(string)
	beta := owner.must(201, "POST", "/api/projects", `{"name":"Beta","slug":"beta"}`)["id"].(string)
	temp := owner.must(201, "POST", "/api/users", `{"login":"carol"}`)["temporaryPassword"].(string)
	owner.must(200, "PUT", "/api/projects/"+alpha+"/members/carol", `{"role":"project_admin"}`)
	const secret = "Beta confidential roadmap"
	created := owner.must(201, "POST", "/api/tasks", `{"projectId":"`+beta+`","title":"`+secret+`"}`)
	view, _ := created["task"].(map[string]any)
	task, _ := view["id"].(string)
	if task == "" {
		t.Fatalf("created task %v", created)
	}

	carol := newHTTPSession(t, base)
	carol.must(200, "POST", "/api/auth/login", `{"login":"carol","password":"`+temp+`"}`)
	carol.must(204, "POST", "/api/auth/password", `{"currentPassword":"`+temp+`","newPassword":"carol's own long phrase"}`)
	// Carol's own project works: the refusals below are about the project, not about her role.
	carol.must(200, "GET", "/api/projects/"+alpha, "")
	carol.must(201, "POST", "/api/tasks", `{"projectId":"`+alpha+`","title":"Alpha task"}`)

	p, tk := "/api/projects/"+beta, "/api/tasks/"+task
	ownerOnly := map[string]bool{p + "/integrations": true} // refused to every non-Owner (403) whatever the project
	for _, c := range []struct{ method, path, body string }{
		{"GET", p, ""},
		{"GET", p + "/members", ""},
		{"PUT", p + "/members/carol", `{"role":"member"}`},
		{"DELETE", p + "/members/owner", ""},
		{"GET", p + "/integrations", ""},
		{"PUT", p + "/integrations/youtrack", `{"shortName":"BETA"}`},
		{"PUT", p + "/integrations/youtrack/states", `{"states":{"RUNNING":"In Progress"}}`},
		{"PUT", p + "/integrations/youtrack/autoplan", `{"autoPlan":true}`},
		{"PUT", p + "/integrations/youtrack/trigger", `{"triggerTag":"kindevai"}`},
		{"GET", p + "/youtrack", ""},
		{"POST", p + "/youtrack/runs", `{"issueId":"BETA-1"}`},
		{"POST", p + "/repositories", `{"owner":"acme","name":"beta"}`},
		{"PUT", p + "/repositories/r1", `{"integrationBranch":"dev"}`},
		{"DELETE", p + "/repositories/r1", ""},
		{"POST", p + "/repositories/r1/sync", ""},
		{"GET", p + "/risk-policy", ""},
		{"PUT", p + "/risk-policy", `{"rules":[]}`},
		{"GET", p + "/agents", ""},
		{"PUT", p + "/agents/switches/claude", `{"enabled":true}`},
		{"PUT", p + "/agents/bindings/implementer", `{"chain":[{"agent":"claude"}]}`},
		{"DELETE", p + "/agents/bindings/implementer", ""},
		{"POST", "/api/tasks", `{"projectId":"` + beta + `","title":"injected"}`},
		{"GET", tk, ""},
		{"GET", tk + "/revisions/1", ""},
		{"GET", tk + "/discussion", ""},
		{"POST", tk + "/discussion/messages", `{"text":"hello"}`},
		{"POST", tk + "/discussion/messages/m1/retry", ""},
		{"GET", tk + "/steps/s1/attempts/1/events", ""},
		{"GET", tk + "/steps/s1/attempts/1/workspace", ""},
		{"POST", tk + "/plan", ""},
		{"POST", tk + "/mock-run", `{"scenario":"success"}`},
		{"POST", tk + "/retry", ""},
		{"POST", tk + "/cancel", `{"reason":"no"}`},
	} {
		code, body := carol.do(c.method, c.path, c.body)
		if (code != http.StatusNotFound && code != http.StatusForbidden) || strings.Contains(body, secret) {
			t.Errorf("%s %s = %d %s, want 404/403 without Beta's data", c.method, c.path, code, body)
		}
		if c.method == "GET" && !ownerOnly[c.path] && code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (another project looks missing)", c.method, c.path, code)
		}
	}
	for _, path := range []string{"/api/tasks", "/api/tasks?projectId=" + beta, "/api/approvals?state=all", "/api/projects"} {
		if code, body := carol.do("GET", path, ""); code >= 500 || strings.Contains(body, secret) || strings.Contains(body, `"`+beta+`"`) {
			t.Errorf("GET %s = %d %s: lists Beta", path, code, body)
		}
	}
	// Nothing changed in Beta: the task is still new, under its title, and no task was injected.
	got := owner.must(200, "GET", tk, "")
	if b, _ := json.Marshal(got); !strings.Contains(string(b), secret) || strings.Contains(string(b), "CANCELLED") {
		t.Fatalf("Beta's task after the attempts: %s", b)
	}
	if _, body := owner.do("GET", "/api/tasks?projectId="+beta, ""); strings.Contains(body, "injected") {
		t.Fatalf("a task was created in Beta: %s", body)
	}
}
