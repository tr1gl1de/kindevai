package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging/httpapi"
)

type fixture struct {
	h      http.Handler
	outbox *messaging.Outbox
	inbox  *messaging.Inbox
}

func setup(t *testing.T) fixture {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	cfg := messaging.Config{BreakerThreshold: 1}
	f := fixture{outbox: messaging.NewOutbox(d, cfg), inbox: messaging.NewInbox(d, cfg)}
	mux := http.NewServeMux()
	(&httpapi.Handler{Outbox: f.outbox, Inbox: f.inbox}).Register(mux)
	users := map[string]auth.Principal{"owner": {UserID: "u1", Login: "owner", Owner: true}, "alice": {UserID: "u2", Login: "alice"}}
	f.h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := users[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(w, r)
	})
	return f
}

func do(t *testing.T, h http.Handler, user, method, target string, out any) int {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	r.Header.Set("X-Test-User", user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if out != nil && w.Code < 300 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, target, err, w.Body)
		}
	}
	return w.Code
}

func TestOnlyOwnerSeesMessaging(t *testing.T) {
	t.Parallel()
	f := setup(t)
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/integrations/status"},
		{http.MethodGet, "/api/messaging/outbox/dead"},
		{http.MethodPost, "/api/messaging/inbox/dead/1/retry"},
	} {
		if code := do(t, f.h, "", tc.method, tc.target, nil); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", tc.method, tc.target, code)
		}
		if code := do(t, f.h, "alice", tc.method, tc.target, nil); code != http.StatusForbidden {
			t.Errorf("non-owner %s %s = %d, want 403", tc.method, tc.target, code)
		}
	}
}

// AC: integration state per adapter and dead messages are visible through the API, and can be retried.
func TestStatusDeadAndRetry(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ctx := t.Context()
	f.outbox.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error {
		return messaging.Permanent(errors.New("404 issue not found"))
	})
	f.outbox.Handle("github.status", func(context.Context, messaging.Delivery) error { return errors.New("502 bad gateway") })
	f.outbox.Handle("telegram.send", func(context.Context, messaging.Delivery) error { return nil })
	for _, m := range []messaging.Message{
		{Adapter: "youtrack", Topic: "youtrack.comment", Key: "c-1", Payload: []byte(`{"issue":"KIN-1"}`)},
		{Adapter: "github", Topic: "github.status", Key: "s-1", Payload: []byte(`{}`)},
	} {
		if err := f.outbox.Enqueue(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.outbox.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inbox.Receive(ctx, messaging.InboundEvent{Source: "github", DeliveryID: "d-1", Topic: "github.push", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}

	var status httpapi.StatusView
	if code := do(t, f.h, "owner", http.MethodGet, "/api/integrations/status", &status); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	byName := map[string]httpapi.AdapterView{}
	for _, a := range status.Adapters {
		byName[a.Adapter] = a
	}
	if gh := byName["github"]; gh.State != "circuit_open" || gh.LastError != "502 bad gateway" || gh.OpenUntil == nil || gh.Pending != 1 {
		t.Errorf("github = %+v, want circuit_open with one pending message", gh)
	}
	if yt := byName["youtrack"]; yt.State != "ok" || yt.Dead != 1 {
		t.Errorf("youtrack = %+v, want ok with one dead message", yt)
	}
	if tg := byName["telegram"]; tg.State != "ok" {
		t.Errorf("registered telegram adapter = %+v, want listed as ok", tg)
	}
	if len(status.Inbox) != 1 || status.Inbox[0].Source != "github" || status.Inbox[0].Pending != 1 {
		t.Errorf("inbox = %+v", status.Inbox)
	}

	var dead []httpapi.DeadView
	if code := do(t, f.h, "owner", http.MethodGet, "/api/messaging/outbox/dead?limit=10", &dead); code != http.StatusOK {
		t.Fatalf("dead = %d", code)
	}
	if len(dead) != 1 || dead[0].Endpoint != "youtrack" || dead[0].Key != "c-1" || dead[0].LastError != "404 issue not found" {
		t.Fatalf("dead = %+v", dead)
	}
	if code := do(t, f.h, "owner", http.MethodGet, "/api/messaging/inbox/dead", &dead); code != http.StatusOK || len(dead) != 0 {
		t.Fatalf("inbox dead = %d %+v, want an empty list", code, dead)
	}
	retry := fmt.Sprintf("/api/messaging/outbox/dead/%d/retry", dead0(t, f))
	if code := do(t, f.h, "owner", http.MethodPost, retry, nil); code != http.StatusNoContent {
		t.Fatalf("retry = %d", code)
	}
	if code := do(t, f.h, "owner", http.MethodPost, retry, nil); code != http.StatusNotFound {
		t.Fatalf("second retry = %d, want 404 (no longer dead)", code)
	}
	for _, bad := range []string{"/api/messaging/outbox/dead/x/retry", "/api/messaging/nope/dead/1/retry"} {
		if code := do(t, f.h, "owner", http.MethodPost, bad, nil); code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", bad, code)
		}
	}
	for _, bad := range []string{"/api/messaging/outbox/dead?limit=0", "/api/messaging/outbox/dead?limit=abc", "/api/messaging/outbox/dead?limit=1000"} {
		if code := do(t, f.h, "owner", http.MethodGet, bad, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, code)
		}
	}
}

func dead0(t *testing.T, f fixture) int64 {
	t.Helper()
	ds, err := f.outbox.Dead(t.Context(), 1)
	if err != nil || len(ds) != 1 {
		t.Fatalf("dead = %v, %v", ds, err)
	}
	return ds[0].ID
}

// Payloads never appear in API responses.
func TestDeadViewHasNoPayload(t *testing.T) {
	t.Parallel()
	f := setup(t)
	if err := f.outbox.Enqueue(t.Context(), messaging.Message{Adapter: "github", Topic: "github.none", Payload: []byte(`{"body":"private text"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.outbox.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/messaging/outbox/dead", nil)
	r.Header.Set("X-Test-User", "owner")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "private text") {
		t.Fatalf("dead view = %d %s", w.Code, w.Body)
	}
}
