package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

var _ app.Dialer = (*Client)(nil)

// Keepalive of the runner side: a dead path (NAT timeout, sleeping laptop) is noticed within ~45s.
const (
	handshakeTimeout = 30 * time.Second
	pingInterval     = 30 * time.Second
	pongTimeout      = 15 * time.Second
	writeTimeout     = 10 * time.Second
)

// wsHTTP is the client for the WebSocket upgrade: the same trust rules as enrollment (verified TLS, no proxy from
// the environment, no redirects, plain http only to loopback) and HTTP/1.1, which the upgrade requires.
func (c *Client) wsHTTP(server domain.ServerURL) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !server.TLS() {
		dialer.Control = loopbackOnly
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // never HTTP(S)_PROXY: the management route is direct, not the AI egress proxy
			DialContext:         dialer.DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.rootCAs},
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   false,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Dial opens wss://<server>/runner/connect (ws:// only for loopback), answers the challenge and returns the
// authorized link.
func (c *Client) Dial(ctx context.Context, server domain.ServerURL, answer func(app.Challenge) (app.Answer, error)) (app.Link, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	hc := c.wsHTTP(server)
	conn, resp, err := websocket.Dial(hctx, server.Endpoint(protocol.ConnectPath), &websocket.DialOptions{
		HTTPClient: hc, Subprotocols: []string{protocol.Subprotocol}, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		hc.CloseIdleConnections()
		return nil, dialError(server, resp, err)
	}
	if conn.Subprotocol() != protocol.Subprotocol {
		_ = conn.Close(websocket.StatusProtocolError, "subprotocol required")
		return nil, domain.ErrUnsupportedProtocol
	}
	conn.SetReadLimit(protocol.MaxMessageBytes)
	if err := handshake(hctx, conn, answer); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return &link{conn: conn}, nil
}

func handshake(ctx context.Context, conn *websocket.Conn, answer func(app.Challenge) (app.Answer, error)) error {
	env, err := readEnvelope(ctx, conn)
	if err != nil {
		return err
	}
	var ch protocol.Challenge
	if env.Type != protocol.TypeChallenge || json.Unmarshal(env.Payload, &ch) != nil {
		_ = conn.Close(protocol.CloseProtocolError, "expected "+protocol.TypeChallenge)
		return fmt.Errorf("%w: expected %s", ErrUnexpected, protocol.TypeChallenge)
	}
	a, err := answer(app.Challenge(ch))
	if err != nil {
		return err
	}
	if err := writeEnvelope(ctx, conn, protocol.TypeAuthenticate, protocol.Authenticate{Version: protocol.ProtocolVersion,
		RunnerID: a.RunnerID, Fingerprint: a.Fingerprint, Signature: base64.StdEncoding.EncodeToString(a.Signature)}); err != nil {
		return closeError(err)
	}
	env, err = readEnvelope(ctx, conn)
	if err != nil {
		return err
	}
	if env.Type != protocol.TypeAuthorized {
		_ = conn.Close(protocol.CloseProtocolError, "expected "+protocol.TypeAuthorized)
		return fmt.Errorf("%w: expected %s", ErrUnexpected, protocol.TypeAuthorized)
	}
	return nil
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	typ, b, err := conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, closeError(err)
	}
	var env protocol.Envelope
	if typ != websocket.MessageText || json.Unmarshal(b, &env) != nil {
		_ = conn.Close(protocol.CloseProtocolError, "invalid frame")
		return protocol.Envelope{}, fmt.Errorf("%w: invalid frame", ErrUnexpected)
	}
	return env, nil
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	b, err := json.Marshal(protocol.Envelope{Type: typ, Payload: raw})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

// closeError maps the Control Plane's close code onto a domain error.
func closeError(err error) error {
	switch websocket.CloseStatus(err) {
	case protocol.CloseRevoked:
		return domain.ErrRunnerRevoked
	case protocol.CloseNotApproved:
		return domain.ErrNotApproved
	case protocol.CloseAuthFailed:
		return domain.ErrAuthRejected
	case protocol.CloseUnsupportedVersion:
		return domain.ErrUnsupportedProtocol
	case -1:
		return fmt.Errorf("control plane connection: %w", err)
	}
	return fmt.Errorf("control plane closed the connection: %w", err)
}

func dialError(server domain.ServerURL, resp *http.Response, err error) error {
	var verr *tls.CertificateVerificationError
	var uerr x509.UnknownAuthorityError
	var herr x509.HostnameError
	var cerr x509.CertificateInvalidError
	if errors.As(err, &verr) || errors.As(err, &uerr) || errors.As(err, &herr) || errors.As(err, &cerr) {
		return fmt.Errorf("%w: %s: %w", domain.ErrTLSVerification, server, err)
	}
	if errors.Is(err, domain.ErrPlainHTTP) {
		return err
	}
	if resp == nil {
		return fmt.Errorf("connecting to the Control Plane at %s: %w", server, err)
	}
	var body protocol.Error
	if resp.Body != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&body)
		_ = resp.Body.Close()
	}
	switch {
	case body.Error == protocol.ErrCodePlainRemote:
		return fmt.Errorf("%w (the Control Plane refused plain ws:// from this address)", domain.ErrPlainHTTP)
	case resp.StatusCode == http.StatusUpgradeRequired || body.Error == protocol.ErrCodeUnsupportedProto:
		return domain.ErrUnsupportedProtocol
	}
	return fmt.Errorf("%w: HTTP %d %s from %s", ErrUnexpected, resp.StatusCode, printable(body.Error), server)
}

// link is an authorized connection. Without a handler any message from the Control Plane is a protocol error.
type link struct {
	conn    *websocket.Conn
	handler func(ctx context.Context, env protocol.Envelope) error
}

var _ app.Conn = (*link)(nil)

// SetHandler sets the receiver of the Control Plane's messages; call it before Wait.
func (l *link) SetHandler(h func(ctx context.Context, env protocol.Envelope) error) { l.handler = h }

// Send writes one envelope; it is safe for concurrent use.
func (l *link) Send(ctx context.Context, typ string, payload any) error {
	return writeEnvelope(ctx, l.conn, typ, payload)
}

func (l *link) Wait(ctx context.Context) error {
	kctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()
	go keepalive(kctx, l.conn)
	go func() { // shutdown: a proper close frame instead of a torn connection
		select {
		case <-ctx.Done():
			_ = l.conn.Close(websocket.StatusGoingAway, "runner shutting down")
		case <-kctx.Done():
		}
	}()
	defer func() { _ = l.conn.CloseNow() }()
	for {
		typ, b, err := l.conn.Read(kctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return closeError(err)
		}
		var env protocol.Envelope
		if typ != websocket.MessageText || json.Unmarshal(b, &env) != nil || env.Type == "" {
			_ = l.conn.Close(protocol.CloseProtocolError, "invalid frame")
			return fmt.Errorf("%w: invalid frame", ErrUnexpected)
		}
		if l.handler == nil {
			_ = l.conn.Close(protocol.CloseProtocolError, "unknown message type")
			return fmt.Errorf("%w: unknown message type %q", ErrUnexpected, printable(env.Type))
		}
		if err := l.handler(ctx, env); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_ = l.conn.Close(protocol.CloseProtocolError, "invalid "+printable(env.Type))
			return fmt.Errorf("handling %s: %w", printable(env.Type), err)
		}
	}
}

func keepalive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pongTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil {
			_ = conn.CloseNow()
			return
		}
	}
}
