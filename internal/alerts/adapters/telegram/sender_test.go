package telegram_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/telegram"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

const token = "123456:AAH-secret-token"

type bot struct {
	token string
	err   error
}

func (b bot) Token(context.Context) (string, error) { return b.token, b.err }

func (b bot) MiniAppLink(_ context.Context, sp string) (string, bool, error) {
	return "https://t.me/kin_bot/app?startapp=" + sp, true, nil
}

// fakeAPI is a Bot API stand-in recording sendMessage bodies and answering with the queued statuses.
type fakeAPI struct {
	mu      sync.Mutex
	paths   []string
	bodies  []map[string]any
	answers []int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.paths = append(f.paths, r.URL.Path)
	f.bodies = append(f.bodies, m)
	status := http.StatusOK
	if len(f.answers) > 0 {
		status, f.answers = f.answers[0], f.answers[1:]
	}
	w.WriteHeader(status)
	switch status {
	case http.StatusOK:
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	case http.StatusTooManyRequests:
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`)
	default:
		_, _ = io.WriteString(w, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`)
	}
}

func newSender(t *testing.T, api *fakeAPI, b telegram.Bot) (*telegram.Sender, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	var waits []time.Duration
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	return &telegram.Sender{Bot: b, APIBase: srv.URL, Client: srv.Client(), Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}, &waits
}

func delivery(t *testing.T, p app.SendPayload) messaging.Delivery {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return messaging.Delivery{Topic: app.TopicTelegramSend, Key: "k", Payload: b, Attempt: 1}
}

// AC: delivery via sendMessage with a deep-link button into the Mini App (no in-chat decision action).
func TestDeliverSendsMessageWithMiniAppButton(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{}
	s, _ := newSender(t, api, bot{token: token})
	err := s.Deliver(t.Context(), delivery(t, app.SendPayload{ChatID: 42, Text: "Approval needed", Button: "Open approval",
		StartParam: "approval_AP1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(api.paths) != 1 || api.paths[0] != "/bot"+token+"/sendMessage" {
		t.Fatalf("paths = %q", api.paths)
	}
	b := api.bodies[0]
	if b["chat_id"].(float64) != 42 || b["text"] != "Approval needed" {
		t.Fatalf("body = %v", b)
	}
	kb := b["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
	if kb["url"] != "https://t.me/kin_bot/app?startapp=approval_AP1" || kb["text"] != "Open approval" || kb["callback_data"] != nil {
		t.Fatalf("button = %v", kb)
	}
}

// AC: retry — 429/5xx are retried by the outbox, a blocked chat or a missing token is permanent; errors never
// contain the token.
func TestDeliverErrorClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		status    int
		bot       bot
		permanent bool
	}{
		{"rate limited", http.StatusTooManyRequests, bot{token: token}, false},
		{"server error", http.StatusBadGateway, bot{token: token}, false},
		{"blocked", http.StatusForbidden, bot{token: token}, true},
		{"token rejected", http.StatusUnauthorized, bot{token: token}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeAPI{answers: []int{c.status}}
			s, _ := newSender(t, api, c.bot)
			err := s.Deliver(t.Context(), delivery(t, app.SendPayload{ChatID: 1, Text: "x"}))
			if err == nil {
				t.Fatal("no error")
			}
			if got := messaging.IsPermanent(err); got != c.permanent {
				t.Fatalf("permanent = %v, want %v (%v)", got, c.permanent, err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("token leaked in %q", err)
			}
		})
	}
}

// Review: before the bot is configured, notifications are dropped rather than dead-lettered.
func TestDeliverSkipsWhenNotConfigured(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{}
	s, _ := newSender(t, api, bot{err: telegram.ErrNotConfigured})
	if err := s.Deliver(t.Context(), delivery(t, app.SendPayload{ChatID: 1, Text: "x"})); err != nil {
		t.Fatal(err)
	}
	if len(api.paths) != 0 {
		t.Fatalf("called the API without a token: %q", api.paths)
	}
}

// AC: rate limit — messages to one chat are spaced by PerChat, different chats by Global.
func TestDeliverPacesPerChatAndGlobally(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{}
	s, waits := newSender(t, api, bot{token: token})
	for _, chat := range []int64{1, 1, 2} {
		if err := s.Deliver(t.Context(), delivery(t, app.SendPayload{ChatID: chat, Text: "x"})); err != nil {
			t.Fatal(err)
		}
	}
	// The clock does not move: 2nd message to chat 1 waits 1s; chat 2 waits for the global slot after it.
	want := []time.Duration{telegram.DefaultPerChat, telegram.DefaultPerChat + telegram.DefaultGlobal}
	if len(*waits) != 2 || (*waits)[0] != want[0] || (*waits)[1] != want[1] {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
}
