//go:build integration && linux

// Real sandboxes: user, network and mount namespaces plus nftables, built without root. Run with
// `make test-integration` (or `go test -tags=integration ./internal/runner/egress/adapters/nsbox/`) on Linux or
// WSL2; the tests skip when this user may not create namespaces (e.g. Ubuntu's
// kernel.apparmor_restrict_unprivileged_userns=1: `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`)
// or nft is not installed.
package nsbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// The test binary is also the helper (KINDEV_NSBOX_HELPER) and the agent (KINDEV_NSBOX_AGENT).
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("KINDEV_NSBOX_HELPER") == "1":
		os.Exit(Main(os.Args[1:], os.Stderr))
	case os.Getenv("KINDEV_NSBOX_AGENT") == "1":
		os.Exit(agentMain())
	}
	os.Exit(m.Run())
}

// agentMain runs inside the sandbox and prints one line per check.
func agentMain() int {
	fail := 0
	check := func(name string, ok bool, detail string) {
		state := "ok"
		if !ok {
			state, fail = "FAIL", 1
		}
		fmt.Printf("%s %s %s\n", state, name, detail)
	}
	st, _ := os.ReadFile("/proc/self/status")
	capEff := ""
	for l := range strings.SplitSeq(string(st), "\n") {
		if v, ok := strings.CutPrefix(l, "CapEff:"); ok {
			capEff = strings.TrimSpace(v)
		}
	}
	check("no-capabilities", capEff == "0000000000000000", capEff)
	_, err := net.DialTimeout("tcp", "1.1.1.1:443", 3*time.Second)
	check("direct-tcp-blocked", err != nil, "")
	_, err = net.DialTimeout("unix", os.Getenv("HOST_SOCKET"), time.Second)
	check("host-socket-hidden", err != nil, "")
	// /run is empty but for the path to this attempt's own adapter socket (when it lives in XDG_RUNTIME_DIR).
	var visible []string
	_ = filepath.WalkDir("/run", func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && !strings.Contains(p, "/kindev-egress-") {
			visible = append(visible, p)
		}
		return nil
	})
	check("run-hidden", len(visible) == 0, strings.Join(visible, ","))
	// Host processes are invisible: their /proc/<pid>/root would lead to the host's /run.
	_, err = os.Stat("/proc/" + os.Getenv("HOST_PID") + "/root")
	check("host-processes-hidden", err != nil, "")
	c := &http.Client{Timeout: 10 * time.Second} // honours HTTP_PROXY
	resp, err := c.Get(os.Getenv("DIAG_URL"))
	body := ""
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		body = string(b)
	}
	check("through-proxy", err == nil && body == "203.0.113.7", fmt.Sprint(body, err))
	check("worktree-writable", os.WriteFile("agent-was-here", []byte("x"), 0o600) == nil, "")
	return fail
}

func requireSandbox(t *testing.T, m *Manager) {
	t.Helper()
	if !namespacesAvailable() {
		t.Skip("this user may not create user/network namespaces")
	}
	if _, err := m.nft(); err != nil {
		t.Skip("nft is not installed")
	}
}

// upstream is an HTTP CONNECT proxy on the host (outside any sandbox).
func upstream(t *testing.T) protocol.NetworkProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(func() { _ = l.Close() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				// diag.test is the diagnostic server (a name: loopback targets would bypass HTTP_PROXY).
				up, err := net.Dial("tcp", strings.Replace(req.RequestURI, "diag.test", "127.0.0.1", 1))
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			})
		}
	})
	return protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port}
}

type fixture struct {
	m       *Manager
	dir     string
	workdir string
	diag    string
	socket  string
	proxy   protocol.NetworkProxy
	last    *Box
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	data := t.TempDir()
	m := &Manager{Program: os.Args[0], Env: []string{"KINDEV_NSBOX_HELPER=1"}, DataDir: data,
		VerifyTimeout: 60 * time.Second}
	requireSandbox(t, m)
	diag := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "203.0.113.7")
	}))
	t.Cleanup(diag.Close)
	f := &fixture{m: m, dir: filepath.Join(data, "attempts", "s-1"), workdir: filepath.Join(data, "workspaces", "w"),
		diag: strings.Replace(diag.URL, "127.0.0.1", "diag.test", 1) + "/ip", proxy: upstream(t)}
	for _, d := range []string{f.dir, f.workdir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A host socket (e.g. a Docker daemon) the sandbox must not reach.
	f.socket = filepath.Join(t.TempDir(), "daemon.sock")
	l, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	m.Sockets = []string{f.socket}
	return f
}

func (f *fixture) profile(expected string) protocol.NetworkProfile {
	px := f.proxy
	return protocol.NetworkProfile{Enforcement: "strict", DNS: "proxy", FailClosed: true, Proxy: &px,
		ExpectedIPv4: expected, DiagnosticURL: f.diag}
}

// run starts the test binary as the agent in a sandbox and returns the verdict error, output and exit error.
func (f *fixture) run(t *testing.T, p protocol.NetworkProfile) (error, string, error) {
	t.Helper()
	box, err := f.m.Open(t.Context(), egress.BoxRequest{Dir: f.dir, Workdir: f.workdir, Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	b := box.(*Box)
	f.last = b
	t.Cleanup(b.Release)
	// The test binary lives under /tmp, which the sandbox hides: run a copy from the kept state directory.
	agent := filepath.Join(f.dir, "agent")
	bin, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, err := b.Command([]string{agent}, []string{"KINDEV_NSBOX_AGENT=1", "HOST_SOCKET=" + f.socket,
		"DIAG_URL=" + f.diag, "HOST_PID=" + strconv.Itoa(os.Getpid()), "SSH_AUTH_SOCK=/tmp/agent.sock"}, f.workdir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	verr := b.Await(ctx)
	werr := cmd.Wait()
	return verr, out.String(), werr
}

func TestSandboxVerifiesThenConfinesTheAgent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	verr, out, werr := f.run(t, f.profile("203.0.113.7"))
	if verr != nil {
		t.Fatalf("verdict: %v\n%s", verr, out)
	}
	if werr != nil || strings.Contains(out, "FAIL") {
		t.Fatalf("agent: %v\n%s", werr, out)
	}
	for _, want := range []string{"ok no-capabilities", "ok direct-tcp-blocked", "ok host-socket-hidden", "ok run-hidden",
		"ok through-proxy", "ok host-processes-hidden", "ok worktree-writable"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(f.workdir, "agent-was-here")); err != nil {
		t.Fatalf("the agent's write did not reach the real worktree: %v", err)
	}
	b := f.last
	if _, err := os.Stat(b.spec.Socket); err != nil {
		t.Fatalf("adapter socket gone before Release: %v", err)
	}
	b.Release()
	if _, err := os.Stat(filepath.Dir(b.spec.Socket)); !os.IsNotExist(err) {
		t.Fatalf("Release left the socket directory: %v", err)
	}
}

func TestSandboxFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(f *fixture, p *protocol.NetworkProfile)
		want string
	}{
		{"egress IP mismatch", func(_ *fixture, p *protocol.NetworkProfile) { p.ExpectedIPv4 = "198.51.100.1" }, "egress_ip_mismatch"},
		{"upstream proxy down", func(_ *fixture, p *protocol.NetworkProfile) {
			p.Proxy = &protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: 1}
		}, "proxy_check_failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			p := f.profile("203.0.113.7")
			tc.edit(f, &p)
			verr, out, werr := f.run(t, p)
			if !errors.Is(verr, ErrVerdict) || !strings.Contains(verr.Error(), tc.want) {
				t.Fatalf("verdict %v, want %s\n%s", verr, tc.want, out)
			}
			var ee interface{ ExitCode() int }
			if !errors.As(werr, &ee) || ee.ExitCode() != ExitUnverified {
				t.Fatalf("helper exit %v, want %d", werr, ExitUnverified)
			}
			if strings.Contains(out, "no-capabilities") {
				t.Fatalf("the agent ran without a verified sandbox:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(f.workdir, "agent-was-here")); err == nil {
				t.Fatal("the agent ran without a verified sandbox")
			}
		})
	}
}

func TestSelfTestReportsSupport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if ok, why := f.m.Support(t.Context()); !ok {
		t.Fatalf("Support = false: %s", why)
	}
	ents, err := os.ReadDir(f.m.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "egress-selftest-") {
			t.Fatalf("self-test left %s behind", e.Name())
		}
	}
}
