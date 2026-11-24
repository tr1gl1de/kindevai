package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// ErrShuttingDown: the Control Plane is stopping and accepts no new runner sessions.
var ErrShuttingDown = errors.New("control plane is shutting down")

// DisconnectReason tells a live connection why the Control Plane closes it; the adapter maps it to a close code.
type DisconnectReason int

const (
	ReasonDisabled DisconnectReason = iota + 1 // the Owner disabled the runner
	ReasonRevoked                              // the Owner revoked the runner; it must not reconnect
	ReasonReplaced                             // a newer connection of the same runner took over
	ReasonShutdown                             // the Control Plane is stopping
)

func (r DisconnectReason) String() string {
	return map[DisconnectReason]string{ReasonDisabled: "disabled", ReasonRevoked: "revoked", ReasonReplaced: "replaced",
		ReasonShutdown: "shutdown"}[r]
}

// Disconnector closes a runner's live connection (the Hub); Service calls it after disable and revoke commit.
type Disconnector interface {
	Disconnect(runnerID string, reason DisconnectReason)
}

// Hub is the registry of live, authorized runner connections: at most one per runner. It lives in memory; after
// a restart every runner reconnects and authenticates again.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*hubEntry
	closed   bool
}

type hubEntry struct {
	connID string
	close  func(DisconnectReason)
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{sessions: map[string]*hubEntry{}} }

// attach registers a connection, closing an older one of the same runner. detach removes only this connection.
func (h *Hub) attach(runnerID, connID string, closeFn func(DisconnectReason)) (detach func(), err error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrShuttingDown
	}
	old := h.sessions[runnerID]
	e := &hubEntry{connID: connID, close: closeFn}
	h.sessions[runnerID] = e
	h.mu.Unlock()
	if old != nil {
		old.close(ReasonReplaced)
	}
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.sessions[runnerID] == e {
			delete(h.sessions, runnerID)
		}
	}, nil
}

// Disconnect closes the runner's live connection, if any.
func (h *Hub) Disconnect(runnerID string, reason DisconnectReason) {
	h.mu.Lock()
	e := h.sessions[runnerID]
	delete(h.sessions, runnerID)
	h.mu.Unlock()
	if e != nil {
		e.close(reason)
	}
}

// Online reports whether the runner holds an authorized connection.
func (h *Hub) Online(runnerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[runnerID]
	return ok
}

// Count is the number of live sessions.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// Shutdown closes every session and refuses new ones.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.closed = true
	all := h.sessions
	h.sessions = map[string]*hubEntry{}
	h.mu.Unlock()
	for _, e := range all {
		e.close(ReasonShutdown)
	}
}

// GatewayRunners is the runner store as the gateway needs it.
type GatewayRunners interface {
	Runner(ctx context.Context, id string) (domain.Runner, error) // domain.ErrNotFound
	MarkSeen(ctx context.Context, id string, at time.Time) error
}

// Gateway authenticates runner connections by Ed25519 challenge-response (KIN-19).
type Gateway struct {
	Runners GatewayRunners
	Hub     *Hub
	Rand    io.Reader
	Now     func() time.Time
	NewID   func() string
	Logger  *slog.Logger
}

// Credentials are the runner's answer to a challenge.
type Credentials struct {
	RunnerID    string
	Fingerprint string // optional public key reference; must match the stored key when given
	Signature   []byte
}

// Session is an authorized connection. Detach must be called when the connection ends.
type Session struct {
	ID       string
	RunnerID string
	Detach   func()
}

func (g *Gateway) logger() *slog.Logger {
	if g.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return g.Logger
}

// NewChallenge starts the handshake of a new connection for the given audience (the Control Plane origin).
func (g *Gateway) NewChallenge(audience string) (*domain.Challenge, error) {
	return domain.NewChallenge(g.Rand, g.NewID(), audience, g.Now())
}

// Authenticate spends the challenge, verifies the signature against the runner's enrolled key and admits only an
// approved runner. The session is registered before the status is checked a second time, so a disable or revoke
// that commits concurrently either finds the session and closes it or is seen by the second check.
func (g *Gateway) Authenticate(ctx context.Context, ch *domain.Challenge, c Credentials, closeFn func(DisconnectReason)) (Session, error) {
	if err := ch.Use(g.Now()); err != nil {
		return Session{}, err
	}
	r, err := g.Runners.Runner(ctx, c.RunnerID)
	if errors.Is(err, domain.ErrNotFound) {
		return Session{}, domain.ErrBadSignature
	}
	if err != nil {
		return Session{}, fmt.Errorf("loading runner: %w", err)
	}
	msg, err := protocol.AuthTranscript(protocol.AuthFields{RunnerID: c.RunnerID, Audience: ch.Audience,
		Nonce: base64.RawURLEncoding.EncodeToString(ch.Nonce), ExpiresAt: ch.ExpiresAt.Unix(), ConnectionID: ch.ConnectionID})
	if err != nil || !r.Verify(msg, c.Signature) || (c.Fingerprint != "" && c.Fingerprint != r.Fingerprint) {
		return Session{}, domain.ErrBadSignature
	}
	if err := r.CheckConnect(); err != nil {
		return Session{}, err
	}
	detach, err := g.Hub.attach(r.ID, ch.ConnectionID, closeFn)
	if err != nil {
		return Session{}, err
	}
	if r, err = g.Runners.Runner(ctx, c.RunnerID); err != nil || r.CheckConnect() != nil {
		detach()
		if err != nil {
			return Session{}, fmt.Errorf("reloading runner: %w", err)
		}
		return Session{}, r.CheckConnect()
	}
	if err := g.Runners.MarkSeen(ctx, r.ID, g.Now()); err != nil {
		g.logger().Warn("recording runner last seen", "runner", r.ID, "err", err)
	}
	return Session{ID: ch.ConnectionID, RunnerID: r.ID, Detach: detach}, nil
}
