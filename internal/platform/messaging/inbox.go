package messaging

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// InboundEvent is an event received from an external system.
type InboundEvent struct {
	ID         int64  // set when processing
	Source     string // sender: "github", "youtrack", "telegram", "runner"
	DeliveryID string // the sender's delivery/event id (e.g. X-GitHub-Delivery); duplicates are dropped
	Topic      string // selects the handler registered with Inbox.Handle, e.g. "github.push"
	Payload    []byte // JSON, at most MaxPayloadBytes; secret-looking fields are redacted before storage
	Attempt    int    // 1 for the first processing attempt
}

// ProcessFunc handles one event. It runs inside the transaction that marks the event done, so its database
// effects happen exactly once; it must use repositories that execute via db.Conn(ctx, d) (any other statement
// waits for the single connection the transaction holds). It must be quick (Config.InboxTimeout): while it
// runs, every other database user waits. It must not call external systems — enqueue
// to the Outbox instead, in the same transaction. Errors are retried like outbox deliveries; Permanent(err)
// makes the event dead at once.
type ProcessFunc func(ctx context.Context, e InboundEvent) error

// Inbox stores incoming events durably, deduplicates them and processes them asynchronously.
type Inbox struct {
	q        *queue
	mu       sync.RWMutex
	handlers map[string]ProcessFunc
}

// NewInbox returns the inbox backed by d.
func NewInbox(d *sql.DB, cfg Config) *Inbox {
	return &Inbox{q: newQueue(d, cfg, inboxTable), handlers: map[string]ProcessFunc{}}
}

// Handle registers the handler of a topic.
func (in *Inbox) Handle(topic string, fn ProcessFunc) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.handlers[topic] = fn
}

func (in *Inbox) handler(topic string) ProcessFunc {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.handlers[topic]
}

// Receive stores e durably (committed when it returns nil, or with the caller's transaction) and reports
// whether it is new; a duplicate (same source and delivery id) returns false and nil. Acknowledge the sender
// (2xx) only after Receive returned nil, also for duplicates; authenticate the request before calling it.
func (in *Inbox) Receive(ctx context.Context, e InboundEvent) (bool, error) {
	for _, f := range []struct {
		name, v string
		max     int
	}{{"source", e.Source, maxEndpointBytes}, {"delivery id", e.DeliveryID, maxKeyBytes}, {"topic", e.Topic, maxTopicBytes}} {
		if err := checkField(f.name, f.v, f.max); err != nil {
			return false, err
		}
	}
	p, err := inboxPayload(e.Payload)
	if err != nil {
		return false, fmt.Errorf("inbox %s: %w", e.Topic, err)
	}
	return in.q.insert(ctx, e.Source, e.Topic, e.DeliveryID, p)
}

// handlerError separates a handler's failure (retried with backoff) from a failure to commit.
type handlerError struct{ err error }

func (e handlerError) Error() string { return e.err.Error() }
func (e handlerError) Unwrap() error { return e.err }

func (in *Inbox) process(ctx context.Context, r record) error {
	h := in.handler(r.topic)
	var cause error
	if h == nil {
		cause = Permanent(errMissingHandler)
	} else {
		err := db.Transactor{DB: in.q.d}.InTx(ctx, func(ctx context.Context) error {
			if err := in.call(ctx, h, r); err != nil {
				return handlerError{err}
			}
			_, err := in.q.finish(ctx, r, nil) // joins the transaction via db.Conn
			return err
		})
		var he handlerError
		if !errors.As(err, &he) {
			return err // nil, or a storage failure: the lease expires and the event is processed again
		}
		cause = he.err
		if ctx.Err() != nil { // stopping: not the event's fault
			return in.q.giveBack(ctx, r)
		}
	}
	dead, err := in.q.finish(ctx, r, cause)
	if dead {
		in.q.cfg.Logger.Warn("inbox event dead", "id", r.id, "source", r.endpoint, "topic", r.topic,
			"attempts", r.attempts, "err", truncate(cause.Error()))
	}
	return err
}

func (in *Inbox) call(ctx context.Context, h ProcessFunc, r record) (err error) {
	ctx, cancel := context.WithTimeout(ctx, in.q.cfg.InboxTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			in.q.cfg.Logger.Error("inbox handler panic", "id", r.id, "topic", r.topic)
			err = errPanic
		}
	}()
	return h(ctx, InboundEvent{ID: r.id, Source: r.endpoint, DeliveryID: r.key, Topic: r.topic, Payload: r.payload, Attempt: r.attempts})
}

// Poll claims one due event and processes it (one at a time: the handler holds the only write connection, and
// a stop never leaves claimed-but-unstarted events hidden behind their lease). It returns how many were
// attempted (0 or 1) and the error recording the outcome, if any.
func (in *Inbox) Poll(ctx context.Context) (int, error) {
	rs, err := in.q.claim(ctx, 1, nil, nil, nil)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, r := range rs {
		if err := in.process(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return len(rs), errors.Join(errs...)
}

// Run processes events until ctx is cancelled: at start, on every Receive, every PollInterval; it purges
// finished events hourly.
func (in *Inbox) Run(ctx context.Context) error {
	in.q.loop(ctx, nil, func(ctx context.Context) bool {
		n, err := in.Poll(ctx)
		if err != nil && ctx.Err() == nil {
			in.q.cfg.Logger.Error("inbox poll", "err", err)
			return false
		}
		return n > 0
	})
	return nil
}

// Dead lists dead events, newest first.
func (in *Inbox) Dead(ctx context.Context, limit int) ([]DeadMessage, error) {
	return in.q.listDead(ctx, limit)
}

// Retry puts a dead event back into the queue with a fresh attempt budget.
func (in *Inbox) Retry(ctx context.Context, id int64) error { return in.q.retry(ctx, id) }

// Purge deletes done events older than Config.Retention and dead ones older than Config.DeadRetention. A
// redelivery of a purged event is accepted again, so retention must exceed the senders' redelivery window.
func (in *Inbox) Purge(ctx context.Context) (int64, error) { return in.q.purge(ctx) }

// Stats counts pending and dead events per source.
func (in *Inbox) Stats(ctx context.Context) (map[string]Counts, error) { return in.q.stats(ctx) }
