package httpapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// fakeYouTrack serves issues like the YouTrack REST API (never a real instance). The trigger tag's history says the
// reporter applied it, unless taggers names somebody else (KIN-50).
type fakeYouTrack struct {
	mu      sync.Mutex
	issues  map[string]string // id -> JSON
	taggers map[string]string // id -> login that applied the tag
	down    bool
	reads   int
}

func (f *fakeYouTrack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/issues/"); ok {
		if id, ok := strings.CutSuffix(id, "/activities"); ok {
			var is struct {
				Updated  int64 `json:"updated"`
				Reporter struct {
					Login string `json:"login"`
				} `json:"reporter"`
			}
			_ = json.Unmarshal([]byte(f.issues[id]), &is)
			login := cmp.Or(f.taggers[id], is.Reporter.Login)
			_, _ = fmt.Fprintf(w, `[{"timestamp":%d,"author":{"login":%q},"added":[{"name":"kindevai"}],"removed":[]}]`, is.Updated, login)
			return
		}
		j, found := f.issues[id]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(j))
		return
	}
	var all []string
	if r.URL.Query().Get("$skip") == "0" {
		for _, j := range f.issues {
			if strings.Contains(j, `"kindevai"`) {
				all = append(all, j)
			}
		}
	}
	_, _ = w.Write([]byte("[" + strings.Join(all, ",") + "]"))
}

func ytIssue(id string, updated int64, reporter string, tags ...string) string {
	quoted := make([]string, len(tags))
	for i, t := range tags {
		quoted[i] = fmt.Sprintf(`{"name":%q}`, t)
	}
	return fmt.Sprintf(`{"idReadable":%q,"summary":"Summary %s","updated":%d,"resolved":null,"reporter":{"login":%q},`+
		`"tags":[%s],"project":{"shortName":"KIN"}}`, id, id, updated, reporter, strings.Join(quoted, ","))
}

type memTasks struct {
	mu      sync.Mutex
	issues  map[string]string
	planned int // AutoStartPlanning calls
}

func (m *memTasks) TaskForIssue(_ context.Context, p, i string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.issues[p+"/"+i]
	return id, ok, nil
}

func (m *memTasks) CreateIssueTask(_ context.Context, p, i, _, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("task-%s", i)
	m.issues[p+"/"+i] = id
	return id, nil
}

func (m *memTasks) RetitleTask(context.Context, string, string) (bool, error) { return false, nil }

func (m *memTasks) AutoStartPlanning(context.Context, string, string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.planned++
	return app.AutoPlanStarted, nil
}

// owner1 may start AI in p1; bob1 may not.
type memAccess struct{}

func (memAccess) CanRunAI(_ context.Context, projectID, userID string) (bool, error) {
	return projectID == "p1" && userID == "owner1", nil
}

func (memAccess) CanManageAI(_ context.Context, projectID, userID string) (bool, error) {
	return projectID == "p1" && userID == "owner1", nil
}

type memUsers struct{}

func (memUsers) ByLogin(_ context.Context, login string) (app.UserRef, error) {
	switch login {
	case "owner":
		return app.UserRef{ID: "owner1", Login: "owner"}, nil
	case "bob":
		return app.UserRef{ID: "bob1", Login: "bob"}, nil
	}
	return app.UserRef{}, domain.ErrNotFound
}

func (memUsers) ByIDs(_ context.Context, ids []string) (map[string]app.UserRef, error) {
	out := map[string]app.UserRef{}
	for _, id := range ids {
		out[id] = app.UserRef{ID: id, Login: strings.TrimSuffix(id, "1")}
	}
	return out, nil
}

type intakeEnv struct {
	*env
	yt     *fakeYouTrack
	intake *app.Intake
	inbox  *messaging.Inbox
	tasks  *memTasks
}

func newIntakeEnv(t *testing.T) *intakeEnv {
	t.Helper()
	e := &intakeEnv{env: newEnv(t), yt: &fakeYouTrack{issues: map[string]string{}}, tasks: &memTasks{issues: map[string]string{}}}
	srv := httptest.NewServer(e.yt)
	t.Cleanup(srv.Close)
	e.inbox = messaging.NewInbox(e.db, messaging.Config{})
	e.intake = &app.Intake{Integrations: e.svc, Store: sqlite.New(e.db), Tasks: e.tasks, Access: memAccess{}, Users: memUsers{},
		Source: &youtrack.Client{HTTP: srv.Client()}, Inbox: e.inbox, Health: e.outbox.Health()}
	e.inbox.Handle(app.TopicYouTrackIssue, e.intake.HandleIssue)
	e.inbox.Handle(app.TopicYouTrackWebhook, e.intake.HandleWebhook)
	e.outbox.Handle(app.TopicYouTrackFetch, e.intake.HandleFetch)
	ih := &IntakeHandler{Intake: e.intake}
	mux := http.NewServeMux()
	ih.Register(mux)
	inner := e.h
	e.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			// The intake routes need the same test principal as the base handler.
			if p, ok := principals[r.Header.Get("X-Test-User")]; ok {
				r = r.WithContext(auth.WithPrincipal(r.Context(), p))
			}
			mux.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
	// The fake listens on plain-HTTP loopback, which the use case refuses for real connections: store it directly.
	c := domain.NewYouTrackConnection(srv.URL, time.Now())
	if err := sqlite.New(e.db).SaveYouTrack(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	tok, _ := domain.NewYouTrackToken(token)
	if err := e.svc.Vault.Put(t.Context(), domain.SecretYouTrackToken, tok, "owner1"); err != nil {
		t.Fatal(err)
	}
	if w := e.do(t, "owner", "PUT", "/api/projects/p1/integrations/youtrack", ShortNameRequest{ShortName: "KIN"}); w.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", w.Code, w.Body)
	}
	return e
}

// process runs the inbox and outbox until both are idle.
func (e *intakeEnv) process(t *testing.T) {
	t.Helper()
	for range 20 {
		n1, err := e.inbox.Poll(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		n2, err := e.outbox.Poll(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n1+n2 == 0 {
			return
		}
	}
	t.Fatal("messages keep coming")
}

func (e *intakeEnv) view(t *testing.T, user string) ProjectYouTrack {
	t.Helper()
	w := e.do(t, user, "GET", "/api/projects/p1/youtrack", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("view: %d %s", w.Code, w.Body)
	}
	return decode[ProjectYouTrack](t, w)
}

// AC: polling alone (no webhook configured) turns tagged issues into tasks, with reasons for rejected ones.
func TestPollingWorksEndToEndWithoutAWebhook(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	if w := e.do(t, "owner", "POST", "/api/integrations/youtrack/users", UserLinkRequest{Login: "owner", YouTrackLogin: "boss"}); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	e.yt.issues["KIN-1"] = ytIssue("KIN-1", 100, "boss", "kindevai")
	e.yt.issues["KIN-2"] = ytIssue("KIN-2", 200, "stranger", "kindevai")
	e.yt.issues["KIN-3"] = ytIssue("KIN-3", 300, "boss", "other")
	e.yt.issues["KIN-4"] = ytIssue("KIN-4", 400, "boss", "kindevai") // the Owner's issue, tagged by an outsider (KIN-50)
	e.yt.taggers = map[string]string{"KIN-4": "stranger"}
	if err := e.intake.PollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.process(t)
	if len(e.tasks.issues) != 1 || e.tasks.issues["p1/KIN-1"] == "" {
		t.Fatalf("tasks %+v", e.tasks.issues)
	}
	v := e.view(t, "owner")
	if !v.Connected || v.Binding == nil || v.Binding.TriggerTag != "kindevai" || v.Status != "ok" || v.LastPolledAt == nil || len(v.Recent) != 3 {
		t.Fatalf("view %+v", v)
	}
	reasons := map[string]string{}
	for _, r := range v.Recent {
		reasons[r.IssueID] = r.Outcome + "/" + r.Reason
	}
	if reasons["KIN-1"] != "created/" || reasons["KIN-2"] != "rejected/tag_not_allowed" ||
		reasons["KIN-4"] != "rejected/tag_not_allowed" {
		t.Fatalf("recent %+v", v.Recent)
	}
	if w := e.do(t, "bob", "GET", "/api/projects/p1/youtrack", nil); w.Code != http.StatusNotFound {
		t.Fatalf("non-member: %d", w.Code)
	}
}

// AC: the webhook verifies the secret, answers 2xx after storing, and the issue is re-read through REST.
func TestWebhookEndToEnd(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	send := func(secret, body string) int {
		req := httptest.NewRequestWithContext(t.Context(), "POST", WebhookPath, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set(WebhookSecretHeader, secret)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, req)
		return w.Code
	}
	body := `{"issueId":"KIN-7","updated":700}`
	if c := send("x", body); c != http.StatusNotFound {
		t.Fatalf("disabled webhook: %d", c)
	}
	secret := strings.Repeat("w", 40)
	if w := e.do(t, "bob", "PUT", "/api/integrations/youtrack/webhook", WebhookSecretRequest{Secret: secret}); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", w.Code)
	}
	w := e.do(t, "owner", "PUT", "/api/integrations/youtrack/webhook", WebhookSecretRequest{Secret: secret})
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), secret) || !decode[IntakeSettings](t, w).WebhookSecret.Set {
		t.Fatalf("set secret: %d %s", w.Code, w.Body)
	}
	if c := send("wrong", body); c != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d", c)
	}
	if c := send(secret, `{"issueId":"nope"}`); c != http.StatusUnprocessableEntity {
		t.Fatalf("bad body: %d", c)
	}
	if _, err := e.svc.Vault.Credential(t.Context(), domain.SecretYouTrackWebhookSecret); err != nil {
		t.Fatal(err)
	}
	if w := e.do(t, "owner", "POST", "/api/integrations/youtrack/users", UserLinkRequest{Login: "owner", YouTrackLogin: "boss"}); w.Code != http.StatusOK {
		t.Fatalf("link: %d", w.Code)
	}
	e.yt.issues["KIN-7"] = ytIssue("KIN-7", 700, "boss", "kindevai")
	if c := send(secret, body); c != http.StatusAccepted {
		t.Fatalf("webhook: %d", c)
	}
	if c := send(secret, body); c != http.StatusAccepted { // redelivery: acknowledged, deduplicated
		t.Fatalf("redelivery: %d", c)
	}
	e.process(t)
	if e.tasks.issues["p1/KIN-7"] == "" {
		t.Fatalf("tasks %+v", e.tasks.issues)
	}
	if e.yt.reads != 2 { // the issue and its tag history, once
		t.Fatalf("YouTrack read %d times", e.yt.reads)
	}
}

// AC: "run by issue ID" for members; YouTrack being down neither blocks the call nor the view, and shows in it.
func TestRunByIDAndOutageEndToEnd(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	if w := e.do(t, "bob", "POST", "/api/projects/p1/youtrack/runs", RunRequest{IssueID: "KIN-9"}); w.Code != http.StatusNotFound {
		t.Fatalf("non-member: %d", w.Code)
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/youtrack/runs", RunRequest{IssueID: "X-9"}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("other project: %d %s", w.Code, w.Body)
	}
	e.yt.issues["KIN-9"] = ytIssue("KIN-9", 900, "whoever")
	w := e.do(t, "owner", "POST", "/api/projects/p1/youtrack/runs", RunRequest{IssueID: "KIN-9"})
	if w.Code != http.StatusAccepted || decode[IntakeEntry](t, w).Outcome != "queued" {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	e.process(t)
	if e.tasks.issues["p1/KIN-9"] == "" {
		t.Fatalf("tasks %+v", e.tasks.issues)
	}

	e.yt.mu.Lock()
	e.yt.down = true
	e.yt.mu.Unlock()
	if err := e.intake.PollOnce(t.Context()); err == nil {
		t.Fatal("outage not reported")
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/youtrack/runs", RunRequest{IssueID: "KIN-10"}); w.Code != http.StatusAccepted {
		t.Fatalf("run during outage: %d", w.Code)
	}
	if v := e.view(t, "owner"); v.Status != "degraded" {
		t.Fatalf("status %+v", v)
	}
	w = e.do(t, "owner", "GET", "/api/integrations/youtrack/intake", nil)
	if s := decode[IntakeSettings](t, w); s.Health.State != "degraded" || s.Health.LastError == "" || s.PollIntervalSeconds != 60 || s.WebhookPath != WebhookPath {
		t.Fatalf("owner settings %+v", s)
	}
}

func TestIntakeSettingsAPI(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	if w := e.do(t, "bob", "GET", "/api/integrations/youtrack/intake", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", w.Code)
	}
	if w := e.do(t, "owner", "PUT", "/api/integrations/youtrack/intake", PollIntervalRequest{PollIntervalSeconds: 5}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too short: %d", w.Code)
	}
	if w := e.do(t, "owner", "PUT", "/api/integrations/youtrack/intake", PollIntervalRequest{PollIntervalSeconds: 300}); w.Code != http.StatusOK ||
		decode[IntakeSettings](t, w).PollIntervalSeconds != 300 {
		t.Fatalf("interval: %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "POST", "/api/integrations/youtrack/users", UserLinkRequest{Login: "bob", YouTrackLogin: "bob.yt"}); w.Code != http.StatusOK {
		t.Fatalf("link bob: %d", w.Code)
	}
	if w := e.do(t, "owner", "POST", "/api/integrations/youtrack/users", UserLinkRequest{Login: "owner", YouTrackLogin: "BOB.YT"}); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "youtrack_login_taken") {
		t.Fatalf("taken: %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "DELETE", "/api/integrations/youtrack/users/bob1", nil); w.Code != http.StatusNoContent {
		t.Fatalf("unlink: %d", w.Code)
	}
	w := e.do(t, "owner", "PUT", "/api/projects/p1/integrations/youtrack/trigger", TriggerTagRequest{TriggerTag: "ai run"})
	if w.Code != http.StatusOK || decode[Binding](t, w).TriggerTag != "ai run" {
		t.Fatalf("trigger: %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "PUT", "/api/projects/p1/integrations/youtrack/trigger", TriggerTagRequest{TriggerTag: "a}b"}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad tag: %d", w.Code)
	}
	if w := e.do(t, "owner", "DELETE", "/api/integrations/youtrack/webhook", nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete webhook: %d", w.Code)
	}
}

// KIN-85: PUT /api/projects/{id}/integrations/youtrack/autoplan switches automatic planning; the project view shows it.
func TestAutoPlanAPI(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	path := "/api/projects/p1/integrations/youtrack/autoplan"
	if w := e.do(t, "bob", "PUT", path, AutoPlanRequest{AutoPlan: true}); w.Code != http.StatusNotFound {
		t.Fatalf("outsider: %d", w.Code)
	}
	if w := e.do(t, "owner", "PUT", "/api/projects/p2/integrations/youtrack/autoplan", AutoPlanRequest{AutoPlan: true}); w.Code != http.StatusConflict {
		t.Fatalf("unbound: %d", w.Code)
	}
	w := e.do(t, "owner", "PUT", path, AutoPlanRequest{AutoPlan: true})
	if w.Code != http.StatusOK || !decode[Binding](t, w).AutoPlan {
		t.Fatalf("on: %d %s", w.Code, w.Body)
	}
	w = e.do(t, "owner", "GET", "/api/projects/p1/youtrack", nil)
	if v := decode[ProjectYouTrack](t, w); v.Binding == nil || !v.Binding.AutoPlan {
		t.Fatalf("view: %s", w.Body)
	}
}
