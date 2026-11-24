// Package messaging is the durable inbox/outbox of the Control Plane (shared kernel, KIN-15).
//
// Every call to an external system (YouTrack, GitHub, Telegram, runner commands) goes through it:
//
//   - Outbox: a use case calls Outbox.Enqueue inside its db.Transactor.InTx, so the message is stored atomically
//     with the domain change (rollback = no message). Outbox.Run delivers messages to the handler registered for
//     their topic with Outbox.Handle, outside any transaction, at least once: retries use exponential backoff
//     with jitter, and after Config.MaxAttempts (or a Permanent error) the message is "dead" until an operator
//     retries it. Every delivery carries a stable idempotency key (Delivery.Key) that the handler must pass to
//     the external system or use for a postcondition check, because a crash between the external call and
//     recording it redelivers the message with the same key.
//   - Inbox: a webhook/poller verifies the request, then calls Inbox.Receive and acknowledges (2xx) only after
//     it returned nil. Duplicates of (source, delivery id) are recognised and dropped. Inbox.Run processes events
//     asynchronously; the handler runs in the same transaction that marks the event done, so its database
//     effects are applied exactly once. Inbox handlers must not call external systems: enqueue to the outbox.
//   - Health tracks ok / degraded / circuit_open per adapter. Outbox deliveries feed it automatically; while a
//     circuit is open the adapter is not called (one probe after the cooldown). Pollers report via
//     Health.Success/Failure.
//
// Payloads are JSON without secrets (the outbox rejects secret-looking fields, the inbox redacts them) and are
// deleted after the retention period by Purge (Run calls it hourly). Handlers must not put secrets in errors:
// error texts are stored (truncated) and shown to Owners.
package messaging

import (
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Config tunes the dispatcher. Zero fields take the defaults documented on each field (see WithDefaults).
type Config struct {
	Logger *slog.Logger     // default discards
	Now    func() time.Time // default time.Now
	Jitter func() float64   // uniform in [0,1); default math/rand/v2
	// Workers is the number of concurrent outbox deliveries (default 2: deliveries wait on the network, and the
	// target is 1 vCPU). The inbox processes one event at a time because its handlers hold the write transaction.
	Workers         int
	PollInterval    time.Duration // fallback poll when nothing woke the dispatcher (default 1s)
	DeliveryTimeout time.Duration // per outbox delivery (default 30s)
	// Lease is how long a claimed message is hidden from other polls; a message whose worker died is retried
	// after it (default DeliveryTimeout + 30s, so a live delivery is never duplicated).
	Lease            time.Duration
	MaxAttempts      int           // attempts before a message is dead (default 10)
	BaseBackoff      time.Duration // wait after the first failure (default 1s), doubled per attempt
	MaxBackoff       time.Duration // cap of the backoff (default 15m)
	Retention        time.Duration // done messages are deleted after this (default 7 days)
	DeadRetention    time.Duration // dead messages are deleted after this (default 30 days)
	BreakerThreshold int           // consecutive failures that open an adapter's circuit (default 5)
	BreakerCooldown  time.Duration // how long the circuit stays open before a probe (default 1m)
}

// WithDefaults returns c with zero fields replaced by the defaults.
func (c Config) WithDefaults() Config {
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Jitter == nil {
		c.Jitter = rand.Float64 //nolint:gosec // G404: jitter spreads retries, it is not a secret
	}
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defInt(&c.Workers, 2)
	def(&c.PollInterval, time.Second)
	def(&c.DeliveryTimeout, 30*time.Second)
	def(&c.Lease, c.DeliveryTimeout+30*time.Second)
	defInt(&c.MaxAttempts, 10)
	def(&c.BaseBackoff, time.Second)
	def(&c.MaxBackoff, 15*time.Minute)
	def(&c.Retention, 7*24*time.Hour)
	def(&c.DeadRetention, 30*24*time.Hour)
	defInt(&c.BreakerThreshold, 5)
	def(&c.BreakerCooldown, time.Minute)
	return c
}

// Backoff is the wait after the given failed attempt (1-based): d = min(MaxBackoff, BaseBackoff·2^(attempt-1)),
// then "equal jitter" d/2 + rand·d/2, so retries of many messages spread out but never fire immediately.
func (c Config) Backoff(attempt int) time.Duration {
	c = c.WithDefaults()
	d := c.BaseBackoff
	for i := 1; i < attempt && d < c.MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, c.MaxBackoff)
	half := d / 2
	return half + time.Duration(c.Jitter()*float64(d-half))
}

// ErrInvalidMessage rejects a message that cannot be stored (missing fields, non-JSON or oversized payload,
// secret-looking fields in an outbox payload).
var ErrInvalidMessage = errors.New("invalid message")

// ErrNotFound means no dead message has the given id.
var ErrNotFound = errors.New("message not found")

// permanentError marks an error that retrying cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps an error that retrying cannot fix (e.g. HTTP 4xx other than 408/429): the message goes dead
// at once and the adapter's health is not affected.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
