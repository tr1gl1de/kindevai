// Package httpprobe runs the Test Network preflight (KIN-30) with net/http through the profile's proxy (HTTP
// CONNECT, HTTPS proxy or SOCKS5 with remote DNS): proxy reachability, the observed egress IP from the diagnostic
// endpoint and the reachability of each required endpoint. Errors become stable codes; no error text — which can
// echo a proxy address or a URL — leaves this package.
package httpprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Failure codes.
const (
	CodeProxyUnreachable  = "proxy_unreachable"
	CodeProxyAuthFailed   = "proxy_auth_failed"
	CodeStrictNeedsProxy  = "strict_requires_proxy"
	CodeDiagnosticFailed  = "diagnostic_failed"
	CodeDiagnosticInvalid = "diagnostic_invalid_response"
	CodeIPMismatch        = "egress_ip_mismatch"
	CodeIPUnverified      = "egress_ip_unverified"
	CodeEndpoint          = "endpoint_unreachable"
	CodeTimeout           = "timeout"
	CodeDNS               = "dns_failed"
	CodeTLS               = "tls_failed"
	CodeUnreachable       = "unreachable"
	CodeBadURL            = "bad_url"
)

// Prober implements the egress app's Prober.
type Prober struct {
	RootCAs        *x509.CertPool // nil: the system roots
	RequestTimeout time.Duration  // per request; default 15s
}

func (p *Prober) timeout() time.Duration {
	if p.RequestTimeout <= 0 {
		return 15 * time.Second
	}
	return p.RequestTimeout
}

func proxyURL(px *protocol.NetworkProxy) *url.URL {
	u := &url.URL{Scheme: px.Scheme, Host: net.JoinHostPort(px.Host, strconv.Itoa(px.Port))}
	switch {
	case px.Username != "" && px.Password != "":
		u.User = url.UserPassword(px.Username, px.Password)
	case px.Username != "":
		u.User = url.User(px.Username)
	}
	return u
}

// classify maps an error to a code.
func classify(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Proxy Authentication Required") || strings.Contains(msg, "username/password authentication failed"):
		return CodeProxyAuthFailed
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return CodeTimeout
	case strings.Contains(msg, "proxyconnect"):
		return CodeProxyUnreachable
	case errors.As(err, &dnsErr):
		return CodeDNS
	case errors.As(err, &certErr), errors.As(err, &unknownAuth), strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return CodeTLS
	}
	return CodeUnreachable
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Probe runs the preflight.
func (p *Prober) Probe(ctx context.Context, np protocol.NetworkProfile, eps []protocol.NetworkEndpoint) protocol.NetworkTestResult {
	var r protocol.NetworkTestResult
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var pu *url.URL
	if np.Proxy != nil {
		pu = proxyURL(np.Proxy)
		c, err := dialer.DialContext(ctx, "tcp", pu.Host)
		if err != nil {
			r.Code = CodeProxyUnreachable
			if isTimeout(err) {
				r.Detail = "the proxy did not answer in time"
			}
			return r
		}
		_ = c.Close()
	} else if np.Enforcement == "strict" {
		r.Code = CodeStrictNeedsProxy
		return r
	}
	tr := &http.Transport{
		Proxy:                 func(*http.Request) (*url.URL, error) { return pu, nil },
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: p.timeout(),
		DisableKeepAlives:     true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: p.timeout(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	ip, code, detail := p.observe(ctx, client, np.DiagnosticURL)
	if code != "" {
		r.Code, r.Detail = code, detail
	} else if ip.Is4() {
		r.ObservedIPv4 = ip.String()
	} else {
		r.ObservedIPv6 = ip.String()
	}
	r.Checks = p.checks(ctx, client, eps)
	switch {
	case r.Code != "":
	case ipVerdict(np, r) != "":
		r.Code = ipVerdict(np, r)
	default:
		for _, c := range r.Checks {
			if !c.OK {
				r.Code = CodeEndpoint
				break
			}
		}
	}
	r.OK = r.Code == ""
	return r
}

func (p *Prober) observe(ctx context.Context, client *http.Client, diag string) (netip.Addr, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, diag, nil)
	if err != nil {
		return netip.Addr{}, CodeDiagnosticFailed, "invalid diagnostic endpoint"
	}
	resp, err := client.Do(req)
	if err != nil {
		c := classify(err)
		if c == CodeUnreachable || c == CodeDNS || c == CodeTLS {
			return netip.Addr{}, CodeDiagnosticFailed, "diagnostic endpoint: " + c
		}
		return netip.Addr{}, c, ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, CodeDiagnosticFailed, fmt.Sprintf("diagnostic endpoint answered HTTP %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, CodeDiagnosticInvalid, ""
	}
	return ip.Unmap(), "", ""
}

func (p *Prober) checks(ctx context.Context, client *http.Client, eps []protocol.NetworkEndpoint) []protocol.NetworkCheck {
	if len(eps) > protocol.MaxNetworkEndpoints {
		eps = eps[:protocol.MaxNetworkEndpoints]
	}
	out := make([]protocol.NetworkCheck, len(eps))
	var wg sync.WaitGroup
	for i, e := range eps {
		wg.Go(func() { out[i] = p.check(ctx, client, e) })
	}
	wg.Wait()
	return out
}

// check: any HTTP response means the endpoint is reachable through the proxy (status codes are the provider's).
func (p *Prober) check(ctx context.Context, client *http.Client, e protocol.NetworkEndpoint) protocol.NetworkCheck {
	c := protocol.NetworkCheck{Name: e.Name}
	u, err := url.Parse(e.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		c.Code = CodeBadURL
		return c
	}
	c.Host = u.Hostname()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}).String(), nil)
	if err != nil {
		c.Code = CodeBadURL
		return c
	}
	start := time.Now()
	resp, err := client.Do(req)
	c.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		c.Code = classify(err)
		return c
	}
	_ = resp.Body.Close()
	c.OK = true
	return c
}

// ipVerdict mirrors the Control Plane's rule (network domain IPVerdict): a diagnostic request observes one address
// family; only that family is compared, and a test that could observe no expected family is unverified.
func ipVerdict(np protocol.NetworkProfile, r protocol.NetworkTestResult) string {
	e4, e6, o4, o6 := np.ExpectedIPv4, np.ExpectedIPv6, r.ObservedIPv4, r.ObservedIPv6
	if (e4 != "" && o4 != "" && o4 != e4) || (e6 != "" && o6 != "" && o6 != e6) {
		return CodeIPMismatch
	}
	if (e4 != "" || e6 != "") && !(e4 != "" && o4 == e4) && !(e6 != "" && o6 == e6) {
		return CodeIPUnverified
	}
	return ""
}
