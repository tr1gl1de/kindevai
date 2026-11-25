package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/routing/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/routing/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/routing/app"
)

type access struct{}

func (access) Visible(_ context.Context, p auth.Principal) (map[string]string, error) {
	switch p.UserID {
	case "owner":
		return map[string]string{"p1": "owner"}, nil
	case "admin":
		return map[string]string{"p1": "project_admin"}, nil
	}
	return map[string]string{"p1": "developer"}, nil
}

type noAudit struct{}

func (noAudit) Record(context.Context, auditapp.Entry) error { return nil }

type stopper struct{}

func (stopper) StopAgent(context.Context, string) (int, error) { return 1, nil }

func server(t *testing.T) *http.ServeMux {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	svc := &app.Service{Repo: sqlite.New(d), Tx: db.Transactor{DB: d}, Audit: noAudit{}, Access: access{},
		Workflow: stopper{}, Now: func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }}
	mux := http.NewServeMux()
	(&httpapi.Handler{Service: svc}).Register(mux)
	return mux
}

func do(t *testing.T, mux *http.ServeMux, who, method, path, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if who != "" {
		r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: who, Login: who, Owner: who == "owner"}))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestGlobalAgentsAPI(t *testing.T) {
	t.Parallel()
	mux := server(t)
	if code, _ := do(t, mux, "admin", "GET", "/api/agents", ""); code != http.StatusForbidden {
		t.Fatalf("admin GET = %d", code)
	}
	if code, _ := do(t, mux, "", "GET", "/api/agents", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", code)
	}
	if code, _ := do(t, mux, "owner", "PUT", "/api/agents/switches/codex", `{"enabled":false}`); code != http.StatusNoContent {
		t.Fatalf("switch = %d", code)
	}
	if code, out := do(t, mux, "owner", "PUT", "/api/agents/switches/zcode", `{"enabled":false}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown CLI = %d %v", code, out)
	}
	code, out := do(t, mux, "owner", "PUT", "/api/agents/bindings/implementer",
		`{"primary":{"agent":"codex","model":"gpt-5"},"fallback":[{"agent":"claude","model":""}]}`)
	if code != http.StatusNoContent {
		t.Fatalf("binding = %d %v", code, out)
	}
	if code, out := do(t, mux, "owner", "PUT", "/api/agents/bindings/implementer",
		`{"primary":{"agent":"claude","model":"--x"}}`); code != http.StatusUnprocessableEntity || out["field"] != "primary.model" {
		t.Fatalf("bad model = %d %v", code, out)
	}
	code, out = do(t, mux, "owner", "POST", "/api/agents/switches/claude/emergency-stop", "")
	if code != http.StatusOK || out["stopped"] != float64(1) {
		t.Fatalf("emergency stop = %d %v", code, out)
	}
	code, out = do(t, mux, "owner", "GET", "/api/agents", "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	b, _ := json.Marshal(out)
	for _, want := range []string{`{"agent":"claude","enabled":false,"set":true`, `"source":"global"`,
		`"primary":{"agent":"codex","model":"gpt-5"}`, `"strategy":"different-from-author"`, `"cooldowns":[]`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("settings %s lack %s", b, want)
		}
	}
	if code, _ := do(t, mux, "owner", "DELETE", "/api/agents/bindings/implementer", ""); code != http.StatusNoContent {
		t.Fatalf("reset = %d", code)
	}
}

func TestProjectAgentsAPI(t *testing.T) {
	t.Parallel()
	mux := server(t)
	if code, _ := do(t, mux, "dev", "GET", "/api/projects/p1/agents", ""); code != http.StatusForbidden {
		t.Fatalf("developer GET = %d", code)
	}
	if code, _ := do(t, mux, "admin", "GET", "/api/projects/p9/agents", ""); code != http.StatusNotFound {
		t.Fatalf("unknown project = %d", code)
	}
	do(t, mux, "owner", "PUT", "/api/agents/switches/codex", `{"enabled":false}`)
	code, out := do(t, mux, "admin", "PUT", "/api/projects/p1/agents/switches/codex", `{"enabled":true}`)
	if code != http.StatusConflict || out["error"] != "disabled_globally" {
		t.Fatalf("enable globally disabled = %d %v", code, out)
	}
	if code, _ := do(t, mux, "admin", "PUT", "/api/projects/p1/agents/switches/claude", `{"enabled":false}`); code != http.StatusNoContent {
		t.Fatalf("project switch = %d", code)
	}
	if code, _ := do(t, mux, "admin", "PUT", "/api/projects/p1/agents/bindings/reviewer",
		`{"primary":{"agent":"claude"},"fallback":[],"strategy":"different-from-author"}`); code != http.StatusNoContent {
		t.Fatalf("project binding = %d", code)
	}
	code, out = do(t, mux, "admin", "GET", "/api/projects/p1/agents", "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d", code)
	}
	b, _ := json.Marshal(out)
	for _, want := range []string{`{"agent":"claude","effective":false,"global":true,"project":{"agent":"claude","enabled":false`,
		`{"agent":"codex","effective":false,"global":false`, `"source":"project"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("project settings %s lack %s", b, want)
		}
	}
	if code, _ := do(t, mux, "admin", "DELETE", "/api/projects/p1/agents/bindings/reviewer", ""); code != http.StatusNoContent {
		t.Fatalf("reset = %d", code)
	}
}

// Review finding: a developer gets 403 whatever the body, before it is validated.
func TestProjectWritesAuthorizeBeforeDecoding(t *testing.T) {
	t.Parallel()
	mux := server(t)
	for _, c := range [][2]string{{"PUT", "/api/projects/p1/agents/switches/claude"}, {"PUT", "/api/projects/p1/agents/bindings/reviewer"}} {
		if code, _ := do(t, mux, "dev", c[0], c[1], "{not json"); code != 403 {
			t.Fatalf("%s %s = %d, want 403", c[0], c[1], code)
		}
	}
}
