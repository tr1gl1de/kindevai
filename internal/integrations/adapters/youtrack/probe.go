// Package youtrack is the YouTrack REST adapter of the integrations context. For now it only tests a connection
// (KIN-26) and reads issues for the intake (KIN-27). It runs from outbox deliveries and the poller, never from
// HTTP handlers.
package youtrack

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// maxResponseBytes bounds what is read from YouTrack for a connection test.
const maxResponseBytes = 64 << 10

// Probe tests a connection by reading the token owner's profile (GET /api/users/me).
type Probe struct {
	Client  *http.Client  // default: a client with Timeout
	Timeout time.Duration // per probe, default 15s
}

func (p *Probe) client() *http.Client {
	c := http.Client{}
	if p.Client != nil {
		c = *p.Client
	}
	// Redirects are not followed: the token must only ever go to the configured base URL.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// networkDetail names the kind of network failure with a fixed phrase: raw dial errors would reveal internal
// addresses and ports.
func networkDetail(err error) string {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var hdr tls.RecordHeaderError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return "timeout"
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &cert) || errors.As(err, &hdr):
		return "TLS error"
	default:
		return "connection failed"
	}
}

// Probe implements app.YouTrackProbe. Results carry a code and a short detail (status code, login or the network
// error), never the token or a response body.
func (p *Probe) Probe(ctx context.Context, baseURL string, token domain.Secret) domain.CheckResult {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/users/me?fields=login", nil)
	if err != nil {
		return domain.CheckResult{Code: "bad_url"}
	}
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return domain.CheckResult{Code: "unreachable", Detail: networkDetail(err)}
	}
	defer resp.Body.Close()
	status := fmt.Sprintf("HTTP %d", resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return domain.CheckResult{Code: "unauthorized", Detail: status}
	case resp.StatusCode == http.StatusNotFound:
		return domain.CheckResult{Code: "not_found", Detail: status}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return domain.CheckResult{Code: "redirected", Detail: status}
	case resp.StatusCode != http.StatusOK:
		return domain.CheckResult{Code: "unexpected_status", Detail: status}
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&me); err != nil || me.Login == "" {
		return domain.CheckResult{Code: "bad_response"}
	}
	return domain.CheckResult{OK: true, Detail: me.Login}
}
