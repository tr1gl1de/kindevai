package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	alertshttp "github.com/tr1gl1de/kindevai/internal/alerts/adapters/httpapi"
	alertsdomain "github.com/tr1gl1de/kindevai/internal/alerts/domain"
	identitydb "github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
)

// fakeYouTrack answers the project lookup and issue creation; no real YouTrack call is made.
type fakeYouTrack struct {
	mu      sync.Mutex
	created []string
}

func (f *fakeYouTrack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/admin/projects":
		_, _ = w.Write([]byte(`[{"id":"0-5","shortName":"SHOP"}]`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/issues": // marker search: nothing yet
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.created = append(f.created, string(b))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"idReadable":"SHOP-12"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// KIN-61 AC: a confirmed suggestion goes through the outbox to YouTrack once, and the alert stores the issue.
func TestAnalysisTaskIsCreatedInYouTrackThroughTheOutbox(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	inst, err := bootstrap.Run(ctx, bootstrap.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', 'h', 'owner', '2026-11-25T10:00:00Z')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', '2026-11-25T10:00:00Z')",
	} {
		if _, err := inst.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	yt := &fakeYouTrack{}
	srv := httptest.NewServer(yt)
	t.Cleanup(srv.Close)
	logger := slog.New(slog.DiscardHandler)
	msg := newMessaging(inst, logger)
	audit := newAudit(inst, logger)
	integ := newIntegrations(inst, msg, audit, "", logger)
	owner := auth.Principal{UserID: "u1", Login: "owner", Owner: true}
	if _, err := integ.Service.ConfigureYouTrack(ctx, owner, srv.URL, "perm:test-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := integ.Service.BindYouTrackProject(ctx, owner, "p1", "SHOP"); err != nil {
		t.Fatal(err)
	}
	svc := newAlerts(inst, msg, audit, identitydb.NewUserRepository(inst.DB), "", logger).(*alertshttp.Handler).Service
	if err := svc.Raise(ctx, "e1", alertsdomain.Occurrence{ProjectID: "p1", Kind: alertsdomain.KindCIFailed,
		Severity: alertsdomain.SeverityWarning, DedupKey: "ci/acme/app/branch/dev", Subject: "acme/app dev",
		Details: map[string]string{"conclusion": "failure"}}); err != nil {
		t.Fatal(err)
	}
	vs, err := svc.List(ctx, owner, true)
	if err != nil || len(vs) != 1 {
		t.Fatalf("alerts = %+v, %v", vs, err)
	}
	id := vs[0].Alert.ID
	for range 2 { // a double confirmation is one issue
		if _, err := svc.SuggestAnalysis(ctx, owner, id, "", alertsdomain.LangEN); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		if n, err := msg.Outbox.Poll(ctx); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			break
		}
	}
	if dead, _ := msg.Outbox.Dead(ctx, 10); len(dead) != 0 {
		t.Fatalf("dead: %+v", dead)
	}
	yt.mu.Lock()
	created := append([]string(nil), yt.created...)
	yt.mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("issues created = %d", len(created))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(created[0]), &body); err != nil ||
		!strings.HasPrefix(body["summary"].(string), "Analyze alert: CI failed on acme/app dev") ||
		!strings.Contains(body["description"].(string), alertsdomain.AnalysisMarker(id)) || strings.Contains(created[0], "test-token") {
		t.Fatalf("issue = %s", created[0])
	}
	vs, err = svc.List(ctx, owner, true)
	if err != nil || vs[0].Analysis == nil || vs[0].Analysis.IssueID != "SHOP-12" || vs[0].Analysis.IssueURL != srv.URL+"/issue/SHOP-12" {
		t.Fatalf("analysis = %+v, %v", vs[0].Analysis, err)
	}
}
