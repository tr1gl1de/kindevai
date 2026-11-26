package messaging_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

func event(id string) messaging.InboundEvent {
	return messaging.InboundEvent{Source: "github", DeliveryID: id, Topic: "github.push", Payload: []byte(`{"ref":"refs/heads/dev"}`)}
}

// AC: the inbox accepts an event durably before the caller acknowledges it: once Receive returns, the event
// survives a restart.
func TestInboxReceiveIsDurable(t *testing.T) {
	t.Parallel()
	d1, path := newDB(t)
	in := messaging.NewInbox(d1, config(newClock()))
	accepted, err := in.Receive(t.Context(), event("d-1"))
	if err != nil || !accepted {
		t.Fatalf("Receive = %v, %v", accepted, err)
	}
	_ = d1.Close()
	d2 := openDB(t, path)
	if n := count(t, d2, "SELECT count(*) FROM inbox_events WHERE source = 'github' AND delivery_id = 'd-1'"); n != 1 {
		t.Fatalf("after restart inbox has %d events, want 1", n)
	}
}

// AC: (source, delivery_id) is unique and a duplicate delivery is processed exactly once.
func TestInboxDuplicateProcessedOnce(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	in := messaging.NewInbox(d, config(newClock()))
	var calls atomic.Int32
	in.Handle("github.push", func(context.Context, messaging.InboundEvent) error { calls.Add(1); return nil })

	first, err := in.Receive(t.Context(), event("d-1"))
	if err != nil || !first {
		t.Fatalf("first Receive = %v, %v", first, err)
	}
	dup, err := in.Receive(t.Context(), event("d-1"))
	if err != nil || dup {
		t.Fatalf("duplicate Receive = %v, %v; want accepted=false without error (still acknowledged)", dup, err)
	}
	other := event("d-1")
	other.Source, other.Topic = "youtrack", "youtrack.issue"
	if ok, err := in.Receive(t.Context(), other); err != nil || !ok {
		t.Fatalf("same delivery id from another source = %v, %v; want accepted", ok, err)
	}
	for range 3 {
		if _, err := in.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	// A redelivery after processing is still recognised as a duplicate.
	if again, err := in.Receive(t.Context(), event("d-1")); err != nil || again {
		t.Fatalf("redelivery after processing = %v, %v", again, err)
	}
	if _, err := in.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("github handler calls = %d, want exactly 1", calls.Load())
	}
}

// The handler runs in the same transaction as marking the event processed: a failed attempt leaves no
// effects behind, so the event's effects are applied exactly once.
func TestInboxHandlerEffectsAreAtomic(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	if _, err := d.ExecContext(t.Context(), "CREATE TABLE effects (delivery_id TEXT)"); err != nil {
		t.Fatal(err)
	}
	c := newClock()
	in := messaging.NewInbox(d, config(c))
	attempts := 0
	in.Handle("github.push", func(ctx context.Context, e messaging.InboundEvent) error {
		attempts++
		if _, err := db.Conn(ctx, d).ExecContext(ctx, "INSERT INTO effects VALUES (?)", e.DeliveryID); err != nil {
			return err
		}
		if attempts == 1 {
			return errors.New("fails after writing")
		}
		return nil
	})
	if _, err := in.Receive(t.Context(), event("d-7")); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM effects"); n != 0 {
		t.Fatalf("failed attempt left %d effects", n)
	}
	c.Advance(time.Hour)
	if _, err := in.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM effects"); n != 1 {
		t.Fatalf("effects = %d, want 1", n)
	}
	if n := count(t, d, "SELECT count(*) FROM inbox_events WHERE status = 'done'"); n != 1 {
		t.Fatalf("done = %d, want 1", n)
	}
}

// AC: processing is asynchronous: Run processes received events in the background.
func TestInboxRunProcessesAsynchronously(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	in := messaging.NewInbox(d, messaging.Config{PollInterval: time.Hour})
	got := make(chan string, 1)
	in.Handle("github.push", func(_ context.Context, e messaging.InboundEvent) error {
		got <- e.DeliveryID
		return nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- in.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, err := in.Receive(t.Context(), event("d-9")); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-got:
		if id != "d-9" {
			t.Fatalf("processed %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("event not processed")
	}
}

// AC: no secrets in stored payloads: secret-looking fields of inbound payloads are redacted before storage.
func TestInboxRedactsSecretFields(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	in := messaging.NewInbox(d, config(newClock()))
	e := event("d-2")
	e.Payload = []byte(`{"ref":"dev","hook":{"config":{"secret":"s3cr3t","url":"https://x"}},"list":[{"access_token":"t0k"}],"n":12345678901234567890}`)
	if _, err := in.Receive(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := d.QueryRowContext(t.Context(), "SELECT CAST(payload AS TEXT) FROM inbox_events").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"s3cr3t", "t0k"} {
		if strings.Contains(stored, leak) {
			t.Fatalf("stored payload leaks %q: %s", leak, stored)
		}
	}
	for _, keep := range []string{`"ref":"dev"`, `"url":"https://x"`, `12345678901234567890`} {
		if !strings.Contains(stored, keep) {
			t.Fatalf("stored payload lost %s: %s", keep, stored)
		}
	}
	bad := event("d-3")
	bad.Payload = []byte("not json")
	if _, err := in.Receive(t.Context(), bad); !errors.Is(err, messaging.ErrInvalidMessage) {
		t.Fatalf("Receive(non-JSON) = %v, want ErrInvalidMessage", err)
	}
	if _, err := in.Receive(t.Context(), messaging.InboundEvent{Source: "github", Topic: "t", Payload: []byte(`{}`)}); !errors.Is(err, messaging.ErrInvalidMessage) {
		t.Fatalf("Receive(no delivery id) = %v, want ErrInvalidMessage", err)
	}
}

func TestInboxDeadAndRetry(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.MaxAttempts = 2
	in := messaging.NewInbox(d, cfg)
	fail := true
	in.Handle("github.push", func(context.Context, messaging.InboundEvent) error {
		if fail {
			return errors.New("bad state")
		}
		return nil
	})
	if _, err := in.Receive(t.Context(), event("d-1")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := in.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Hour)
	}
	dead, err := in.Dead(t.Context(), 10)
	if err != nil || len(dead) != 1 || dead[0].Endpoint != "github" || dead[0].Key != "d-1" || dead[0].Attempts != 2 {
		t.Fatalf("dead = %+v, %v", dead, err)
	}
	fail = false
	if err := in.Retry(t.Context(), dead[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := in.Poll(t.Context()); err != nil || n != 1 {
		t.Fatalf("Poll after retry = %d, %v", n, err)
	}
	stats, err := in.Stats(t.Context())
	if err != nil || stats["github"].Dead != 0 || stats["github"].Pending != 0 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

// Review: a graceful shutdown while processing gives the attempt back and leaves the event due at once.
func TestInboxShutdownDuringProcessingKeepsAttempt(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	in := messaging.NewInbox(d, messaging.Config{PollInterval: time.Hour})
	started := make(chan struct{})
	in.Handle("github.push", func(ctx context.Context, _ messaging.InboundEvent) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	for _, id := range []string{"d-1", "d-2"} {
		if _, err := in.Receive(t.Context(), event(id)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- in.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("processing did not start")
	}
	cancel()
	<-done
	if n := count(t, d, "SELECT count(*) FROM inbox_events WHERE status = 'pending' AND attempts = 0"); n != 2 {
		t.Fatalf("events due at once after shutdown = %d, want 2", n)
	}
}
