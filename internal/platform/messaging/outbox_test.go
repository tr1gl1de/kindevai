package messaging_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

func msg(key string) messaging.Message {
	return messaging.Message{Adapter: "youtrack", Topic: "youtrack.comment", Key: key, Payload: []byte(`{"issue":"KIN-1"}`)}
}

// AC: the outbox row is written in the same transaction as the domain change; rollback leaves no message.
func TestOutboxEnqueueJoinsTransaction(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	if _, err := d.ExecContext(t.Context(), "CREATE TABLE domain_rows (v TEXT)"); err != nil {
		t.Fatal(err)
	}
	o := messaging.NewOutbox(d, config(newClock()))
	tx := db.Transactor{DB: d}
	change := func(ctx context.Context, key string) error {
		if _, err := db.Conn(ctx, d).ExecContext(ctx, "INSERT INTO domain_rows VALUES (?)", key); err != nil {
			return err
		}
		return o.Enqueue(ctx, msg(key))
	}

	boom := errors.New("domain invariant violated")
	err := tx.InTx(t.Context(), func(ctx context.Context) error {
		if err := change(ctx, "rolled-back"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx = %v, want %v", err, boom)
	}
	if n := count(t, d, "SELECT count(*) FROM outbox_events"); n != 0 {
		t.Fatalf("rolled back transaction left %d outbox events", n)
	}

	if err := tx.InTx(t.Context(), func(ctx context.Context) error { return change(ctx, "committed") }); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM outbox_events WHERE idempotency_key = 'committed'"); n != 1 {
		t.Fatalf("committed transaction has %d outbox events, want 1", n)
	}
	if n := count(t, d, "SELECT count(*) FROM domain_rows"); n != 1 {
		t.Fatalf("domain rows = %d, want 1", n)
	}
}

// Enqueueing the same idempotency key twice (e.g. a retried use case) stores one message.
func TestOutboxEnqueueIsIdempotent(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, config(newClock()))
	for range 2 {
		if err := o.Enqueue(t.Context(), msg("k1")); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, d, "SELECT count(*) FROM outbox_events"); n != 1 {
		t.Fatalf("outbox events = %d, want 1", n)
	}
}

// Without an explicit key the outbox assigns one at enqueue time, so every retry carries the same key.
func TestOutboxAssignsStableKey(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	o := messaging.NewOutbox(d, config(c))
	var keys []string
	o.Handle("youtrack.comment", func(_ context.Context, m messaging.Delivery) error {
		keys = append(keys, m.Key)
		return errors.New("503 from YouTrack")
	})
	if err := o.Enqueue(t.Context(), msg("")); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := o.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Hour)
	}
	if len(keys) != 3 || keys[0] == "" || keys[1] != keys[0] || keys[2] != keys[0] {
		t.Fatalf("keys across retries = %q, want one stable non-empty key", keys)
	}
}

func TestOutboxRejectsInvalidMessages(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, config(newClock()))
	big := make([]byte, messaging.MaxPayloadBytes+1)
	for i := range big {
		big[i] = ' '
	}
	big[0], big[len(big)-1] = '{', '}'
	for name, m := range map[string]messaging.Message{
		"no adapter":      {Topic: "t", Payload: []byte(`{}`)},
		"no topic":        {Adapter: "a", Payload: []byte(`{}`)},
		"not json":        {Adapter: "a", Topic: "t", Payload: []byte(`not json`)},
		"too large":       {Adapter: "a", Topic: "t", Payload: big},
		"token field":     {Adapter: "a", Topic: "t", Payload: []byte(`{"issue":"KIN-1","auth":{"access_token":"x"}}`)},
		"password field":  {Adapter: "a", Topic: "t", Payload: []byte(`[{"Password":"x"}]`)},
		"api key field":   {Adapter: "a", Topic: "t", Payload: []byte(`{"api-key":"x"}`)},
		"authorization":   {Adapter: "a", Topic: "t", Payload: []byte(`{"Authorization":"Bearer x"}`)},
		"client secret":   {Adapter: "a", Topic: "t", Payload: []byte(`{"clientSecret":"x"}`)},
		"private key":     {Adapter: "a", Topic: "t", Payload: []byte(`{"private_key":"x"}`)},
		"key too long":    {Adapter: "a", Topic: "t", Key: string(make([]byte, 300)), Payload: []byte(`{}`)},
		"adapter too lon": {Adapter: string(make([]byte, 100)), Topic: "t", Payload: []byte(`{}`)},
	} {
		if err := o.Enqueue(t.Context(), m); !errors.Is(err, messaging.ErrInvalidMessage) {
			t.Errorf("%s: Enqueue = %v, want ErrInvalidMessage", name, err)
		}
	}
	if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "a", Topic: "t", Payload: []byte(`{"tokens_used":3,"token_count":1}`)}); err != nil {
		t.Errorf("harmless field names rejected: %v", err)
	}
}

// AC: delivery retries with exponential backoff + jitter; the attempt limit moves the message to dead.
func TestOutboxBackoffThenDead(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.MaxAttempts = 4
	cfg.BaseBackoff = time.Second
	cfg.BreakerThreshold = 100 // keep the circuit closed: this test is about per-message retries
	o := messaging.NewOutbox(d, cfg)
	calls := 0
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error {
		calls++
		return errors.New("connection refused")
	})
	if err := o.Enqueue(t.Context(), msg("k")); err != nil {
		t.Fatal(err)
	}
	// Jitter 0.5 with "equal jitter": wait = d/2 + 0.5*d/2 = 0.75*d for d = 1s, 2s, 4s.
	waits := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second}
	for i, w := range waits {
		if n, err := o.Poll(t.Context()); err != nil || n != 1 {
			t.Fatalf("attempt %d: Poll = %d, %v", i+1, n, err)
		}
		c.Advance(w - time.Millisecond)
		if n, _ := o.Poll(t.Context()); n != 0 {
			t.Fatalf("attempt %d: retried before the backoff of %s elapsed", i+1, w)
		}
		c.Advance(time.Millisecond)
	}
	if n, err := o.Poll(t.Context()); err != nil || n != 1 {
		t.Fatalf("last attempt: Poll = %d, %v", n, err)
	}
	if calls != 4 {
		t.Fatalf("handler calls = %d, want 4", calls)
	}
	dead, err := o.Dead(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].Attempts != 4 || dead[0].LastError != "connection refused" || dead[0].Key != "k" {
		t.Fatalf("dead = %+v", dead)
	}
	c.Advance(24 * time.Hour)
	if n, _ := o.Poll(t.Context()); n != 0 {
		t.Fatal("dead message was delivered again")
	}

	// An operator retry puts it back in the queue with a fresh attempt budget.
	if err := o.Retry(t.Context(), dead[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := o.Poll(t.Context()); n != 1 {
		t.Fatal("retried message was not delivered")
	}
	if err := o.Retry(t.Context(), 999); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("Retry(unknown) = %v, want ErrNotFound", err)
	}
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	t.Parallel()
	cfg := messaging.Config{BaseBackoff: time.Second, MaxBackoff: time.Minute}
	for _, tc := range []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0, 500 * time.Millisecond},
		{1, 0.999999999, time.Second},
		{3, 0, 2 * time.Second},
		{20, 0, 30 * time.Second},
		{20, 0.999999999, time.Minute},
		{200, 0, 30 * time.Second}, // no overflow
	} {
		cfg.Jitter = func() float64 { return tc.jitter }
		got := cfg.Backoff(tc.attempt)
		if diff := got - tc.want; diff < -time.Millisecond || diff > time.Millisecond {
			t.Errorf("Backoff(%d, jitter %v) = %s, want %s", tc.attempt, tc.jitter, got, tc.want)
		}
	}
}

// A Permanent error (e.g. HTTP 4xx) is not retried.
func TestOutboxPermanentErrorGoesDead(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, config(newClock()))
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error {
		return messaging.Permanent(errors.New("404 issue not found"))
	})
	if err := o.Enqueue(t.Context(), msg("k")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if dead, _ := o.Dead(t.Context(), 10); len(dead) != 1 || dead[0].Attempts != 1 {
		t.Fatalf("dead = %+v, want the message dead after one attempt", dead)
	}
	if s := o.Health().Status("youtrack"); s.State != messaging.StateOK {
		t.Fatalf("a rejected message is not an outage, state = %s", s.State)
	}
}

// AC: a crash between delivery and marking it delivered leads to redelivery with the same idempotency key.
func TestOutboxRedeliversSameKeyAfterCrash(t *testing.T) {
	t.Parallel()
	_, path := newDB(t)
	c := newClock()
	external := &recorder{}

	// Process 1 delivers, then "crashes" before it can record the delivery.
	d1 := openDB(t, path)
	o1 := messaging.NewOutbox(d1, config(c))
	o1.Handle("youtrack.comment", func(ctx context.Context, m messaging.Delivery) error {
		_ = external.deliver(ctx, m)
		_ = d1.Close() // the process dies right after the external call succeeded
		return nil
	})
	if err := o1.Enqueue(t.Context(), msg("comment-42")); err != nil {
		t.Fatal(err)
	}
	if _, err := o1.Poll(t.Context()); err == nil {
		t.Fatal("Poll after crash: want an error recording the delivery")
	}

	// Process 2 starts on the same database; once the lease of the dead worker expires the message is redelivered.
	d2 := openDB(t, path)
	o2 := messaging.NewOutbox(d2, config(c))
	o2.Handle("youtrack.comment", external.deliver)
	if n, err := o2.Poll(t.Context()); err != nil || n != 0 {
		t.Fatalf("Poll during lease = %d, %v; want nothing (the lease is still held)", n, err)
	}
	c.Advance(messaging.Config{}.WithDefaults().Lease)
	if n, err := o2.Poll(t.Context()); err != nil || n != 1 {
		t.Fatalf("Poll after lease = %d, %v", n, err)
	}
	if got := external.got(); !slices.Equal(got, []string{"comment-42", "comment-42"}) {
		t.Fatalf("external system received %q, want the same key twice", got)
	}
	if n := count(t, d2, "SELECT count(*) FROM outbox_events WHERE status = 'done'"); n != 1 {
		t.Fatalf("done = %d, want 1", n)
	}
}

// AC: pending messages survive a restart, and Run delivers them (also when woken by Enqueue).
func TestOutboxRunDeliversPendingAfterRestart(t *testing.T) {
	t.Parallel()
	_, path := newDB(t)
	d1 := openDB(t, path)
	if err := messaging.NewOutbox(d1, messaging.Config{}).Enqueue(t.Context(), msg("before-restart")); err != nil {
		t.Fatal(err)
	}
	_ = d1.Close()

	d2 := openDB(t, path)
	cfg := messaging.Config{PollInterval: time.Hour} // only the startup poll and the wake-up channel can deliver
	o := messaging.NewOutbox(d2, cfg)
	got := make(chan string, 2)
	o.Handle("youtrack.comment", func(_ context.Context, m messaging.Delivery) error {
		got <- m.Key
		return nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	want := func(key string) {
		t.Helper()
		select {
		case k := <-got:
			if k != key {
				t.Fatalf("delivered %q, want %q", k, key)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%q not delivered", key)
		}
	}
	want("before-restart")
	if err := o.Enqueue(t.Context(), msg("after-restart")); err != nil {
		t.Fatal(err)
	}
	want("after-restart")
}

// AC: an unavailable external service does not block the rest of the platform: a hanging delivery holds no
// database connection, so API reads and new domain transactions proceed.
func TestOutboxHangingDeliveryDoesNotBlockDatabase(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	cfg := messaging.Config{Workers: 1}
	o := messaging.NewOutbox(d, cfg)
	started := make(chan struct{})
	release := make(chan struct{})
	o.Handle("youtrack.comment", func(ctx context.Context, _ messaging.Delivery) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return errors.New("timeout")
	})
	if err := o.Enqueue(t.Context(), msg("hangs")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	t.Cleanup(func() { close(release); cancel(); <-done })
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("delivery did not start")
	}

	quick, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	if err := (db.Transactor{DB: d}).InTx(quick, func(ctx context.Context) error {
		return o.Enqueue(ctx, msg("another"))
	}); err != nil {
		t.Fatalf("domain transaction blocked by a hanging delivery: %v", err)
	}
	if _, err := o.Stats(quick); err != nil {
		t.Fatalf("status read blocked by a hanging delivery: %v", err)
	}
}

// AC: bounded retention of payloads.
func TestOutboxPurgeRemovesOldFinishedMessages(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	o := messaging.NewOutbox(d, config(c))
	o.Handle("youtrack.comment", func(_ context.Context, m messaging.Delivery) error {
		if m.Key == "fails" {
			return messaging.Permanent(errors.New("rejected"))
		}
		return nil
	})
	for _, k := range []string{"delivered", "fails"} {
		if err := o.Enqueue(t.Context(), msg(k)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Hour)
	if err := o.Enqueue(t.Context(), msg("pending")); err != nil {
		t.Fatal(err)
	}
	keys := func() []string {
		rows, err := d.QueryContext(t.Context(), "SELECT idempotency_key FROM outbox_events ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			out = append(out, k)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	c.Advance(7*24*time.Hour - 2*time.Hour)
	if _, err := o.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := keys(); len(got) != 3 {
		t.Fatalf("purged too early: %q", got)
	}
	c.Advance(2 * time.Hour)
	if n, err := o.Purge(t.Context()); err != nil || n != 1 {
		t.Fatalf("Purge after 7d = %d, %v; want the delivered message removed", n, err)
	}
	c.Advance(23 * 24 * time.Hour)
	if n, err := o.Purge(t.Context()); err != nil || n != 1 {
		t.Fatalf("Purge after 30d = %d, %v; want the dead message removed", n, err)
	}
	if got := keys(); !slices.Equal(got, []string{"pending"}) {
		t.Fatalf("remaining = %q, want only the pending message", got)
	}
}

func TestOutboxUnknownTopicGoesDead(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, config(newClock()))
	if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "github", Topic: "github.nobody", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if dead, _ := o.Dead(t.Context(), 10); len(dead) != 1 || dead[0].LastError != "no handler for topic" {
		t.Fatalf("dead = %+v", dead)
	}
}

// AC: per-adapter integration state; after repeated failures the circuit opens and the adapter is not called
// until the cooldown ends; other adapters keep flowing.
func TestOutboxCircuitBreakerPerAdapter(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.BreakerThreshold = 2
	cfg.BreakerCooldown = time.Minute
	cfg.BaseBackoff = time.Millisecond
	o := messaging.NewOutbox(d, cfg)
	var yt, gh atomic.Int32
	ytDown := true // only changed between polls; Poll waits for its deliveries
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error {
		yt.Add(1)
		if ytDown {
			return errors.New("503")
		}
		return nil
	})
	o.Handle("github.status", func(context.Context, messaging.Delivery) error { gh.Add(1); return nil })
	for _, k := range []string{"y1", "y2", "y3"} {
		if err := o.Enqueue(t.Context(), msg(k)); err != nil {
			t.Fatal(err)
		}
	}
	if s := o.Health().Status("youtrack"); s.State != messaging.StateOK {
		t.Fatalf("registered adapter state = %s, want ok", s.State)
	}
	cfgWorkers := cfg.WithDefaults().Workers
	if cfgWorkers < 2 {
		t.Fatalf("default workers = %d", cfgWorkers)
	}
	// One poll takes y1 and y2 (two workers): both fail -> circuit open.
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := o.Health().Status("youtrack")
	if s.State != messaging.StateCircuitOpen || s.LastError != "503" || s.ConsecutiveFailures != 2 {
		t.Fatalf("status = %+v, want circuit_open", s)
	}
	if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "github", Topic: "github.status", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Second)
	before := yt.Load()
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if yt.Load() != before {
		t.Fatal("open circuit still called the adapter")
	}
	if gh.Load() != 1 {
		t.Fatalf("other adapter calls = %d, want 1 while youtrack's circuit is open", gh.Load())
	}

	// After the cooldown a single probe goes through; its success closes the circuit.
	c.Advance(time.Minute)
	ytDown = false
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if yt.Load() != before+1 {
		t.Fatalf("half-open probe calls = %d, want 1", yt.Load()-before)
	}
	if s := o.Health().Status("youtrack"); s.State != messaging.StateOK {
		t.Fatalf("after successful probe state = %s, want ok", s.State)
	}
	c.Advance(time.Second)
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM outbox_events WHERE status = 'done' AND adapter = 'youtrack'"); n != 3 {
		t.Fatalf("delivered youtrack messages = %d, want 3", n)
	}
	stats, err := o.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats["youtrack"].Pending != 0 || stats["github"].Pending != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// Review: a graceful shutdown during a delivery is not a failure: the attempt is given back, the adapter's
// health is unchanged and the message is due again at once.
func TestOutboxShutdownDuringDeliveryKeepsAttempt(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	o := messaging.NewOutbox(d, messaging.Config{PollInterval: time.Hour})
	started := make(chan struct{})
	o.Handle("youtrack.comment", func(ctx context.Context, _ messaging.Delivery) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err := o.Enqueue(t.Context(), msg("k")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("delivery did not start")
	}
	cancel()
	<-done
	if n := count(t, d, "SELECT count(*) FROM outbox_events WHERE status = 'pending' AND attempts = 0 AND last_error IS NULL"); n != 1 {
		t.Fatal("shutdown consumed the attempt")
	}
	if s := o.Health().Status("youtrack"); s.State != messaging.StateOK {
		t.Fatalf("shutdown counted as an outage: %+v", s)
	}
	o2 := messaging.NewOutbox(d, messaging.Config{})
	o2.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error { return nil })
	if n, err := o2.Poll(t.Context()); err != nil || n != 1 {
		t.Fatalf("Poll after restart = %d, %v; want the message due at once", n, err)
	}
}

// Review: while one adapter is half-open (a single probe in flight), its backlog must not hide other adapters'
// due messages from the same claim.
func TestOutboxHalfOpenAdapterDoesNotStarveOthers(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.BreakerThreshold = 1
	cfg.BreakerCooldown = time.Minute
	o := messaging.NewOutbox(d, cfg)
	var gh atomic.Int32
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error { return errors.New("503") })
	o.Handle("github.status", func(context.Context, messaging.Delivery) error { gh.Add(1); return nil })
	if err := o.Enqueue(t.Context(), msg("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Poll(t.Context()); err != nil { // fails -> circuit open
		t.Fatal(err)
	}
	for i := range 20 {
		if err := o.Enqueue(t.Context(), msg(fmt.Sprint("backlog-", i))); err != nil {
			t.Fatal(err)
		}
	}
	c.Advance(time.Second)
	if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "github", Topic: "github.status", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute) // cooldown over: youtrack is half-open, its backlog sorts first
	if n, err := o.Poll(t.Context()); err != nil || n != 2 {
		t.Fatalf("Poll = %d, %v; want the youtrack probe plus the github message", n, err)
	}
	if gh.Load() != 1 {
		t.Fatal("github message starved behind the half-open youtrack backlog")
	}
}

// Review: a message without a handler says nothing about the adapter, so it must not close an open circuit.
func TestOutboxMissingHandlerDoesNotCloseCircuit(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.BreakerThreshold = 1
	o := messaging.NewOutbox(d, cfg)
	o.Health().Failure("youtrack", errors.New("503"))
	c.Advance(time.Hour)
	if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "youtrack", Topic: "youtrack.unknown", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := o.Health().Status("youtrack"); s.State != messaging.StateCircuitOpen {
		t.Fatalf("state = %s, want still circuit_open", s.State)
	}
	if !o.Health().Allow("youtrack") {
		t.Fatal("the probe slot was not released")
	}
}

// KIN-40: messages that went dead only because no handler was registered yet are redelivered once their handler
// exists; dead messages of still-unhandled topics and real failures stay dead.
func TestOutboxRetryUnhandledRequeuesOnlyNowHandledTopics(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	o := messaging.NewOutbox(d, config(c))
	for _, m := range []messaging.Message{
		{Adapter: "alerts", Topic: "alerts.runner_unreachable", Key: "a", Payload: []byte(`{}`)},
		{Adapter: "alerts", Topic: "alerts.nobody", Key: "b", Payload: []byte(`{}`)},
		{Adapter: "github", Topic: "github.status", Key: "c", Payload: []byte(`{}`)},
	} {
		if err := o.Enqueue(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	o.Handle("github.status", func(context.Context, messaging.Delivery) error {
		return messaging.Permanent(errors.New("422"))
	})
	for range 2 { // two workers per poll
		if _, err := o.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if dead, _ := o.Dead(t.Context(), 10); len(dead) != 3 {
		t.Fatalf("dead = %+v, want 3", dead)
	}
	rec := &recorder{}
	o.Handle("alerts.runner_unreachable", rec.deliver)
	n, err := o.RetryUnhandled(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("RetryUnhandled = %d, %v; want 1", n, err)
	}
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.keys, []string{"a"}) {
		t.Fatalf("delivered = %q, want [a]", rec.keys)
	}
	dead, _ := o.Dead(t.Context(), 10)
	var left []string
	for _, m := range dead {
		left = append(left, m.Key)
	}
	slices.Sort(left)
	if !slices.Equal(left, []string{"b", "c"}) {
		t.Fatalf("still dead = %q, want [b c]", left)
	}
}

func ordered(key, order string) messaging.Message {
	m := msg(key)
	m.OrderingKey = order
	return m
}

// KIN-107: messages of one adapter and ordering key are delivered one at a time in enqueue order: a later message
// waits while an earlier one is pending (in flight or backing off); a dead one no longer holds it back. Other keys
// and unordered messages are not held up.
func TestOutboxOrderingKeyDeliversInEnqueueOrder(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	c := newClock()
	cfg := config(c)
	cfg.Workers = 8
	cfg.BaseBackoff = time.Second
	cfg.BreakerThreshold = 100
	o := messaging.NewOutbox(d, cfg)
	mu := make(chan struct{}, 1)
	var got []string
	failA := 1
	o.Handle("youtrack.comment", func(_ context.Context, dl messaging.Delivery) error {
		mu <- struct{}{}
		defer func() { <-mu }()
		if dl.Key == "a" && failA > 0 {
			failA--
			return errors.New("connection refused")
		}
		if dl.Key == "x" {
			return messaging.Permanent(errors.New("rejected"))
		}
		got = append(got, dl.Key)
		return nil
	})
	for _, m := range []messaging.Message{ordered("a", "task/1"), ordered("b", "task/1"), ordered("c", "task/1"),
		ordered("p", "task/2"), msg("free")} {
		if err := o.Enqueue(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := o.Poll(t.Context()); n != 3 { // a (fails), p, free: b and c wait for a
		t.Fatalf("first poll attempted %d, want 3", n)
	}
	if n, _ := o.Poll(t.Context()); n != 0 {
		t.Fatalf("b was delivered while a was backing off (%d attempted)", n)
	}
	c.Advance(time.Minute)
	for range 3 {
		if _, err := o.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(got[:2]) // p and free ran concurrently
	if !slices.Equal(got, []string{"free", "p", "a", "b", "c"}) {
		t.Fatalf("delivered %v, want free and p, then a, b, c", got)
	}

	// A dead message does not block its successors.
	for _, m := range []messaging.Message{ordered("x", "task/3"), ordered("y", "task/3")} {
		if err := o.Enqueue(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := o.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got[len(got)-1] != "y" {
		t.Fatalf("y was not delivered after x went dead: %v", got)
	}
}
