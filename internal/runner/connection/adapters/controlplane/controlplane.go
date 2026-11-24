// Package controlplane is the runner's HTTP client for the Control Plane's public runner API. TLS certificates
// are always verified (there is no insecure mode), proxy environment variables are ignored (the management channel
// never goes through the AI egress proxy) and redirects are never followed, so the token cannot be re-sent elsewhere.
package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

// ErrUnexpected means the Control Plane answered with something the protocol does not define.
var ErrUnexpected = errors.New("unexpected response from the Control Plane")

const (
	maxResponse    = 64 << 10
	requestTimeout = 30 * time.Second
)

// A registration is printed to the operator's terminal and saved, so it must be plain, bounded text.
var (
	runnerIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	statusRe      = regexp.MustCompile(`^(pending|approved|disabled|revoked)$`)
	fingerprintRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{1,64}$`)
)

// Client calls the Control Plane.
type Client struct {
	rootCAs *x509.CertPool // nil = system roots; set only by tests
}

var _ app.ControlPlane = (*Client)(nil)

// New returns a client that trusts the system certificate roots.
func New() *Client { return &Client{} }

func (c *Client) http(server domain.ServerURL) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !server.TLS() {
		dialer.Control = loopbackOnly // plain http: the token must never leave the host, whatever "localhost" resolves to
	}
	tr := &http.Transport{
		Proxy:               nil, // never HTTP(S)_PROXY: the management route is direct
		DialContext:         dialer.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.rootCAs},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Enroll posts the one-time token and public key. The token is sent only in the JSON body.
func (c *Client) Enroll(ctx context.Context, server domain.ServerURL, req app.EnrollRequest) (app.EnrollResult, error) {
	body, err := json.Marshal(protocol.EnrollRequest{Token: req.Token.Reveal(), PublicKey: req.PublicKey,
		Hostname: req.Host.Hostname, OS: req.Host.OS, Arch: req.Host.Arch, Version: req.Host.Version})
	if err != nil {
		return app.EnrollResult{}, fmt.Errorf("encoding enrollment: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, server.Endpoint(protocol.EnrollPath), bytes.NewReader(body))
	if err != nil {
		return app.EnrollResult{}, fmt.Errorf("building enrollment request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hc := c.http(server)
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(hreq)
	if err != nil {
		return app.EnrollResult{}, fmt.Errorf("contacting the Control Plane at %s: %w", server, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return app.EnrollResult{}, fmt.Errorf("reading the Control Plane response: %w", err)
	}
	if len(raw) > maxResponse {
		return app.EnrollResult{}, fmt.Errorf("%w: response larger than %d bytes", ErrUnexpected, maxResponse)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var out protocol.EnrollResult
		if err := json.Unmarshal(raw, &out); err != nil || !runnerIDRe.MatchString(out.RunnerID) ||
			!statusRe.MatchString(out.Status) || !fingerprintRe.MatchString(out.Fingerprint) {
			return app.EnrollResult{}, fmt.Errorf("%w: HTTP %d without a runner registration", ErrUnexpected, resp.StatusCode)
		}
		return app.EnrollResult(out), nil
	}
	return app.EnrollResult{}, mapError(resp.StatusCode, raw)
}

// loopbackOnly refuses a connection to any non-loopback address.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("plain http dial: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("plain http only to a loopback address, not %s: %w", host, domain.ErrPlainHTTP)
	}
	return nil
}

func mapError(status int, raw []byte) error {
	var e protocol.Error
	_ = json.Unmarshal(raw, &e) // non-JSON bodies (a proxy's error page) fall through to ErrUnexpected
	switch e.Error {
	case protocol.ErrCodeTokenInvalid:
		return domain.ErrTokenInvalid
	case protocol.ErrCodeTokenUsed:
		return domain.ErrTokenUsed
	case protocol.ErrCodeTokenExpired:
		return domain.ErrTokenExpired
	case protocol.ErrCodeTokenRevoked:
		return domain.ErrTokenRevoked
	case protocol.ErrCodeKeyEnrolled:
		return domain.ErrKeyEnrolled
	case protocol.ErrCodeRateLimited:
		return domain.ErrRateLimited
	case protocol.ErrCodeValidation:
		return fmt.Errorf("%w: field %q: %s", domain.ErrRejected, printable(e.Field), printable(e.Message))
	}
	return fmt.Errorf("%w: HTTP %d %s", ErrUnexpected, status, printable(e.Error))
}

// printable bounds a server-provided string before it reaches the terminal.
func printable(s string) string {
	out := make([]rune, 0, 64)
	for _, r := range s {
		if len(out) == 64 {
			break
		}
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		out = append(out, r)
	}
	return string(out)
}
