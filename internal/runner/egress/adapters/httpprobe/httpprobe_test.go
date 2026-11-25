package httpprobe_test

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/httpprobe"
)

// connectProxy is a minimal HTTP CONNECT proxy requiring basic auth user:pass.
func connectProxy(t *testing.T) *httptest.Server {
	t.Helper()
	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pa ss"))
		if r.Header.Get("Proxy-Authorization") != want {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		up, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		// Add before Hijack: until then httptest's Close waits for this handler, so the cleanup's wg.Wait cannot
		// start with a zero counter and race this Add.
		wg.Add(2)
		// Hijack before answering and tunnel from the returned reader: after a WriteHeader, Hijack flushes the 200
		// before it stops the server's background read, which can then swallow the first byte of the client's
		// TLS ClientHello into that reader (KIN-84).
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			wg.Add(-2)
			_ = up.Close()
			return
		}
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			wg.Add(-2)
			_, _ = c.Close(), up.Close()
			return
		}
		go func() { defer wg.Done(); _, _ = io.Copy(up, brw.Reader); _ = up.Close() }()
		go func() { defer wg.Done(); _, _ = io.Copy(c, up); _ = c.Close() }()
	}))
	t.Cleanup(func() { srv.Close(); wg.Wait() })
	return srv
}

// droppingListener is a peer that accepts TCP connections and closes them at once without a byte, the way a dead
// local proxy behind a port relay (e.g. WSL2 localhost forwarding) looks. Unlike a closed port, it is held for the
// whole test, so no parallel listener can take the address over.
func droppingListener(t *testing.T) (string, int) {
	t.Helper()
	// Like httptest: under WSL2 mirrored networking the loopback port space is shared with Windows and an
	// ephemeral bind on 127.0.0.1 can fail with EADDRINUSE, so fall back to [::1].
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if ln, err = net.Listen("tcp", "[::1]:0"); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func hostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

type fixture struct {
	prober *httpprobe.Prober
	proxy  *protocol.NetworkProxy
	diag   string
	ep     protocol.NetworkEndpoint
}

func setup(t *testing.T) fixture {
	t.Helper()
	diag := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "203.0.113.7\n") }))
	t.Cleanup(diag.Close)
	ep := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(ep.Close)
	pool := x509.NewCertPool()
	pool.AddCert(diag.Certificate())
	pool.AddCert(ep.Certificate())
	px := connectProxy(t)
	h, p := hostPort(t, px.URL)
	return fixture{prober: &httpprobe.Prober{RootCAs: pool, RequestTimeout: 5 * time.Second},
		proxy: &protocol.NetworkProxy{Scheme: "http", Host: h, Port: p, Username: "user", Password: "pa ss"},
		diag:  diag.URL, ep: protocol.NetworkEndpoint{Name: "github", URL: ep.URL}}
}

func TestProbeThroughProxy(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.prober.Probe(t.Context(), protocol.NetworkProfile{Proxy: f.proxy, Enforcement: "strict", ExpectedIPv4: "203.0.113.7",
		DiagnosticURL: f.diag}, []protocol.NetworkEndpoint{f.ep})
	epHost, _ := hostPort(t, f.ep.URL) // httptest falls back to [::1] when it cannot bind 127.0.0.1
	if !r.OK || r.ObservedIPv4 != "203.0.113.7" || len(r.Checks) != 1 || !r.Checks[0].OK || r.Checks[0].Host != strings.Trim(epHost, "[]") {
		t.Fatalf("result %+v", r)
	}
}

func TestProbeFailures(t *testing.T) {
	t.Parallel()
	f := setup(t)
	ch, cp := droppingListener(t)

	bad := *f.proxy
	bad.Password = "wrong-s3cret"
	down := *f.proxy
	down.Host, down.Port = ch, cp
	cases := []struct {
		name string
		np   protocol.NetworkProfile
		eps  []protocol.NetworkEndpoint
		code string
	}{
		{"wrong password", protocol.NetworkProfile{Proxy: &bad, DiagnosticURL: f.diag}, nil, httpprobe.CodeProxyAuthFailed},
		{"proxy drops connections", protocol.NetworkProfile{Proxy: &down, DiagnosticURL: f.diag}, nil, httpprobe.CodeProxyUnreachable},
		// The proxy answers the CONNECT, then the endpoint behind it drops the tunnel: blame the endpoint.
		{"diagnostic endpoint drops connections", protocol.NetworkProfile{Proxy: f.proxy,
			DiagnosticURL: "https://" + net.JoinHostPort(ch, strconv.Itoa(cp))}, nil, httpprobe.CodeDiagnosticFailed},
		{"strict without proxy", protocol.NetworkProfile{Enforcement: "strict", DiagnosticURL: f.diag}, nil, httpprobe.CodeStrictNeedsProxy},
		{"ip mismatch", protocol.NetworkProfile{Proxy: f.proxy, ExpectedIPv4: "198.51.100.1", DiagnosticURL: f.diag}, nil, httpprobe.CodeIPMismatch},
		{"endpoint down", protocol.NetworkProfile{Proxy: f.proxy, DiagnosticURL: f.diag},
			[]protocol.NetworkEndpoint{f.ep, {Name: "youtrack", URL: "https://" + net.JoinHostPort(ch, strconv.Itoa(cp))}}, httpprobe.CodeEndpoint},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := f.prober.Probe(t.Context(), c.np, c.eps)
			if r.OK || r.Code != c.code {
				t.Fatalf("result %+v, want %s", r, c.code)
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "pa ss") {
				t.Fatalf("result leaks a password: %s", b)
			}
		})
	}
}

// pipelinedConn holds writes until the first read and sends them in one segment, then reads past the proxy's
// CONNECT response: the CONNECT request and the TLS ClientHello reach the proxy together, as a fast client's do
// when the proxy's 200 and the ClientHello cross.
type pipelinedConn struct {
	net.Conn
	pending []byte
	br      *bufio.Reader
	err     error
}

func (c *pipelinedConn) Write(b []byte) (int, error) {
	if c.br != nil {
		return c.Conn.Write(b)
	}
	c.pending = append(c.pending, b...)
	return len(b), nil
}

func (c *pipelinedConn) Read(b []byte) (int, error) {
	if c.br == nil {
		c.br = bufio.NewReader(c.Conn)
		if _, c.err = c.Conn.Write(c.pending); c.err == nil {
			var resp *http.Response
			if resp, c.err = http.ReadResponse(c.br, nil); c.err == nil && resp.StatusCode != http.StatusOK {
				c.err = fmt.Errorf("CONNECT: %s", resp.Status)
			}
		}
	}
	if c.err != nil {
		return 0, c.err
	}
	return c.br.Read(b)
}

// The test proxy is the fixture of every probe through a proxy: bytes that reach it together with the CONNECT
// belong to the tunnel and must not be lost (KIN-84: a swallowed first ClientHello byte failed the TLS handshake).
func TestConnectProxyTunnelsBytesSentWithTheConnect(t *testing.T) {
	t.Parallel()
	f := setup(t)
	target := strings.TrimPrefix(f.ep.URL, "https://")
	raw, err := net.Dial("tcp", net.JoinHostPort(f.proxy.Host, strconv.Itoa(f.proxy.Port)))
	if err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(f.proxy.Username + ":" + f.proxy.Password))
	pc := &pipelinedConn{Conn: raw, pending: fmt.Appendf(nil, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)}
	host, _ := hostPort(t, f.ep.URL)
	tc := tls.Client(pc, &tls.Config{RootCAs: f.prober.RootCAs, ServerName: strings.Trim(host, "[]"), MinVersion: tls.VersionTLS12})
	t.Cleanup(func() { _ = tc.Close() })
	if err := tc.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tc.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("TLS handshake through the proxy: %v", err)
	}
}
