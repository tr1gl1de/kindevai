package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/httpapi"
	alertsdb "github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type projects struct{}

func (projects) Visible(_ context.Context, p auth.Principal) ([]app.ProjectRef, error) {
	switch p.UserID {
	case "owner":
		return []app.ProjectRef{{ID: "p1", Name: "Alpha", Role: "owner"}}, nil
	case "member":
		return []app.ProjectRef{{ID: "p1", Name: "Alpha", Role: "member"}}, nil
	}
	return nil, nil
}
func (projects) Name(context.Context, string) (string, error)      { return "Alpha", nil }
func (projects) Members(context.Context, string) ([]string, error) { return nil, nil }
func (projects) Admins(context.Context, string) ([]string, error)  { return nil, nil }

type approvals struct{}

func (approvals) Pending(context.Context, string) (bool, error) { return true, nil }

type people struct{}

func (people) Owners(context.Context) ([]string, error) { return []string{"owner"}, nil }
func (people) TelegramChat(_ context.Context, u string) (int64, bool, error) {
	return 7, u == "owner", nil
}

var principals = map[string]auth.Principal{
	"owner":  {UserID: "owner", Login: "owner", Owner: true},
	"member": {UserID: "member", Login: "member"},
}

func setup(t *testing.T) (http.Handler, *app.Service) {
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
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner', 'owner', 'h', 'owner', 'x'), ('member', 'member', 'h', 'user', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	svc := &app.Service{Repo: alertsdb.New(d), Projects: projects{}, People: people{}, Approvals: approvals{}, Tx: db.Transactor{DB: d},
		Audit:  &auditapp.Service{Repo: auditdb.New(d), Now: time.Now},
		Outbox: messaging.NewOutbox(d, messaging.Config{}), Now: time.Now,
		NewID: func() string { n++; return "A" + string(rune('0'+n)) }}
	mux := http.NewServeMux()
	(&httpapi.Handler{Service: svc}).Register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(w, r)
	}), svc
}

func do(t *testing.T, h http.Handler, user, method, path string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Test-User", user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func TestAlertsAPI(t *testing.T) {
	t.Parallel()
	h, svc := setup(t)
	ctx := t.Context()
	if err := svc.Raise(ctx, "e1", domain.Occurrence{Kind: domain.KindRunnerUnreachable, Severity: domain.SeverityWarning,
		DedupKey: "runner/r1/unreachable", Subject: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Raise(ctx, "e2", domain.Occurrence{ProjectID: "p1", Kind: "ci_failed", Severity: domain.SeverityCritical,
		DedupKey: "ci/p1", Subject: "dev"}); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, h, "owner", "GET", "/api/alerts", nil)
	var list []httpapi.AlertView
	if err := json.Unmarshal(body, &list); code != 200 || err != nil || len(list) != 2 || !list[0].CanManage {
		t.Fatalf("owner list %d %s", code, body)
	}
	code, body = do(t, h, "member", "GET", "/api/alerts", nil)
	if err := json.Unmarshal(body, &list); code != 200 || err != nil || len(list) != 1 || list[0].ProjectName != "Alpha" || list[0].CanManage {
		t.Fatalf("member list %d %s", code, body)
	}
	if code, _ := do(t, h, "member", "POST", "/api/alerts/A2/acknowledge", nil); code != http.StatusForbidden {
		t.Fatalf("member ack = %d", code)
	}
	if code, _ := do(t, h, "member", "POST", "/api/alerts/A1/acknowledge", nil); code != http.StatusNotFound {
		t.Fatalf("member on platform alert = %d", code)
	}
	if code, _ := do(t, h, "owner", "POST", "/api/alerts/A2/explode", nil); code != http.StatusNotFound {
		t.Fatalf("unknown action = %d", code)
	}
	if code, body := do(t, h, "owner", "POST", "/api/alerts/A2/resolve", nil); code != 200 {
		t.Fatalf("resolve = %d %s", code, body)
	}
	if code, _ := do(t, h, "owner", "POST", "/api/alerts/A2/dismiss", nil); code != http.StatusConflict {
		t.Fatalf("dismiss resolved = %d", code)
	}
	if code, body = do(t, h, "owner", "GET", "/api/alerts?state=all", nil); code != 200 || json.Unmarshal(body, &list) != nil || len(list) != 2 {
		t.Fatalf("all = %d %s", code, body)
	}
	if code, _ := do(t, h, "", "GET", "/api/alerts", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", code)
	}
}

func TestPreferencesAPI(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	code, body := do(t, h, "owner", "GET", "/api/notifications/preferences", nil)
	var p httpapi.PreferencesView
	if err := json.Unmarshal(body, &p); code != 200 || err != nil || p.Language != "en" || len(p.Disabled) != 0 ||
		!p.TelegramLinked || len(p.Types) != len(domain.NotificationTypes) {
		t.Fatalf("get = %d %s", code, body)
	}
	code, body = do(t, h, "owner", "PUT", "/api/notifications/preferences", map[string]any{"language": "ru", "disabled": []string{"alert_raised"}})
	if err := json.Unmarshal(body, &p); code != 200 || err != nil || p.Language != "ru" || len(p.Disabled) != 1 {
		t.Fatalf("put = %d %s", code, body)
	}
	if code, _ := do(t, h, "owner", "PUT", "/api/notifications/preferences", map[string]any{"language": "ru", "disabled": []string{"spam"}}); code != 422 {
		t.Fatalf("bad type = %d", code)
	}
}
