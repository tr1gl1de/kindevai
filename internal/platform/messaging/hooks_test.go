package messaging_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// KIN-108: a topic's dead-letter and retry hooks run in the transaction that changes the message, so the domain
// record that mirrors the message (e.g. a YouTrack write "awaiting publication") follows it whatever made it dead.

func hookTable(t *testing.T, d *sql.DB) {
	t.Helper()
	if _, err := d.ExecContext(t.Context(), "CREATE TABLE hook_rows (event TEXT, key TEXT, cause TEXT)"); err != nil {
		t.Fatal(err)
	}
}

// recordHook returns hooks that store what they saw through the caller's transaction (db.Conn).
func recordHooks(d *sql.DB) (messaging.DeadFunc, messaging.RetryFunc) {
	dead := func(ctx context.Context, m messaging.Delivery, cause error) error {
		_, err := db.Conn(ctx, d).ExecContext(ctx, "INSERT INTO hook_rows VALUES ('dead', ?, ?)", m.Key, cause.Error())
		return err
	}
	retry := func(ctx context.Context, m messaging.Delivery) error {
		_, err := db.Conn(ctx, d).ExecContext(ctx, "INSERT INTO hook_rows VALUES ('retry', ?, '')", m.Key)
		return err
	}
	return dead, retry
}

func deadAfterPoll(t *testing.T, o *messaging.Outbox) []messaging.DeadMessage {
	t.Helper()
	if _, err := o.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	dead, err := o.Dead(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	return dead
}

func TestOutboxOnDeadRunsWhenHandlerPanics(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	hookTable(t, d)
	onDead, _ := recordHooks(d)
	cfgMax := messaging.Config{}.WithDefaults().MaxAttempts
	c := newClock()
	o := messaging.NewOutbox(d, config(c))
	o.OnDead("youtrack.comment", onDead)
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error { panic("boom") })
	if err := o.Enqueue(t.Context(), msg("k-panic")); err != nil {
		t.Fatal(err)
	}
	for range cfgMax {
		if _, err := o.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n := count(t, d, "SELECT count(*) FROM hook_rows"); n != 0 && count(t, d, "SELECT count(*) FROM outbox_events WHERE status = 'dead'") == 0 {
			t.Fatal("dead hook ran before the message went dead")
		}
		c.Advance(24 * time.Hour)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows WHERE event = 'dead' AND key = 'k-panic' AND cause = 'handler panic'"); n != 1 {
		t.Fatalf("dead hook rows = %d, want one for the panicking handler", n)
	}
}

func TestOutboxOnDeadRunsWithoutHandler(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	hookTable(t, d)
	o := messaging.NewOutbox(d, config(newClock()))
	onDead, _ := recordHooks(d)
	o.OnDead("youtrack.comment", onDead) // a hook, but no handler: the message goes dead at once
	if err := o.Enqueue(t.Context(), msg("k-nohandler")); err != nil {
		t.Fatal(err)
	}
	if dead := deadAfterPoll(t, o); len(dead) != 1 {
		t.Fatalf("dead = %+v, want the unhandled message", dead)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows WHERE event = 'dead' AND key = 'k-nohandler' AND cause = 'no handler for topic'"); n != 1 {
		t.Fatalf("dead hook rows = %d, want 1", n)
	}
}

func TestOutboxOnDeadNotCalledForRetryableFailureOrOtherTopics(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	hookTable(t, d)
	o := messaging.NewOutbox(d, config(newClock()))
	onDead, _ := recordHooks(d)
	o.OnDead("youtrack.comment", onDead)
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error { return errors.New("503") })
	if err := o.Enqueue(t.Context(), msg("k-retry")); err != nil {
		t.Fatal(err)
	}
	if err := o.Enqueue(t.Context(), messaging.Message{Adapter: "github", Topic: "github.nobody", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if dead := deadAfterPoll(t, o); len(dead) != 1 || dead[0].Topic != "github.nobody" {
		t.Fatalf("dead = %+v, want only the unhandled github message", dead)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows"); n != 0 {
		t.Fatalf("hook rows = %d, want none (one message backs off, the dead one has no hook)", n)
	}
}

// A failing hook rolls the dead mark back: the message stays pending (leased) and goes dead with its hook later.
func TestOutboxOnDeadFailureKeepsMessagePending(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	hookTable(t, d)
	c := newClock()
	o := messaging.NewOutbox(d, config(c))
	onDead, _ := recordHooks(d)
	fail := true
	o.OnDead("youtrack.comment", func(ctx context.Context, m messaging.Delivery, cause error) error {
		if fail {
			return errors.New("disk full")
		}
		return onDead(ctx, m, cause)
	})
	o.Handle("youtrack.comment", func(context.Context, messaging.Delivery) error {
		return messaging.Permanent(errors.New("rejected"))
	})
	if err := o.Enqueue(t.Context(), msg("k-hookfail")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Poll(t.Context()); err == nil {
		t.Fatal("Poll = nil, want the hook's error")
	}
	if n := count(t, d, "SELECT count(*) FROM outbox_events WHERE status = 'pending'"); n != 1 {
		t.Fatalf("pending = %d, want the message kept pending when its dead hook fails", n)
	}
	fail = false
	c.Advance(time.Hour) // the lease expires
	if dead := deadAfterPoll(t, o); len(dead) != 1 {
		t.Fatalf("dead = %+v, want the message dead on the next attempt", dead)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows WHERE event = 'dead'"); n != 1 {
		t.Fatalf("dead hook rows = %d, want 1", n)
	}
}

func TestOutboxOnRetryRunsInRetryTransaction(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	hookTable(t, d)
	o := messaging.NewOutbox(d, config(newClock()))
	onDead, onRetry := recordHooks(d)
	fail := true
	o.OnDead("youtrack.comment", onDead)
	o.OnRetry("youtrack.comment", func(ctx context.Context, m messaging.Delivery) error {
		if fail {
			return errors.New("disk full")
		}
		return onRetry(ctx, m)
	})
	if err := o.Enqueue(t.Context(), msg("k-again")); err != nil {
		t.Fatal(err)
	}
	dead := deadAfterPoll(t, o)
	if len(dead) != 1 {
		t.Fatalf("dead = %+v", dead)
	}
	if err := o.Retry(t.Context(), dead[0].ID); err == nil {
		t.Fatal("Retry = nil, want the hook's error")
	}
	if n := count(t, d, "SELECT count(*) FROM outbox_events WHERE status = 'dead'"); n != 1 {
		t.Fatal("a failed retry hook must leave the message dead")
	}
	fail = false
	if err := o.Retry(t.Context(), dead[0].ID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows WHERE event = 'retry' AND key = 'k-again'"); n != 1 {
		t.Fatalf("retry hook rows = %d, want 1", n)
	}
	if err := o.Retry(t.Context(), dead[0].ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("second Retry = %v, want ErrNotFound", err)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows WHERE event = 'retry'"); n != 1 {
		t.Fatalf("retry hook rows = %d, want no hook for a message that is not dead", n)
	}
}

func TestOutboxRetryUnhandledRunsRetryHook(t *testing.T) {
	t.Parallel()
	d, _ := newDB(t)
	hookTable(t, d)
	o := messaging.NewOutbox(d, config(newClock()))
	onDead, onRetry := recordHooks(d)
	o.OnDead("youtrack.comment", onDead)
	o.OnRetry("youtrack.comment", onRetry)
	if err := o.Enqueue(t.Context(), msg("k-late")); err != nil {
		t.Fatal(err)
	}
	if dead := deadAfterPoll(t, o); len(dead) != 1 {
		t.Fatalf("dead = %+v", dead)
	}
	o.Handle("youtrack.comment", (&recorder{}).deliver)
	if n, err := o.RetryUnhandled(t.Context()); err != nil || n != 1 {
		t.Fatalf("RetryUnhandled = %d, %v", n, err)
	}
	if n := count(t, d, "SELECT count(*) FROM hook_rows WHERE event = 'retry' AND key = 'k-late'"); n != 1 {
		t.Fatalf("retry hook rows = %d, want 1", n)
	}
}
