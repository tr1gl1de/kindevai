package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

// Challenge is the Control Plane's runner.challenge as the runner sees it.
type Challenge struct {
	Version      int
	Nonce        string
	ExpiresAt    int64
	Audience     string
	ConnectionID string
}

// Answer is the runner.authenticate the runner sends back.
type Answer struct {
	RunnerID    string
	Fingerprint string
	Signature   []byte
}

// Link is an authorized connection. Wait blocks until it ends and returns why (nil when ctx ended it).
// KIN-20/KIN-22 add sending heartbeats and receiving commands to it.
type Link interface {
	Wait(ctx context.Context) error
}

// Dialer opens the WebSocket to the Control Plane, hands the challenge to answer and returns once the
// connection is authorized. Implementations map transport failures and close codes onto domain errors.
type Dialer interface {
	Dial(ctx context.Context, server domain.ServerURL, answer func(Challenge) (Answer, error)) (Link, error)
}

// Connector keeps the runner connected: it reconnects with jittered exponential backoff and stops only on
// cancellation or a fatal error (revoked, TLS verification failure, unsupported protocol, foreign audience).
type Connector struct {
	Dialer Dialer
	Now    func() time.Time
	// Sleep waits d or until ctx ends (then it returns ctx.Err()).
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand returns a uniform number in [0, 1) for the jitter.
	Rand   func() float64
	Logger *slog.Logger
	// MinBackoff, MaxBackoff bound the reconnect delay (defaults 1s and 60s); a session that stayed up for
	// StableAfter (default 1m) resets it.
	MinBackoff, MaxBackoff, StableAfter time.Duration
	// Handlers run on every authorized link (KIN-22 command/event channel); a message no handler takes closes
	// the link as a protocol error.
	Handlers []LinkHandler
	// Halt stops every running attempt and returns once they ended (KIN-63, fail-closed): the Control Plane said
	// the runner was disabled (reason runner_disabled) or revoked (runner_revoked), so its attempts were taken
	// away; the runner does not wait for an execution.cancel it may never receive. It must return within a bound
	// (the executor's: grace + 5 s) even if ctx ends, so a revoked runner never exits leaving processes behind.
	// nil: nothing to stop.
	Halt func(reason string)
}

func (c *Connector) defaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if c.Rand == nil {
		c.Rand = rand.Float64 //nolint:gosec // G404: jitter, not a secret
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = time.Minute
	}
	if c.StableAfter <= 0 {
		c.StableAfter = time.Minute
	}
}

// Run connects as the enrolled runner until ctx ends (nil) or a fatal error occurs.
func (c *Connector) Run(ctx context.Context, e domain.Enrollment, id domain.Identity) error {
	c.defaults()
	answer := func(ch Challenge) (Answer, error) { return sign(e, id, ch) }
	b := domain.Backoff{Min: c.MinBackoff, Max: c.MaxBackoff}
	for {
		link, err := c.Dialer.Dial(ctx, e.Server, answer)
		if err == nil {
			start := c.Now()
			c.Logger.Info("connected to the Control Plane", "server", e.Server.String(), "runner", e.RunnerID)
			err = c.serve(ctx, link)
			if c.Now().Sub(start) >= c.StableAfter {
				b.Reset()
			}
			if err == nil && ctx.Err() == nil {
				err = errors.New("connection closed")
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		c.halt(err)
		if domain.Fatal(err) {
			return err
		}
		d := b.Next(c.Rand())
		c.Logger.Warn("Control Plane connection lost; reconnecting", "err", err, "retry_in", d.Round(time.Millisecond))
		if c.Sleep(ctx, d) != nil {
			return nil
		}
	}
}

// halt stops the attempts of a runner the Control Plane disabled or revoked.
func (c *Connector) halt(err error) {
	if c.Halt == nil {
		return
	}
	switch {
	case errors.Is(err, domain.ErrRunnerRevoked):
		c.Logger.Warn("runner revoked: stopping every running attempt")
		c.Halt(protocol.CancelRunnerRevoked)
	case errors.Is(err, domain.ErrNotApproved):
		c.Halt(protocol.CancelRunnerDisabled)
	}
}

// serve runs the handlers on the link until it ends.
func (c *Connector) serve(ctx context.Context, link Link) error {
	conn, ok := link.(Conn)
	if !ok || len(c.Handlers) == 0 {
		return link.Wait(ctx)
	}
	conn.SetHandler(func(ctx context.Context, env protocol.Envelope) error {
		for _, h := range c.Handlers {
			if ok, err := h.Handle(ctx, conn, env); ok {
				return err
			}
		}
		return fmt.Errorf("%w: unknown message type %q", ErrUnknownMessage, env.Type)
	})
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, h := range c.Handlers {
		wg.Go(func() {
			if err := h.Serve(lctx, conn); err != nil && lctx.Err() == nil {
				c.Logger.Warn("link handler stopped", "err", err)
			}
		})
	}
	err := link.Wait(ctx)
	cancel()
	wg.Wait()
	return err
}

// ErrUnknownMessage: the Control Plane sent a message type no handler takes.
var ErrUnknownMessage = errors.New("unknown message from the Control Plane")

// sign answers a challenge after checking it was issued by the configured Control Plane for this protocol.
func sign(e domain.Enrollment, id domain.Identity, ch Challenge) (Answer, error) {
	if ch.Version != protocol.ProtocolVersion {
		return Answer{}, fmt.Errorf("%w (challenge version %d)", domain.ErrUnsupportedProtocol, ch.Version)
	}
	if want := protocol.Audience(e.Server.TLS(), e.Server.Host()); ch.Audience != want {
		return Answer{}, fmt.Errorf("%w: expected %s", domain.ErrAudienceMismatch, want)
	}
	msg, err := protocol.AuthTranscript(protocol.AuthFields{RunnerID: e.RunnerID, Audience: ch.Audience, Nonce: ch.Nonce,
		ExpiresAt: ch.ExpiresAt, ConnectionID: ch.ConnectionID})
	if err != nil {
		return Answer{}, fmt.Errorf("%w: %w", domain.ErrAuthRejected, err)
	}
	return Answer{RunnerID: e.RunnerID, Fingerprint: id.Fingerprint(), Signature: id.Sign(msg)}, nil
}
