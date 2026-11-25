//go:build integration && linux

// Real sandboxes: user, network and mount namespaces plus nftables, built without root. Run with
// `make test-integration` (or `go test -tags=integration ./internal/runner/egress/adapters/nsbox/`) on Linux or
// WSL2; the tests skip when this user may not create namespaces (e.g. Ubuntu's
// kernel.apparmor_restrict_unprivileged_userns=1: `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`)
// or nft is not installed (CI sets KINDEV_REQUIRE_SANDBOX=1 to fail instead).
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
	"os/exec"
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
	case os.Getenv(RunEnv) != "": // the outer launcher of a wrapped run
		os.Exit(RunMain(os.Stderr))
	case os.Getenv("KINDEV_NSBOX_HELPER") == "1":
		os.Exit(Main(os.Args[1:], os.Stderr))
	case os.Getenv("KINDEV_NSBOX_AGENT") == "hang": // runs until a signal ends it
		fmt.Println("agent hanging")
		select {}
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
	sigIgn := ""
	for l := range strings.SplitSeq(string(st), "\n") {
		if v, ok := strings.CutPrefix(l, "SigIgn:"); ok {
			sigIgn = strings.TrimSpace(v)
		}
	}
	check("signals-not-ignored", sigIgn == "0000000000000000", sigIgn)
	fd, err := syscall.Socket(40 /* AF_VSOCK */, syscall.SOCK_STREAM, 0)
	if err == nil {
		_ = syscall.Close(fd)
	}
	check("vsock-refused", err != nil, fmt.Sprint(err))
	_, err = net.DialTimeout("unix", os.Getenv("SIBLING_SOCKET"), time.Second)
	check("sibling-adapter-hidden", err != nil, "")
	_, err = os.ReadFile(os.Getenv("RUNNER_SECRET"))
	check("runner-data-hidden", err != nil, "")
	_, err = os.ReadFile("/proc/1/environ") // the helper: not dumpable
	check("helper-not-readable", err != nil, "")
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
	if want := os.Getenv("STDIN_WANT"); want != "" {
		in, _ := io.ReadAll(os.Stdin)
		check("stdin-relayed", string(in) == want, string(in))
	}
	check("worktree-writable", os.WriteFile("agent-was-here", []byte("x"), 0o600) == nil, "")
	if home := os.Getenv("HOME"); os.Getenv("HOME_SECRET") != "" { // the filesystem view (KIN-80)
		_, err = os.ReadFile(os.Getenv("HOME_SECRET"))
		check("home-hidden", err != nil, fmt.Sprint(err))
		_, err = os.Stat(filepath.Join(home, ".claude", "history.jsonl"))
		check("home-history-hidden", err != nil, "")
		b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
		check("cli-login-copied", err == nil && string(b) == os.Getenv("CRED_WANT"), string(b))
		check("home-private", os.WriteFile(filepath.Join(home, "agent-note"), []byte("x"), 0o600) == nil, "")
		_, err = os.ReadFile(os.Getenv("RUNNER_KEY"))
		check("runner-key-hidden", err != nil, "")
		for _, d := range []string{"/", "/usr", "/etc", "/bin", filepath.Join(os.Getenv("MIRROR"), "objects")} {
			err := os.WriteFile(filepath.Join(d, "agent-was-here"), []byte("x"), 0o600)
			check("readonly "+d, err != nil, fmt.Sprint(err))
		}
		_, err = os.ReadFile(filepath.Join(os.Getenv("MIRROR"), "HEAD"))
		check("mirror-readable", err == nil, fmt.Sprint(err))
		check("admin-writable", os.WriteFile(filepath.Join(os.Getenv("ADMIN"), "index"), []byte("x"), 0o600) == nil, "")
		_, err = os.ReadDir(os.Getenv("OTHER_REPO"))
		check("other-repo-hidden", err != nil, "")
		_, err = os.Stat("/mnt/c")
		check("windows-drive-hidden", err != nil, "")
		_, err = os.ReadFile("/etc/passwd")
		check("etc-passwd-readable", err == nil, fmt.Sprint(err))
		_, err = os.Stat("/etc/hostname") // host configuration outside the allowlist
		check("etc-rest-hidden", err != nil, "")
		_, err = os.Stat(filepath.Join(os.Getenv("MIRROR"), "worktrees", "sibling"))
		check("sibling-worktree-hidden", err != nil, "")
		if r := os.Getenv("REFRESHED"); r != "" {
			_ = os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte(r), 0o600)
		}
	}
	return fail
}

// requireSandbox skips without namespaces or nft, unless KINDEV_REQUIRE_SANDBOX=1 (the CI job) makes that a
// failure.
func requireSandbox(t *testing.T, m *Manager) {
	t.Helper()
	skip := t.Skip
	if os.Getenv("KINDEV_REQUIRE_SANDBOX") == "1" {
		skip = t.Fatal
	}
	if !namespacesAvailable() {
		skip("this user may not create user/network namespaces")
	}
	if _, err := m.nft(); err != nil {
		skip("nft is not installed")
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
	sibling string // another attempt's adapter socket
	secret  string // a file in the runner's data directory
	home    string // the runner user's HOME (and the CLI's)
	key     string // the runner's Ed25519 key
	mirror  string // the worktree's repository mirror
	admin   string // the worktree's administrative directory in the mirror
	other   string // another project's mirror
}

const (
	credOrig      = `{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1}}`
	credRefreshed = `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2","expiresAt":2}}`
)

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
	sd, err := os.MkdirTemp(socketBase(), "kindev-egress-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sd) })
	f.sibling = filepath.Join(sd, "s")
	sl, err := net.Listen("unix", f.sibling)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sl.Close() })
	f.secret = filepath.Join(data, "network-config.json")
	if err := os.WriteFile(f.secret, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The runner user's HOME, with the CLI's login, an SSH key and history the agent must not see.
	f.home = filepath.Join(t.TempDir(), "home", "runner")
	m.Home = f.home
	f.key = filepath.Join(data, "keys", "ed25519.pem")
	f.mirror = filepath.Join(data, "repos", "o", "n.git")
	f.admin = filepath.Join(f.mirror, "worktrees", "w")
	f.other = filepath.Join(data, "repos", "x", "other.git")
	for p, c := range map[string]string{
		filepath.Join(f.home, ".ssh", "id_ed25519"):           "ssh key",
		filepath.Join(f.home, ".claude", ".credentials.json"): credOrig,
		filepath.Join(f.home, ".claude", "history.jsonl"):     "history",
		f.key:                           "runner key",
		filepath.Join(f.mirror, "HEAD"): "ref: refs/heads/main\n",
		filepath.Join(f.mirror, "objects", "pack", "p"):          "objects",
		filepath.Join(f.admin, "gitdir"):                         filepath.Join(f.workdir, ".git") + "\n",
		filepath.Join(f.workdir, ".git"):                         "gitdir: " + f.admin + "\n",
		filepath.Join(f.other, "HEAD"):                           "ref: refs/heads/main\n",
		filepath.Join(f.mirror, "worktrees", "sibling", "index"): "another attempt",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
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
		"DIAG_URL=" + f.diag, "HOST_PID=" + strconv.Itoa(os.Getpid()),
		"SIBLING_SOCKET=" + f.sibling, "RUNNER_SECRET=" + f.secret, "SSH_AUTH_SOCK=/tmp/agent.sock",
		"HOME=" + f.home, "HOME_SECRET=" + filepath.Join(f.home, ".ssh", "id_ed25519"), "CRED_WANT=" + credOrig,
		"REFRESHED=" + credRefreshed, "RUNNER_KEY=" + f.key, "MIRROR=" + f.mirror, "ADMIN=" + f.admin,
		"OTHER_REPO=" + f.other}, f.workdir)
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
		"ok through-proxy", "ok host-processes-hidden",
		"ok signals-not-ignored", "ok vsock-refused", "ok sibling-adapter-hidden", "ok runner-data-hidden",
		"ok helper-not-readable", "ok worktree-writable", "ok home-hidden", "ok home-history-hidden",
		"ok cli-login-copied", "ok home-private", "ok runner-key-hidden", "ok readonly /", "ok readonly /usr",
		"ok readonly /etc", "ok readonly " + filepath.Join(f.mirror, "objects"), "ok mirror-readable", "ok admin-writable",
		"ok other-repo-hidden", "ok windows-drive-hidden", "ok etc-passwd-readable", "ok etc-rest-hidden",
		"ok sibling-worktree-hidden"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// Nothing the agent wrote outside the worktree reached the host, not even its CLI login.
	if _, err := os.Stat(filepath.Join(f.home, "agent-note")); !os.IsNotExist(err) {
		t.Errorf("the agent's private HOME leaked to the host: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.mirror, "objects", "agent-was-here")); !os.IsNotExist(err) {
		t.Errorf("the agent wrote into the shared mirror: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.admin, "index")); string(b) != "x" {
		t.Errorf("the worktree's index was not writable: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(f.home, ".claude", ".credentials.json")); string(b) != credOrig {
		t.Errorf("the agent changed the host's CLI login: %s", b)
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

// agentCopy copies the test binary into the kept state directory (the sandbox hides /tmp, where it lives).
func (f *fixture) agentCopy(t *testing.T) string {
	t.Helper()
	agent := filepath.Join(f.dir, "agent")
	bin, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return agent
}

// The CLI adapters (KIN-32) start a plain command line: Wrap turns it into `sandbox-run`, which builds the same
// sandbox, relays stdin (the prompt) and the exit status, and never starts the CLI unverified.
func TestWrappedRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		expected string
		exit     int
	}{
		{"verified", "203.0.113.7", 0},
		{"egress IP mismatch", "198.51.100.1", ExitUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			agent := f.agentCopy(t)
			path, args, env, err := f.m.Wrap(egress.BoxRequest{Dir: f.dir, Workdir: f.workdir, Profile: f.profile(tc.expected)},
				[]string{agent}, []string{"KINDEV_NSBOX_AGENT=1", "HOST_SOCKET=" + f.socket, "DIAG_URL=" + f.diag,
					"HOST_PID=" + strconv.Itoa(os.Getpid()), "SIBLING_SOCKET=" + f.sibling, "RUNNER_SECRET=" + f.secret,
					"STDIN_WANT=the prompt"}, f.workdir)
			if err != nil {
				t.Fatal(err)
			}
			for _, kv := range env { // the credentials never travel in the environment
				if strings.Contains(kv, f.proxy.Password) && f.proxy.Password != "" || strings.Contains(kv, "\"argv\"") {
					t.Fatalf("run description in the environment: %s", kv)
				}
			}
			cmd := exec.Command(path, args...) //nolint:noctx // killed with its group on cleanup
			cmd.Env, cmd.Stdin = env, strings.NewReader("the prompt")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			_ = cmd.Wait()
			if code := cmd.ProcessState.ExitCode(); code != tc.exit {
				t.Fatalf("exit %d, want %d\n%s", code, tc.exit, out.String())
			}
			ran := strings.Contains(out.String(), "no-capabilities")
			if tc.exit == 0 && (!ran || strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "ok stdin-relayed")) {
				t.Fatalf("agent output:\n%s", out.String())
			}
			if tc.exit != 0 && (ran || !strings.Contains(out.String(), UnverifiedMarker)) {
				t.Fatalf("unverified run output:\n%s", out.String())
			}
			if left, _ := filepath.Glob(filepath.Join(f.dir, "sandbox-run-*.json")); len(left) != 0 {
				t.Fatalf("run description left behind: %v", left)
			}
		})
	}
}

// A cancel (SIGTERM to the process group) ends a wrapped run promptly whether it arrives while the sandbox is
// being built (the agent then never starts) or once the agent runs.
func TestWrappedRunCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent := f.agentCopy(t)
	path, args, env, err := f.m.Wrap(egress.BoxRequest{Dir: f.dir, Workdir: f.workdir, Profile: f.profile("203.0.113.7")},
		[]string{agent}, []string{"KINDEV_NSBOX_AGENT=hang"}, f.workdir)
	if err != nil {
		t.Fatal(err)
	}
	for _, delay := range []time.Duration{0, 5 * time.Second} {
		cmd := exec.Command(path, args...) //nolint:noctx // killed with its group on cleanup
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out := &syncBuf{}
		cmd.Stdout, cmd.Stderr = out, out
		if delay > 0 { // a fresh description: the first run deleted it
			if path, args, env, err = f.m.Wrap(egress.BoxRequest{Dir: f.dir, Workdir: f.workdir, Profile: f.profile("203.0.113.7")},
				[]string{agent}, []string{"KINDEV_NSBOX_AGENT=hang"}, f.workdir); err != nil {
				t.Fatal(err)
			}
			cmd = exec.Command(path, args...) //nolint:noctx // as above
			cmd.Env, cmd.Stdout, cmd.Stderr = env, out, out
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		if delay > 0 {
			deadline := time.Now().Add(60 * time.Second)
			for !strings.Contains(out.String(), "agent hanging") && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
		}
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("delay %v: the run ignored the cancel\n%s", delay, out.String())
		}
		if code := cmd.ProcessState.ExitCode(); code == 0 {
			t.Fatalf("delay %v: a cancelled run exited 0\n%s", delay, out.String())
		}
	}
}

// syncBuf is an output buffer read while the process writes it.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A runner data directory reached through a symlink: hidden under its link and its real path alike, and the
// worktree's mirror still found.
func TestSandboxWithSymlinkedDataDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	link := filepath.Join(t.TempDir(), "data-link")
	if err := os.Symlink(f.m.DataDir, link); err != nil {
		t.Fatal(err)
	}
	real := f.m.DataDir
	f.m.DataDir = link
	f.dir = strings.Replace(f.dir, real, link, 1)
	f.workdir = strings.Replace(f.workdir, real, link, 1)
	verr, out, werr := f.run(t, f.profile("203.0.113.7"))
	if verr != nil || werr != nil || strings.Contains(out, "FAIL") {
		t.Fatalf("verdict %v, agent %v\n%s", verr, werr, out)
	}
	for _, want := range []string{"ok runner-key-hidden", "ok runner-data-hidden", "ok mirror-readable", "ok admin-writable"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// git works in a real worktree of a mirror (KIN-29's layout): it reads the mirror and updates the worktree's own
// index, while the shared mirror stays unwritable.
func TestGitInSandbox(t *testing.T) {
	t.Parallel()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	f := newFixture(t)
	data := f.m.DataDir
	mirror := filepath.Join(data, "repos", "g", "r.git")
	src := filepath.Join(t.TempDir(), "src")
	wt := filepath.Join(data, "workspaces", "g", "r", "leaf")
	env := []string{"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "PATH=/usr/bin:/bin"}
	for _, args := range [][]string{{"init", "-q", "-b", "main", src}, {"-C", src, "commit", "-q", "--allow-empty", "-m", "base"},
		{"init", "-q", "--bare", mirror}, {"-C", src, "push", "-q", mirror, "main"},
		{"--git-dir=" + mirror, "worktree", "add", "-q", wt, "main"}} {
		cmd := exec.Command(gitBin, args...) //nolint:noctx // a short local git command
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f.workdir = wt
	box, err := f.m.Open(t.Context(), egress.BoxRequest{Dir: f.dir, Workdir: wt, Profile: f.profile("203.0.113.7")})
	if err != nil {
		t.Fatal(err)
	}
	b := box.(*Box)
	t.Cleanup(b.Release)
	script := "echo x > new && git status --porcelain && git log -1 --format=%s && git add new && echo added; " +
		"touch " + mirror + "/hooks-x 2>/dev/null && echo MIRROR-WRITABLE; exit 0"
	cmd, err := b.Command([]string{"/bin/sh", "-c", script}, []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1"}, wt)
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
	if err := b.Await(ctx); err != nil {
		t.Fatalf("verdict: %v\n%s", err, out.String())
	}
	_ = cmd.Wait()
	got := out.String()
	if !strings.Contains(got, "?? new") || !strings.Contains(got, "base") || strings.Contains(got, "MIRROR-WRITABLE") {
		t.Fatalf("git in the sandbox:\n%s", got)
	}
	t.Logf("git in the sandbox:\n%s", got)
}
