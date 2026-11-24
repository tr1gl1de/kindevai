package messaging

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Message is an outgoing change for an external system.
type Message struct {
	Adapter string // external system ("youtrack", "github", "telegram", "runner"); its health gates delivery
	Topic   string // selects the handler registered with Outbox.Handle, e.g. "youtrack.comment"
	// Key is the idempotency key, unique per adapter: enqueueing an existing key is a no-op. Use a
	// deterministic key derived from the domain change (e.g. "task-42/status/review") so a repeated use case
	// does not send twice; empty means a random key assigned now, stable across retries.
	Key     string
	Payload []byte // JSON without secrets, at most MaxPayloadBytes
}

// Delivery is a message handed to a handler.
type Delivery struct {
	ID      int64
	Adapter string
	Topic   string
	Key     string // pass to the external system (Idempotency-Key header, marker comment) or check it before acting
	Payload []byte
	Attempt int // 1 for the first delivery
}

// DeliverFunc performs one delivery. nil = delivered; Permanent(err) = rejected, do not retry; any other error
// is retried with backoff and counts as an outage of the adapter. It must honour ctx (Config.DeliveryTimeout).
type DeliverFunc func(ctx context.Context, d Delivery) error

// Outbox stores outgoing messages and delivers them. Create one per process with NewOutbox, register handlers
// before Run.
type Outbox struct {
	q        *queue
	health   *Health
	mu       sync.RWMutex
	handlers map[string]DeliverFunc
}

// NewOutbox returns the outbox backed by d.
func NewOutbox(d *sql.DB, cfg Config) *Outbox {
	q := newQueue(d, cfg, outboxTable)
	return &Outbox{q: q, health: NewHealth(q.cfg), handlers: map[string]DeliverFunc{}}
}

// Health returns the per-adapter state fed by deliveries; pollers of the same adapters report to it too.
func (o *Outbox) Health() *Health { return o.health }

// Handle registers the handler of a topic. Topics are named "<adapter>.<action>"; the adapter becomes visible
// in Health as ok before its first delivery.
func (o *Outbox) Handle(topic string, fn DeliverFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.handlers[topic] = fn
	if adapter, _, ok := strings.Cut(topic, "."); ok && adapter != "" {
		o.health.Register(adapter)
	}
}

func (o *Outbox) handler(topic string) DeliverFunc {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.handlers[topic]
}

// Enqueue stores m. Call it inside the use case's db.Transactor.InTx so the message commits or rolls back with
// the domain change; the dispatcher is woken immediately.
func (o *Outbox) Enqueue(ctx context.Context, m Message) error {
	if m.Key == "" {
		m.Key = rand.Text()
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"adapter", m.Adapter, maxEndpointBytes}, {"topic", m.Topic, maxTopicBytes}, {"key", m.Key, maxKeyBytes}} {
		if err := checkField(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	p, err := outboxPayload(m.Payload)
	if err != nil {
		return fmt.Errorf("outbox %s: %w", m.Topic, err)
	}
	if _, err := o.q.insert(ctx, m.Adapter, m.Topic, m.Key, p); err != nil {
		return fmt.Errorf("outbox %s: %w", m.Topic, err)
	}
	return nil
}

func (o *Outbox) claim(ctx context.Context, limit int) ([]record, error) {
	return o.q.claim(ctx, limit, o.health.blocked(), o.health.Allow, o.health.abandon)
}

// deliver runs one attempt and records its outcome.
func (o *Outbox) deliver(ctx context.Context, r record) error {
	err := o.call(ctx, r)
	switch {
	case err != nil && ctx.Err() != nil:
		// The dispatcher is stopping: neither the message nor the adapter failed.
		o.health.abandon(r.endpoint)
		return o.q.giveBack(ctx, r)
	case errors.Is(err, errMissingHandler):
		o.health.abandon(r.endpoint) // no call was made, so this says nothing about the adapter
	case err == nil, isPermanent(err):
		o.health.Success(r.endpoint) // the system answered, so it is reachable
	default:
		o.health.Failure(r.endpoint, err)
	}
	dead, ferr := o.q.finish(ctx, r, err)
	if dead {
		o.q.cfg.Logger.Warn("outbox message dead", "id", r.id, "adapter", r.endpoint, "topic", r.topic,
			"attempts", r.attempts, "err", errorText(err))
	}
	return ferr
}

// errMissingHandler: no handler is registered for the topic (a message goes dead; Retry after deploying one).
var errMissingHandler = errors.New("no handler for topic")

// errPanic replaces a handler panic; the panic value is not stored or logged, it may hold request data.
var errPanic = errors.New("handler panic")

func (o *Outbox) call(ctx context.Context, r record) (err error) {
	h := o.handler(r.topic)
	if h == nil {
		return Permanent(errMissingHandler)
	}
	ctx, cancel := context.WithTimeout(ctx, o.q.cfg.DeliveryTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			o.q.cfg.Logger.Error("outbox handler panic", "id", r.id, "topic", r.topic)
			err = errPanic
		}
	}()
	return h(ctx, Delivery{ID: r.id, Adapter: r.endpoint, Topic: r.topic, Key: r.key, Payload: r.payload, Attempt: r.attempts})
}

// Poll claims up to Config.Workers due messages, delivers them concurrently and waits for them. It returns how
// many were attempted and the first error recording an outcome. Run is the production loop; Poll is for tests
// and tools.
func (o *Outbox) Poll(ctx context.Context) (int, error) {
	rs, err := o.claim(ctx, o.q.cfg.Workers)
	if err != nil {
		return 0, err
	}
	errs := make([]error, len(rs))
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Go(func() { errs[i] = o.deliver(ctx, r) })
	}
	wg.Wait()
	return len(rs), errors.Join(errs...)
}

// Run delivers messages until ctx is cancelled, with at most Config.Workers deliveries in flight. It polls at
// start (resuming messages left pending by a previous process), on every Enqueue, every PollInterval and when a
// worker frees up; it purges finished messages hourly. It returns after in-flight deliveries end.
func (o *Outbox) Run(ctx context.Context) error {
	workers := int64(o.q.cfg.Workers)
	var inflight atomic.Int64
	freed := make(chan struct{}, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	o.q.loop(ctx, freed, func(ctx context.Context) bool {
		free := workers - inflight.Load()
		if free <= 0 {
			return false
		}
		rs, err := o.claim(ctx, int(free))
		if err != nil {
			if ctx.Err() == nil {
				o.q.cfg.Logger.Error("outbox poll", "err", err)
			}
			return false
		}
		for _, r := range rs {
			inflight.Add(1)
			wg.Go(func() {
				defer func() {
					inflight.Add(-1)
					select {
					case freed <- struct{}{}:
					default:
					}
				}()
				if err := o.deliver(ctx, r); err != nil {
					o.q.cfg.Logger.Error("outbox delivery", "id", r.id, "err", err)
				}
			})
		}
		return false
	})
	return nil
}

// Dead lists dead messages, newest first.
func (o *Outbox) Dead(ctx context.Context, limit int) ([]DeadMessage, error) {
	return o.q.listDead(ctx, limit)
}

// Retry puts a dead message back into the queue with a fresh attempt budget and the same idempotency key.
func (o *Outbox) Retry(ctx context.Context, id int64) error { return o.q.retry(ctx, id) }

// maxUnhandledScan bounds how many dead messages RetryUnhandled looks at in one call.
const maxUnhandledScan = 10_000

// RetryUnhandled puts back every dead message that died only because its topic had no handler and whose topic
// has one now (call it after registering the handlers, before Run). It returns how many were requeued.
func (o *Outbox) RetryUnhandled(ctx context.Context) (int, error) {
	dead, err := o.q.listDead(ctx, maxUnhandledScan)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range dead {
		if m.LastError != errMissingHandler.Error() || o.handler(m.Topic) == nil {
			continue
		}
		if err := o.q.retry(ctx, m.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// Purge deletes done messages older than Config.Retention and dead ones older than Config.DeadRetention.
func (o *Outbox) Purge(ctx context.Context) (int64, error) { return o.q.purge(ctx) }

// Stats counts pending and dead messages per adapter.
func (o *Outbox) Stats(ctx context.Context) (map[string]Counts, error) { return o.q.stats(ctx) }
