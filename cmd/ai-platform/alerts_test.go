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
	"time"

	alertsdb "github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	alertsdomain "github.com/tr1gl1de/kindevai/internal/alerts/domain"
	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	identitydb "github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	identity "github.com/tr1gl1de/kindevai/internal/identity/domain"
	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
)

// fakeBotAPI records sendMessage calls; no real Telegram call is made.
type fakeBotAPI struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
}

// AC (KIN-40): events emitted before the Alert Center existed (dead for lack of a handler) are redelivered once
// it is wired; they become alerts and Telegram messages to the linked Owner with Mini App deep links.
func TestAlertCenterDrainsDeadEventsIntoAlertsAndTelegram(t *testing.T) {
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
		`INSERT INTO tasks(id, project_id, title, state, plan_revision, created_at, updated_at, version)
		 VALUES ('T1', 'p1', 'Billing', 'AWAITING_APPROVAL', 1, 'x', 'x', 1)`,
		"INSERT INTO task_plan_revisions(task_id, revision, plan_json, plan_hash, risk, created_at) VALUES ('T1', 1, '{}', '1111111111111111111111111111111111111111111111111111111111111111', 'L2', 'x')",
		`INSERT INTO approvals(id, project_id, task_id, task_title, plan_revision, plan_hash, scope, reason, risk, state, requested_at)
		 VALUES ('AP1', 'p1', 'T1', 'Billing', 1, '1111111111111111111111111111111111111111111111111111111111111111', 'execute', 'plan', 'L2', 'pending', '2026-11-25T10:00:00Z')`,
	} {
		if _, err := inst.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	tg := identitydb.NewTelegramRepository(inst.DB)
	if err := tg.CreateIdentity(ctx, identity.TelegramIdentity{UserID: "u1", TelegramID: 4242, LinkedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := tg.SaveSettings(ctx, identity.BotSettings{BotUsername: "kin_bot", AppShortName: "app"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	vault := botTokenVault{&integrationsapp.Vault{Store: integrationsdb.New(inst.DB), Cipher: inst.Box, Now: time.Now}}
	if err := vault.SetBotToken(ctx, "123456:AAH-test-token", "u1"); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.DiscardHandler)
	msg := newMessaging(inst, logger)
	for _, m := range []messaging.Message{
		{Adapter: runnersapp.AdapterAlerts, Topic: runnersapp.TopicRunnerUnreachable, Key: "runner/r1/unreachable/1",
			Payload: []byte(`{"runnerId":"r1","since":"2026-11-25T10:00:00Z","missedHeartbeats":3}`)},
		{Adapter: approvalsapp.AdapterTelegram, Topic: approvalsapp.TopicApprovalRequested, Key: "approval/AP1/requested",
			Payload: []byte(`{"approvalId":"AP1","projectId":"p1","taskId":"T1","planRevision":1,"scope":"execute","reason":"plan","risk":"L2"}`)},
	} {
		if err := msg.Outbox.Enqueue(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := msg.Outbox.Poll(ctx); err != nil { // no handlers yet: both go dead
		t.Fatal(err)
	}
	if dead, _ := msg.Outbox.Dead(ctx, 10); len(dead) != 2 {
		t.Fatalf("dead before wiring = %d", len(dead))
	}

	api := &fakeBotAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	newAlerts(inst, msg, newAudit(inst, logger), identitydb.NewUserRepository(inst.DB), srv.URL, logger)
	if n, err := msg.Outbox.RetryUnhandled(ctx); err != nil || n != 2 {
		t.Fatalf("requeued %d, %v", n, err)
	}
	for range 10 {
		if n, err := msg.Outbox.Poll(ctx); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			break
		}
	}
	if dead, _ := msg.Outbox.Dead(ctx, 10); len(dead) != 0 {
		t.Fatalf("still dead: %+v", dead)
	}
	a, err := alertsdb.New(inst.DB).Active(ctx, "runner/r1/unreachable")
	if err != nil || a.Kind != alertsdomain.KindRunnerUnreachable || a.ProjectID != "" {
		t.Fatalf("alert = %+v, %v", a, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.bodies) != 2 {
		t.Fatalf("telegram calls = %d", len(api.bodies))
	}
	all := strings.Join(api.bodies, "\n")
	for _, want := range []string{`"chat_id":4242`, "startapp=approval_AP1", "startapp=alert_" + a.ID, "https://t.me/kin_bot/app"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in %s", want, all)
		}
	}
	for _, b := range api.bodies {
		var m map[string]any
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b, "AAH-test-token") {
			t.Fatal("token in a message body")
		}
	}
}
