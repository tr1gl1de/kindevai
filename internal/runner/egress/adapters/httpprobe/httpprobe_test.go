package httpprobe_test

import (
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
		w.WriteHeader(http.StatusOK)
		// Add before Hijack: until then httptest's Close waits for this handler, so the cleanup's wg.Wait cannot
		// start with a zero counter and race this Add.
		wg.Add(2)
		c, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			wg.Add(-2)
			_ = up.Close()
			return
		}
		go func() { defer wg.Done(); _, _ = io.Copy(up, c); _ = up.Close() }()
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
