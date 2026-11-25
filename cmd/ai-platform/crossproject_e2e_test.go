package main

import (
	"encoding/json"
	"fmt"
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
// any project- or task-scoped endpoint of the served Control Plane. Beta has a real repository, plan revision, step
// and discussion message, and every read the admin is refused succeeds for the Owner (positive control), so no row
// passes only because its id does not exist. Reads answer 404 like a missing project, writes are refused (403/404),
// no answer names Beta's task, and Beta looks exactly the same to the Owner afterwards.
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

	// Beta: a repository, a task planned by the mock planner (revision 1, one step; no runner, so it waits) and a
	// plan discussion message.
	repo, _ := owner.must(201, "POST", "/api/projects/"+beta+"/repositories", `{"owner":"acme","name":"beta"}`)["id"].(string)
	const secret = "Beta confidential roadmap"
	created := owner.must(201, "POST", "/api/tasks", `{"projectId":"`+beta+`","title":"`+secret+`"}`)
	view, _ := created["task"].(map[string]any)
	task, _ := view["id"].(string)
	if task == "" || repo == "" {
		t.Fatalf("created task %v, repository %q", created, repo)
	}
	p, tk := "/api/projects/"+beta, "/api/tasks/"+task
	// Every Beta plan needs the Owner's approval, so the plan stays open for the discussion until approved.
	owner.must(200, "PUT", p+"/risk-policy", `{"rules":[{"category":"paths","risk":"L2","patterns":["**"]}]}`)
	owner.must(202, "POST", tk+"/mock-run", `{"scenario":"success"}`)
	msg, _ := owner.must(202, "POST", tk+"/discussion/messages", `{"text":"Why this step?"}`)["id"].(string)
	pending := func() map[string]map[string]any {
		_, body := owner.do("GET", "/api/approvals?state=pending", "")
		var list []map[string]any
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatalf("approvals %s: %v", body, err)
		}
		out := map[string]map[string]any{}
		for _, a := range list {
			out[a["scope"].(string)] = a
		}
		return out
	}
	exec := pending()["execute"]
	if msg == "" || exec == nil {
		t.Fatalf("discussion message %q, approvals %v", msg, pending())
	}
	owner.must(200, "POST", "/api/approvals/"+exec["id"].(string)+"/approve", fmt.Sprintf(`{"planRevision":%v,"planHash":%q}`,
		exec["planRevision"], exec["planHash"]))
	integ := pending()["integrate_dev"]
	if integ == nil {
		t.Fatalf("no pending integrate_dev approval: %v", pending())
	}
	approval := "/api/approvals/" + integ["id"].(string)
	var step string
	eventually(t, "Beta's task waiting for a runner", func() bool {
		d := owner.must(200, "GET", tk, "")
		task, _ := d["task"].(map[string]any)
		ex, _ := d["execution"].(map[string]any)
		steps, _ := ex["steps"].([]any)
		if len(steps) == 1 {
			step, _ = steps[0].(map[string]any)["id"].(string)
		}
		return task["state"] == "BLOCKED" && step != ""
	})
	eventually(t, "the discussion message settled (the approved plan can no longer change)", func() bool {
		d := owner.must(200, "GET", tk+"/discussion", "")
		msgs, _ := d["messages"].([]any)
		return len(msgs) == 1 && msgs[0].(map[string]any)["status"] == "FAILED"
	})
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, path := range []string{p, approval, "/api/approvals?state=all", p + "/members", p + "/integrations", p + "/youtrack", p + "/risk-policy", p + "/agents",
			tk + "/revisions/1", tk + "/discussion"} {
			_, out[path] = owner.do("GET", path, "")
		}
		var list struct{ Tasks []map[string]any }
		_, body := owner.do("GET", "/api/tasks?projectId="+beta, "")
		_ = json.Unmarshal([]byte(body), &list)
		for _, it := range list.Tasks {
			delete(it, "time") // the calendar time of a waiting task grows by itself
		}
		b, _ := json.Marshal(list.Tasks)
		out["tasks of Beta"] = string(b)
		d := owner.must(200, "GET", tk, "")
		b, _ = json.Marshal([]any{d["task"], d["execution"]})
		out[tk] = string(b)
		return out
	}
	before := snapshot()

	carol := newHTTPSession(t, base)
	carol.must(200, "POST", "/api/auth/login", `{"login":"carol","password":"`+temp+`"}`)
	carol.must(204, "POST", "/api/auth/password", `{"currentPassword":"`+temp+`","newPassword":"carol's own long phrase"}`)
	// Carol's own project works: the refusals below are about the project, not about her role.
	carol.must(200, "GET", "/api/projects/"+alpha, "")
	carol.must(201, "POST", "/api/tasks", `{"projectId":"`+alpha+`","title":"Alpha task"}`)

	ownerOnly := map[string]bool{p + "/integrations": true} // refused to every non-Owner (403) whatever the project
	// No attempt exists without a runner: the attempt reads are checked for the refusal only.
	noControl := map[string]bool{tk + "/steps/" + step + "/attempts/1/events": true, tk + "/steps/" + step + "/attempts/1/workspace": true}
	r, st, m := p+"/repositories/"+repo, tk+"/steps/"+step, tk+"/discussion/messages/"+msg
	for _, c := range []struct{ method, path, body string }{
		{"GET", p, ""},
		{"GET", approval, ""},
		{"POST", approval + "/approve", fmt.Sprintf(`{"planRevision":%v,"planHash":%q}`, integ["planRevision"], integ["planHash"])},
		{"POST", approval + "/reject", fmt.Sprintf(`{"planRevision":%v,"planHash":%q,"comment":"no"}`, integ["planRevision"], integ["planHash"])},
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
		{"POST", p + "/repositories", `{"owner":"acme","name":"beta2"}`},
		{"PUT", r, `{"integrationBranch":"release"}`},
		{"DELETE", r, ""},
		{"POST", r + "/sync", ""},
		{"GET", p + "/risk-policy", ""},
		{"PUT", p + "/risk-policy", `{"rules":[]}`},
		{"GET", p + "/agents", ""},
		{"PUT", p + "/agents/switches/claude", `{"enabled":false}`},
		{"PUT", p + "/agents/bindings/implementer", `{"chain":[{"agent":"claude"}]}`},
		{"DELETE", p + "/agents/bindings/implementer", ""},
		{"POST", "/api/tasks", `{"projectId":"` + beta + `","title":"injected"}`},
		{"GET", tk, ""},
		{"GET", tk + "/revisions/1", ""},
		{"GET", tk + "/discussion", ""},
		{"POST", tk + "/discussion/messages", `{"text":"hello"}`},
		{"POST", m + "/retry", ""},
		{"GET", st + "/attempts/1/events", ""},
		{"GET", st + "/attempts/1/workspace", ""},
		{"POST", tk + "/plan", ""},
		{"POST", tk + "/mock-run", `{"scenario":"fail"}`},
		{"POST", tk + "/retry", ""},
		{"POST", tk + "/resume", ""},
		{"POST", tk + "/retry-merge", ""},
		{"POST", tk + "/cancel", `{"reason":"no"}`},
	} {
		code, body := carol.do(c.method, c.path, c.body)
		if (code != http.StatusNotFound && code != http.StatusForbidden) || strings.Contains(body, secret) {
			t.Errorf("%s %s = %d %s, want 404/403 without Beta's data", c.method, c.path, code, body)
		}
		if c.method != "GET" {
			continue
		}
		if !ownerOnly[c.path] && code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (another project looks missing)", c.method, c.path, code)
		}
		if oc, ob := owner.do("GET", c.path, ""); !noControl[c.path] && oc != http.StatusOK {
			t.Errorf("positive control: the Owner's GET %s = %d %s", c.path, oc, ob)
		}
	}
	for _, path := range []string{"/api/tasks", "/api/tasks?projectId=" + beta, "/api/approvals?state=all", "/api/projects"} {
		if code, body := carol.do("GET", path, ""); code >= 500 || strings.Contains(body, secret) || strings.Contains(body, `"`+beta+`"`) {
			t.Errorf("GET %s = %d %s: lists Beta", path, code, body)
		}
	}
	// Nothing changed in Beta: settings, members, repositories, task, plan, step and discussion look the same.
	after := snapshot()
	for path, b := range before {
		if after[path] != b {
			t.Errorf("Beta changed at %s:\nbefore %s\nafter  %s", path, b, after[path])
		}
	}
}
