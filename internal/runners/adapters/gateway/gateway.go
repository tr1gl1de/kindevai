// Package gateway is the Control Plane end of the runner WebSocket (GET /runner/connect, KIN-19): transport
// checks, the challenge-response handshake and the authorized session loop with keepalive. Runners are machines:
// the dashboard session middleware does not apply; the Ed25519 challenge-response is the only authentication.
//
// Library: github.com/coder/websocket (the stdlib has no WebSocket server). It is small, context-based, has no
// dependencies, supports read limits, ping/pong and close codes, and works with net/http's Hijacker.
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Config bounds what runner connections may cost. Zero values take the defaults below. With the defaults the
// worst case is MaxConnections × (one MaxMessageBytes message + ~8 KiB buffers) ≈ 20 MiB.
type Config struct {
	MaxConnections int           // open WebSocket connections, authorized or not (default 256)
	MaxHandshakes  int           // connections still authenticating (default 32)
	AuthTimeout    time.Duration // time to answer the challenge (default 10s)
	PingInterval   time.Duration // keepalive ping period (default 30s)
	PongTimeout    time.Duration // a ping not answered in time closes the connection (default 15s)
	WriteTimeout   time.Duration // per message (default 10s)
	// PlainPeers are TCP peers treated like loopback for plain ws:// (e.g. Docker's gateway when the port is
	// published on the host's 127.0.0.1 only). Everything else must arrive over TLS.
	PlainPeers []netip.Prefix
}

func (c Config) withDefaults() Config {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = 256
	}
	if c.MaxHandshakes <= 0 {
		c.MaxHandshakes = 32
	}
	def(&c.AuthTimeout, 10*time.Second)
	def(&c.PingInterval, 30*time.Second)
	def(&c.PongTimeout, 15*time.Second)
	def(&c.WriteTimeout, 10*time.Second)
	return c
}

// MessageFunc handles one message type of an authorized session (KIN-20 heartbeat, KIN-22 commands). An error
// closes the connection with CloseProtocolError.
type MessageFunc func(ctx context.Context, s app.Session, env protocol.Envelope) error

// Handler serves ConnectPath.
type Handler struct {
	Gateway  *app.Gateway
	Limiter  *ratelimit.Limiter // connection attempts per client address
	Config   Config
	Messages map[string]MessageFunc // message types accepted after authorization; anything else closes 4400
	Logger   *slog.Logger

	once       sync.Once
	cfg        Config
	conns      chan struct{}
	handshakes chan struct{}
}

// Register mounts the endpoint.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+protocol.ConnectPath, h.connect)
}

func (h *Handler) init() {
	h.once.Do(func() {
		h.cfg = h.Config.withDefaults()
		h.conns = make(chan struct{}, h.cfg.MaxConnections)
		h.handshakes = make(chan struct{}, h.cfg.MaxHandshakes)
	})
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func acquire(sem chan struct{}) (release func(), ok bool) {
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return nil, false
	}
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	h.init()
	client := httpx.ClientIP(r)
	if h.Limiter != nil && !h.Limiter.Allow(client) {
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many connection attempts")
		return
	}
	if err := h.transportAllowed(r); err != nil {
		h.logger().Warn("runner connection refused", "client", client, "reason", err)
		httpx.WriteError(w, http.StatusForbidden, protocol.ErrCodePlainRemote, err.Error())
		return
	}
	if r.Header.Get("Origin") != "" {
		httpx.WriteError(w, http.StatusForbidden, protocol.ErrCodeBrowserNotAllowed, "browsers may not open runner connections")
		return
	}
	if !offers(r, protocol.Subprotocol) {
		httpx.WriteError(w, http.StatusUpgradeRequired, protocol.ErrCodeUnsupportedProto,
			"this Control Plane speaks the runner protocol "+protocol.Subprotocol+"; upgrade ai-runner")
		return
	}
	releaseConn, ok := acquire(h.conns)
	if !ok {
		httpx.WriteError(w, http.StatusServiceUnavailable, protocol.ErrCodeBusy, "too many runner connections")
		return
	}
	defer releaseConn()
	releaseHandshake, ok := acquire(h.handshakes)
	if !ok {
		httpx.WriteError(w, http.StatusServiceUnavailable, protocol.ErrCodeBusy, "too many runner handshakes")
		return
	}
	// The http.Server's read/write timeouts are deadlines on the TCP connection; they would kill a long-lived
	// WebSocket, so clear them. Liveness is the keepalive ping below instead.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{protocol.Subprotocol}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		releaseHandshake()
		h.logger().Warn("runner websocket upgrade failed", "client", client, "err", err)
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(protocol.MaxMessageBytes)

	sess, ok := h.handshake(r.Context(), c, protocol.Audience(secure(r), r.Host), client)
	releaseHandshake()
	if !ok {
		return
	}
	defer sess.Detach()
	h.logger().Info("runner connected", "runner", sess.RunnerID, "session", sess.ID, "client", client)
	err = h.serve(r.Context(), c, sess)
	h.logger().Info("runner disconnected", "runner", sess.RunnerID, "session", sess.ID, "reason", err)
}

func secure(r *http.Request) bool { return r.TLS != nil || httpx.ForwardedProto(r) == "https" }

var errPlainRemote = errors.New("plain ws:// is accepted only from this host (loopback); remote runners must " +
	"connect with wss:// through the HTTPS profile")

// transportAllowed: TLS (directly or terminated by a trusted proxy) from anywhere; plain ws:// only from the TCP
// peer loopback (or a configured local peer) and only when no proxy forwarded the request on someone's behalf.
func (h *Handler) transportAllowed(r *http.Request) error {
	if secure(r) {
		return nil
	}
	for _, hdr := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if r.Header.Get(hdr) != "" {
			return errPlainRemote
		}
	}
	peer := httpx.Peer(r)
	if peer.IsValid() && (peer.IsLoopback() || slices.ContainsFunc(h.Config.PlainPeers, func(p netip.Prefix) bool {
		return p.Contains(peer)
	})) {
		return nil
	}
	return errPlainRemote
}

func offers(r *http.Request, proto string) bool {
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(p) == proto {
				return true
			}
		}
	}
	return false
}

func (h *Handler) write(ctx context.Context, c *websocket.Conn, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", typ, err)
	}
	b, err := json.Marshal(protocol.Envelope{Type: typ, Payload: raw})
	if err != nil {
		return fmt.Errorf("encoding %s: %w", typ, err)
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

func closeWith(c *websocket.Conn, code int, reason string) {
	_ = c.Close(websocket.StatusCode(code), reason)
}

// read returns the next envelope; a binary frame or malformed JSON closes the connection with CloseProtocolError.
func read(ctx context.Context, c *websocket.Conn) (protocol.Envelope, bool) {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, false
	}
	var env protocol.Envelope
	if typ != websocket.MessageText || json.Unmarshal(data, &env) != nil || env.Type == "" {
		closeWith(c, protocol.CloseProtocolError, "invalid frame: one JSON envelope per text message")
		return protocol.Envelope{}, false
	}
	return env, true
}

func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (h *Handler) handshake(ctx context.Context, c *websocket.Conn, audience, client string) (app.Session, bool) {
	ch, err := h.Gateway.NewChallenge(audience)
	if err != nil {
		h.logger().Error("creating runner challenge", "err", err)
		closeWith(c, int(websocket.StatusInternalError), "internal error")
		return app.Session{}, false
	}
	if err := h.write(ctx, c, protocol.TypeChallenge, protocol.Challenge{Version: protocol.ProtocolVersion,
		Nonce: base64.RawURLEncoding.EncodeToString(ch.Nonce), ExpiresAt: ch.ExpiresAt.Unix(), Audience: ch.Audience,
		ConnectionID: ch.ConnectionID}); err != nil {
		return app.Session{}, false
	}
	// A Read whose context ends tears the connection down without a close frame, so the timeout closes it
	// explicitly with a code the runner can log.
	timer := time.AfterFunc(h.cfg.AuthTimeout, func() { closeWith(c, protocol.CloseAuthTimeout, "authentication timeout") })
	env, ok := read(ctx, c)
	timer.Stop()
	if !ok {
		return app.Session{}, false
	}
	var auth protocol.Authenticate
	if env.Type != protocol.TypeAuthenticate || strict(env.Payload, &auth) != nil {
		closeWith(c, protocol.CloseProtocolError, "expected "+protocol.TypeAuthenticate)
		return app.Session{}, false
	}
	if auth.Version != protocol.ProtocolVersion {
		closeWith(c, protocol.CloseUnsupportedVersion, "unsupported protocol version")
		return app.Session{}, false
	}
	sig, err := base64.StdEncoding.DecodeString(auth.Signature)
	if err != nil {
		sig = nil
	}
	closeFn := func(reason app.DisconnectReason) {
		code, text := disconnectCode(reason)
		go closeWith(c, code, text) // never block the caller (an Owner's revoke request)
	}
	sess, err := h.Gateway.Authenticate(ctx, ch, app.Credentials{RunnerID: auth.RunnerID, Fingerprint: auth.Fingerprint,
		Signature: sig}, closeFn)
	if err != nil {
		code, text := authCode(err)
		if code == int(websocket.StatusInternalError) {
			h.logger().Error("authenticating runner", "err", err)
		} else {
			h.logger().Warn("runner authentication rejected", "runner", printable(auth.RunnerID), "client", client, "reason", text)
		}
		closeWith(c, code, text)
		return app.Session{}, false
	}
	if err := h.write(ctx, c, protocol.TypeAuthorized, protocol.Authorized{RunnerID: sess.RunnerID, SessionID: sess.ID,
		Capabilities: []string{}}); err != nil {
		sess.Detach()
		return app.Session{}, false
	}
	return sess, true
}

func authCode(err error) (int, string) {
	switch {
	case errors.Is(err, domain.ErrRunnerRevoked):
		return protocol.CloseRevoked, "runner revoked"
	case errors.Is(err, domain.ErrNotApproved):
		return protocol.CloseNotApproved, "runner is not approved"
	case errors.Is(err, domain.ErrBadSignature), errors.Is(err, domain.ErrChallengeExpired), errors.Is(err, domain.ErrChallengeUsed):
		return protocol.CloseAuthFailed, "authentication failed"
	case errors.Is(err, app.ErrShuttingDown):
		return int(websocket.StatusGoingAway), "control plane is shutting down"
	}
	return int(websocket.StatusInternalError), "internal error"
}

func disconnectCode(r app.DisconnectReason) (int, string) {
	switch r {
	case app.ReasonRevoked:
		return protocol.CloseRevoked, "runner revoked"
	case app.ReasonDisabled:
		return protocol.CloseNotApproved, "runner disabled"
	case app.ReasonReplaced:
		return protocol.CloseReplaced, "replaced by a newer connection"
	}
	return int(websocket.StatusGoingAway), "control plane is shutting down"
}

// serve runs an authorized session until it closes: a keepalive pinger and the message loop.
func (h *Handler) serve(ctx context.Context, c *websocket.Conn, s app.Session) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.keepalive(ctx, c)
	for {
		env, ok := read(ctx, c)
		if !ok {
			return errors.New("connection closed")
		}
		fn := h.Messages[env.Type]
		if fn == nil {
			closeWith(c, protocol.CloseProtocolError, "unknown message type")
			return fmt.Errorf("unknown message type %q", printable(env.Type))
		}
		if err := fn(ctx, s, env); err != nil {
			closeWith(c, protocol.CloseProtocolError, "invalid "+env.Type)
			return err
		}
	}
}

func (h *Handler) keepalive(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(h.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, h.cfg.PongTimeout)
		err := c.Ping(pctx)
		cancel()
		if err != nil {
			_ = c.CloseNow()
			return
		}
	}
}

// printable bounds a runner-provided string before it reaches the log.
func printable(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}
