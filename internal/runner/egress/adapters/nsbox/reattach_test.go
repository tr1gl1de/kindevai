//go:build linux

package nsbox

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// strictProfile allows nothing (every CONNECT is denied by the allowlist), so the adapter answers without an
// upstream proxy.
func strictProfile() protocol.NetworkProfile {
	return protocol.NetworkProfile{Enforcement: "strict", DNS: "proxy", Restricted: true,
		Proxy: &protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: 9}}
}

// deadAdapter leaves an adapter socket directory as a killed runner does: the socket file stays, nobody serves it.
func deadAdapter(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(socketBase(), "kindev-egress-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	sock := filepath.Join(d, "s")
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	return sock
}

func connect(t *testing.T, sock, authority string) int {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the adapter: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("adapter answer: %v", err)
	}
	return resp.StatusCode
}

// KIN-81: after a runner restart the adapter is served again on the same socket (the sandbox's forwarder holds
// that directory and dials the socket by name), with the profile's allowlist, and Release removes it.
//
// Not parallel: the dead socket's listener was in this process; a parallel test forking a sandbox helper would hold
// a copy of its fd (see TestSweepStale).
func TestReattachServesTheAdapterAgain(t *testing.T) {
	sock := deadAdapter(t)
	m := &Manager{MonitorEvery: -1}
	box, err := m.Reattach(t.Context(), egress.BoxRequest{Profile: strictProfile()}, sock)
	if err != nil {
		t.Fatal(err)
	}
	b := box.(*Box)
	defer b.Release()
	if b.Endpoint() != sock {
		t.Fatalf("Endpoint = %q, want %q", b.Endpoint(), sock)
	}
	if code := connect(t, sock, "example.org:443"); code != http.StatusForbidden {
		t.Fatalf("CONNECT outside the allowlist: %d, want 403", code)
	}
	if s := b.Summary(); !strings.Contains(s, "1 connection") || !strings.Contains(s, "example.org:443") {
		t.Fatalf("Summary = %q", s)
	}
	if _, err := b.Command([]string{"/bin/true"}, nil, "/"); err == nil {
		t.Fatal("a reattached sandbox started a new command")
	}
	b.Release()
	if _, err := os.Stat(filepath.Dir(sock)); !os.IsNotExist(err) {
		t.Fatalf("Release left the socket directory: %v", err)
	}
}

// Anything but this user's own private adapter directory, or a socket still served, or a profile that is not
// strict, is refused: the caller then stops the attempt (fail-closed).
func TestReattachRefuses(t *testing.T) {
	served := func(t *testing.T) string {
		d, err := os.MkdirTemp(socketBase(), "kindev-egress-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		l, err := net.Listen("unix", filepath.Join(d, "s"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		return filepath.Join(d, "s")
	}
	open := func(t *testing.T) string {
		sock := deadAdapter(t)
		if err := os.Chmod(filepath.Dir(sock), 0o755); err != nil {
			t.Fatal(err)
		}
		return sock
	}
	linked := func(t *testing.T) string {
		sock := deadAdapter(t)
		link := filepath.Dir(sock) + "-link"
		if err := os.Symlink(filepath.Dir(sock), link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		return filepath.Join(link, "s")
	}
	tests := []struct {
		name    string
		sock    func(t *testing.T) string
		profile protocol.NetworkProfile
	}{
		{"the socket is still served", served, strictProfile()},
		{"a directory others may read", open, strictProfile()},
		{"a symlinked directory", linked, strictProfile()},
		{"a path outside the socket directories", func(t *testing.T) string { return filepath.Join(t.TempDir(), "s") }, strictProfile()},
		{"a relative path", func(*testing.T) string { return "kindev-egress-x/s" }, strictProfile()},
		{"a directory that is gone", func(*testing.T) string { return filepath.Join(socketBase(), "kindev-egress-gone0", "s") }, strictProfile()},
		{"a profile that is not strict", deadAdapter, protocol.NetworkProfile{Enforcement: "monitor"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sock := tc.sock(t)
			if b, err := (&Manager{MonitorEvery: -1}).Reattach(t.Context(), egress.BoxRequest{Profile: tc.profile}, sock); err == nil {
				b.Release()
				t.Fatalf("Reattach(%s) succeeded", sock)
			}
		})
	}
}

type fakeProber struct {
	mu    sync.Mutex
	codes []string // one per check; the last one repeats
	calls int
}

func (p *fakeProber) Probe(context.Context, protocol.NetworkProfile, []protocol.NetworkEndpoint) protocol.NetworkTestResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	code := p.codes[min(p.calls, len(p.codes)-1)]
	p.calls++
	return protocol.NetworkTestResult{OK: code == "", Code: code}
}

// KIN-81: the egress is checked again while the attempt runs (at once after a reattach); a wrong egress IP cuts
// the adapter (new connections refused) and the sandbox reports the fault.
func TestMonitorCutsTheAdapterOnAnEgressIPMismatch(t *testing.T) {
	sock := deadAdapter(t)
	pr := &fakeProber{codes: []string{"", "egress_ip_mismatch"}}
	p := strictProfile()
	p.DiagnosticURL, p.ExpectedIPv4 = "https://diag.test/ip", "203.0.113.7"
	box, err := (&Manager{MonitorEvery: 10 * time.Millisecond, Prober: pr}).Reattach(t.Context(), egress.BoxRequest{Profile: p}, sock)
	if err != nil {
		t.Fatal(err)
	}
	b := box.(*Box)
	defer b.Release()
	select {
	case why := <-b.Fault():
		if why != "egress_ip_mismatch" {
			t.Fatalf("fault %q", why)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no fault after an egress IP mismatch")
	}
	if code := connect(t, sock, "example.org:443"); code != http.StatusServiceUnavailable {
		t.Fatalf("CONNECT after the cut: %d, want 503", code)
	}
}

// One failed check (the proxy briefly unreachable) does not cut a running attempt.
func TestMonitorToleratesOneFailedCheck(t *testing.T) {
	sock := deadAdapter(t)
	pr := &fakeProber{codes: []string{"proxy_unreachable", ""}}
	p := strictProfile()
	p.DiagnosticURL = "https://diag.test/ip"
	box, err := (&Manager{MonitorEvery: 5 * time.Millisecond, Prober: pr}).Reattach(t.Context(), egress.BoxRequest{Profile: p}, sock)
	if err != nil {
		t.Fatal(err)
	}
	b := box.(*Box)
	defer b.Release()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pr.mu.Lock()
		n := pr.calls
		pr.mu.Unlock()
		if n >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the monitor did not keep checking")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case why := <-b.Fault():
		t.Fatalf("fault %q after a single failed check", why)
	default:
	}
}
