package proxyadapter

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// listen opens a TCP listener on 127.0.0.1:0 closed with the test.
func listen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func serve(t *testing.T, l net.Listener, h func(net.Conn)) {
	t.Helper()
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(func() { _ = l.Close() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { defer c.Close(); h(c) })
		}
	})
}

// echoServer answers each line with "echo: <line>".
func echoServer(t *testing.T) (string, int) {
	t.Helper()
	l := listen(t)
	serve(t, l, func(c net.Conn) {
		s := bufio.NewScanner(c)
		for s.Scan() {
			fmt.Fprintf(c, "echo: %s\n", s.Text())
		}
	})
	a := l.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

// pipe relays until either side ends, then closes both.
func pipe(c net.Conn, from io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, from); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
	_ = c.Close()
	_ = up.Close()
	<-done
}

type seen struct {
	mu     sync.Mutex
	auth   []string
	target []string
	atyp   []byte
}

func (s *seen) add(auth, target string, atyp byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth, s.target, s.atyp = append(s.auth, auth), append(s.target, target), append(s.atyp, atyp)
}

// connectProxy is an upstream HTTP CONNECT proxy requiring user:pass; "echo.test" targets go to 127.0.0.1.
func connectProxy(t *testing.T, s *seen) protocol.NetworkProxy {
	t.Helper()
	l := listen(t)
	serve(t, l, func(c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		auth := req.Header.Get("Proxy-Authorization")
		s.add(auth, req.RequestURI, 0)
		if auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pa:ss")) {
			_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
			return
		}
		up, err := net.Dial("tcp", strings.Replace(req.RequestURI, "echo.test", "127.0.0.1", 1))
		if err != nil {
			_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}
		defer up.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
		pipe(c, br, up)
	})
	return protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port,
		Username: "user", Password: "pa:ss"}
}

// socksProxy is an upstream SOCKS5 proxy requiring user/pass; it resolves names itself ("echo.test").
func socksProxy(t *testing.T, s *seen) protocol.NetworkProxy {
	t.Helper()
	l := listen(t)
	serve(t, l, func(c net.Conn) {
		var g [2]byte
		if _, err := io.ReadFull(c, g[:]); err != nil {
			return
		}
		ms := make([]byte, g[1])
		_, _ = io.ReadFull(c, ms)
		_, _ = c.Write([]byte{5, 2})
		var h [2]byte
		_, _ = io.ReadFull(c, h[:])
		u := make([]byte, h[1])
		_, _ = io.ReadFull(c, u)
		var pl [1]byte
		_, _ = io.ReadFull(c, pl[:])
		p := make([]byte, pl[0])
		_, _ = io.ReadFull(c, p)
		if string(u) != "user" || string(p) != "pa:ss" {
			_, _ = c.Write([]byte{1, 1})
			return
		}
		_, _ = c.Write([]byte{1, 0})
		var r [4]byte
		_, _ = io.ReadFull(c, r[:])
		var host string
		switch r[3] {
		case 1:
			b := make([]byte, 4)
			_, _ = io.ReadFull(c, b)
			host = net.IP(b).String()
		case 3:
			var n [1]byte
			_, _ = io.ReadFull(c, n[:])
			b := make([]byte, n[0])
			_, _ = io.ReadFull(c, b)
			host = string(b)
		}
		var pb [2]byte
		_, _ = io.ReadFull(c, pb[:])
		target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb[:]))))
		s.add("", target, r[3])
		up, err := net.Dial("tcp", strings.Replace(target, "echo.test", "127.0.0.1", 1))
		if err != nil {
			_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer up.Close()
		_, _ = c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		pipe(c, c, up)
	})
	return protocol.NetworkProxy{Scheme: "socks5", Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port,
		Username: "user", Password: "pa:ss"}
}

// start runs the adapter on a Unix socket and returns a dialer for it.
func start(t *testing.T, s *Server) func() net.Conn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "egress.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return func() net.Conn {
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		return c
	}
}

// tunnel sends CONNECT authority and returns the status line and the reader.
func tunnel(t *testing.T, c net.Conn, authority string) (int, *bufio.Reader) {
	t.Helper()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, br
}

func TestConnectThroughUpstream(t *testing.T) {
	t.Parallel()
	_, port := echoServer(t)
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			var s seen
			up := connectProxy(t, &s)
			if scheme == "socks5" {
				up = socksProxy(t, &s)
			}
			dial := start(t, &Server{Upstream: up, Allow: domain.Allowlist{Restricted: true,
				Rules: []domain.Rule{{Host: "echo.test", Ports: []int{port}}}}})
			c := dial()
			code, br := tunnel(t, c, net.JoinHostPort("echo.test", strconv.Itoa(port)))
			if code != http.StatusOK {
				t.Fatalf("CONNECT status %d", code)
			}
			fmt.Fprintln(c, "hello")
			line, err := br.ReadString('\n')
			if err != nil || line != "echo: hello\n" {
				t.Fatalf("tunnel read %q, %v", line, err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.target) != 1 || s.target[0] != "echo.test:"+strconv.Itoa(port) {
				t.Fatalf("upstream saw %v", s.target)
			}
			if scheme == "socks5" && s.atyp[0] != 3 {
				t.Fatalf("SOCKS5 address type %d, want 3 (a name: remote DNS)", s.atyp[0])
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	_, port := echoServer(t)
	var s seen
	up := connectProxy(t, &s)
	allow := domain.Allowlist{Restricted: true, Rules: []domain.Rule{{Host: "echo.test", Ports: []int{port}}}}
	bad := up
	bad.Password = "wrong"
	tests := []struct {
		name      string
		upstream  protocol.NetworkProxy
		authority string
		want      int
	}{
		{"destination not in the allowlist", up, "example.org:443", http.StatusForbidden},
		{"allowed host on another port", up, "echo.test:1", http.StatusForbidden},
		{"upstream rejects the credentials", bad, net.JoinHostPort("echo.test", strconv.Itoa(port)), http.StatusBadGateway},
		{"malformed authority", up, "echo.test", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dial := start(t, &Server{Upstream: tc.upstream, Allow: allow})
			if code, _ := tunnel(t, dial(), tc.authority); code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, target := range s.target {
		if strings.HasPrefix(target, "example.org") || target == "echo.test:1" {
			t.Fatalf("a refused destination reached the upstream proxy: %v", s.target)
		}
	}
}

// Even without an allowlist the adapter never opens the host's loopback, localhost names or link-local addresses
// (a proxy on the host would reach the host itself).
func TestReservedDestinationsAreRefused(t *testing.T) {
	t.Parallel()
	var s seen
	dial := start(t, &Server{Upstream: connectProxy(t, &s)})
	for _, a := range []string{"127.0.0.1:22", "localhost:22", "[::1]:22", "169.254.169.254:80"} {
		if code, _ := tunnel(t, dial(), a); code != http.StatusForbidden {
			t.Errorf("CONNECT %s: status %d", a, code)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.target) != 0 {
		t.Fatalf("reserved destinations reached the upstream proxy: %v", s.target)
	}
}

func TestOriginFormIsRefused(t *testing.T) {
	t.Parallel()
	var s seen
	dial := start(t, &Server{Upstream: connectProxy(t, &s)})
	c := dial()
	fmt.Fprint(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("response %v, %v", resp, err)
	}
}

func TestPlainHTTPThroughUpstream(t *testing.T) {
	t.Parallel()
	var gotProxyAuth string
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		_, _ = io.WriteString(w, "203.0.113.7")
	}))
	t.Cleanup(web.Close)
	wu, _ := url.Parse(web.URL)
	var s seen
	dial := start(t, &Server{Upstream: connectProxy(t, &s)})
	c := dial()
	fmt.Fprintf(c, "GET http://echo.test:%s/ip HTTP/1.1\r\nHost: echo.test:%s\r\nProxy-Authorization: Basic eDp5\r\n\r\n",
		wu.Port(), wu.Port())
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "203.0.113.7" {
		t.Fatalf("response %d %q", resp.StatusCode, body)
	}
	if gotProxyAuth != "" {
		t.Fatalf("the sandbox's Proxy-Authorization reached the destination: %q", gotProxyAuth)
	}
}

func TestServeClosesConnectionsOnCancel(t *testing.T) {
	t.Parallel()
	_, port := echoServer(t)
	var s seen
	path := filepath.Join(t.TempDir(), "egress.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = (&Server{Upstream: connectProxy(t, &s)}).Serve(ctx, l); close(done) }()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if code, _ := tunnel(t, c, net.JoinHostPort("echo.test", strconv.Itoa(port))); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancel with an open tunnel")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the tunnel stayed open after the adapter stopped")
	}
}

// Only the request head is bounded: a large plain-HTTP upload passes whole (review finding).
func TestPlainHTTPLargeBody(t *testing.T) {
	t.Parallel()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, n)
	}))
	t.Cleanup(web.Close)
	wu, _ := url.Parse(web.URL)
	var s seen
	dial := start(t, &Server{Upstream: connectProxy(t, &s)})
	c := dial()
	body := strings.Repeat("x", 300<<10)
	fmt.Fprintf(c, "POST http://echo.test:%s/up HTTP/1.1\r\nHost: echo.test:%s\r\nContent-Length: %d\r\n\r\n%s",
		wu.Port(), wu.Port(), len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != strconv.Itoa(len(body)) {
		t.Fatalf("response %d %q", resp.StatusCode, got)
	}
}
