package proxyadapter

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// KIN-81 (KB KIN-A-22 «Во время работы»): once the egress is no longer the verified one the adapter cuts the open
// tunnels and refuses new connections.
func TestCutDropsOpenTunnelsAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	_, port := echoServer(t)
	var s seen
	srv := &Server{Upstream: connectProxy(t, &s)}
	dial := start(t, srv)
	c := dial()
	code, br := tunnel(t, c, net.JoinHostPort("echo.test", strconv.Itoa(port)))
	if code != http.StatusOK {
		t.Fatalf("CONNECT status %d", code)
	}
	fmt.Fprintln(c, "before")
	if line, err := br.ReadString('\n'); err != nil || line != "echo: before\n" {
		t.Fatalf("tunnel read %q, %v", line, err)
	}
	srv.Cut()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintln(c, "after")
	if line, err := br.ReadString('\n'); err == nil {
		t.Fatalf("the open tunnel survived the cut: %q", line)
	}
	if code, _ := tunnel(t, dial(), net.JoinHostPort("echo.test", strconv.Itoa(port))); code != http.StatusServiceUnavailable {
		t.Fatalf("CONNECT after the cut: status %d, want 503", code)
	}
	if !srv.IsCut() {
		t.Fatal("IsCut = false after Cut")
	}
}

// Connections the allowlist refuses are counted, with the last destination, for the attempt's report.
func TestDeniedConnectionsAreCounted(t *testing.T) {
	t.Parallel()
	_, port := echoServer(t)
	var s seen
	srv := &Server{Upstream: connectProxy(t, &s), Allow: domain.Allowlist{Restricted: true,
		Rules: []domain.Rule{{Host: "echo.test", Ports: []int{port}}}}}
	dial := start(t, srv)
	if n, _ := srv.Denied(); n != 0 {
		t.Fatalf("Denied = %d before any request", n)
	}
	for _, a := range []string{"example.org:443", "127.0.0.1:22", net.JoinHostPort("echo.test", strconv.Itoa(port)), "evil.test:80"} {
		tunnel(t, dial(), a)
	}
	n, last := srv.Denied()
	if n != 3 || last != "evil.test:80" {
		t.Fatalf("Denied = %d, %q; want 3, evil.test:80", n, last)
	}
}
