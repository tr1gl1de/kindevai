package messaging_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// Typical compact projections of YouTrack and GitHub webhooks: prose about tokens, opaque node ids, SHAs, URLs.
var (
	youtrackPayload = `{"issue":"KIN-12","summary":"Token refresh fails for basic auth users",` +
		`"description":"Steps: log in, wait until the token expires, the Bearer of bad news is a 401.",` +
		`"url":"https://dbk.youtrack.cloud/issue/KIN-12","updated":1732460000000}`
	githubPayload = `{"action":"opened","number":23,"node_id":"PR_kwDOJx8Yrs5abcdEFGH12",` +
		`"title":"KIN-15: Add durable inbox/outbox","html_url":"https://github.com/tr1gl1de/kindevai/pull/23",` +
		`"head":{"ref":"feature/KIN-15-outbox-inbox","sha":"3688ffd1c1a2b3c4d5e6f708192a3b4c5d6e7f80"},` +
		`"user":{"login":"tr1gl1de","node_id":"MDQ6VXNlcjE="},"cursor":"MDExOlB1bGxSZXF1ZXN0MQ=="}`
)

// KIN-45 AC: secret-looking values (not only field names) are masked in inbox payloads before storage.
func TestInboxRedactsSecretValues(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	in := messaging.NewInbox(d, config(newClock()))
	e := event("v-1")
	e.Payload = []byte(`{"comment":{"body":"try Authorization: Bearer abcDEF123456 now"},` +
		`"links":["https://x.example/hook?page=2&access_token=t0k3nVal"],"note":"ghp_` + strings.Repeat("a1B", 12) + `",` +
		`"node_id":"PR_kwDOJx8Yrs5abcdEFGH12"}`)
	if _, err := in.Receive(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := d.QueryRowContext(t.Context(), "SELECT CAST(payload AS TEXT) FROM inbox_events").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"abcDEF123456", "t0k3nVal", "ghp_"} {
		if strings.Contains(stored, leak) {
			t.Errorf("stored payload leaks %q: %s", leak, stored)
		}
	}
	for _, keep := range []string{"try Authorization: Bearer", "page=2", "PR_kwDOJx8Yrs5abcdEFGH12"} {
		if !strings.Contains(stored, keep) {
			t.Errorf("stored payload lost %q: %s", keep, stored)
		}
	}
}

// KIN-45 AC: the outbox rejects payloads whose values look like secrets; the error names the field, not the value.
func TestOutboxRejectsSecretValues(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, config(newClock()))
	for name, p := range map[string]string{
		"bearer":      `{"headers":["Bearer abcDEF123456"]}`,
		"query token": `{"url":"https://h.example/x?access_token=zz9secret"}`,
		"github pat":  `{"note":"ghp_` + strings.Repeat("a1B", 12) + `"}`,
		"url creds":   `{"repo":"https://bot:hunter2@github.com/a/b.git"}`,
	} {
		err := o.Enqueue(t.Context(), messaging.Message{Adapter: "a", Topic: "t", Payload: []byte(p)})
		if !errors.Is(err, messaging.ErrInvalidMessage) {
			t.Errorf("%s: Enqueue = %v, want ErrInvalidMessage", name, err)
			continue
		}
		for _, leak := range []string{"abcDEF123456", "zz9secret", "a1Ba1B", "hunter2"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error leaks the value: %v", name, err)
			}
		}
	}
}

// KIN-45 AC: no false positives on typical YouTrack/GitHub payloads: accepted by the outbox, stored unchanged.
func TestTypicalPayloadsPassUnchanged(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, config(newClock()))
	in := messaging.NewInbox(d, config(newClock()))
	for i, p := range []string{youtrackPayload, githubPayload} {
		if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "a", Topic: "t", Payload: []byte(p)}); err != nil {
			t.Errorf("payload %d rejected by the outbox: %v", i, err)
		}
		e := event("typical-" + string(rune('a'+i)))
		e.Payload = []byte(p)
		if _, err := in.Receive(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := d.QueryRowContext(t.Context(), "SELECT CAST(payload AS TEXT) FROM inbox_events WHERE delivery_id = ?", e.DeliveryID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != p {
			t.Errorf("payload %d changed in the inbox:\n got %s\nwant %s", i, stored, p)
		}
	}
}

// KIN-45 AC: error texts are scrubbed before they reach last_error (outbox and inbox) and the adapter health.
func TestErrorTextsAreScrubbed(t *testing.T) {
	t.Parallel()
	leaky := errors.New(`Get "https://api.example.com/me?access_token=abc123secret": 401, sent Bearer zzTOP999xyz`)
	check := func(where, got string) {
		t.Helper()
		for _, leak := range []string{"abc123secret", "zzTOP999xyz"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s leaks %q: %s", where, leak, got)
			}
		}
		if !strings.Contains(got, "401") {
			t.Errorf("%s lost the error text: %s", where, got)
		}
	}

	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.MaxAttempts = 1
	h := messaging.NewHealth(cfg)
	h.Failure("github", leaky)
	check("health", h.Status("github").LastError)

	o := messaging.NewOutbox(d, cfg)
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error { return leaky })
	if err := o.Enqueue(t.Context(), msg("k")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	dead, err := o.Dead(t.Context(), 10)
	if err != nil || len(dead) != 1 {
		t.Fatalf("outbox dead = %+v, %v", dead, err)
	}
	check("outbox last_error", dead[0].LastError)

	in := messaging.NewInbox(d, cfg)
	in.Handle("github.push", func(context.Context, messaging.InboundEvent) error { return leaky })
	if _, err := in.Receive(t.Context(), event("e-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	idead, err := in.Dead(t.Context(), 10)
	if err != nil || len(idead) != 1 {
		t.Fatalf("inbox dead = %+v, %v", idead, err)
	}
	check("inbox last_error", idead[0].LastError)
}
