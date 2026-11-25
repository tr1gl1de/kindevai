//go:build linux

package nsbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/httpprobe"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// sysProcAttr starts the helper as root of new user, network, mount and PID namespaces (mapped to the runner's
// own uid and gid: no privilege on the host), leading its own process group like any attempt process. The PID
// namespace hides every host process (their /proc/<pid>/root would lead back to the host's sockets) and ends the
// agent's whole tree with the helper.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:     true,
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
}

// namespacesAvailable tries to start a process in new user and network namespaces.
func namespacesAvailable() bool {
	cmd := exec.Command("/bin/true") //nolint:noctx // returns at once
	if _, err := os.Stat(cmd.Path); err != nil {
		cmd = exec.Command("true") //nolint:noctx // returns at once
	}
	attr := sysProcAttr()
	attr.Setpgid = false
	cmd.SysProcAttr = attr
	return cmd.Run() == nil
}

// leakTimeout bounds each leak test (the namespace has no route: they fail at once unless something is wrong).
const leakTimeout = 3 * time.Second

// Public targets of the leak tests (never contacted when the sandbox works: there is no route to them).
var (
	leakIPv4 = "1.1.1.1:443"
	leakIPv6 = "[2606:4700:4700::1111]:443"
	leakDNS  = []string{"1.1.1.1:53", "8.8.8.8:53", "127.0.0.53:53"}
	leakHTTP = []string{"http://1.1.1.1/", "http://example.com/"}
	// Well-known host Unix sockets; /run/WSL holds WSL2's interop sockets.
	leakSockets = []string{"/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock",
		"/run/podman/podman.sock", "/run/dbus/system_bus_socket", "/tmp/.X11-unix/X0", "/mnt/wslg/.X11-unix/X0"}
	leakSocketDirs = []string{"/run/WSL", "/run/user"}
)

// maxSpec bounds the spec the runner sends.
const maxSpec = 4 << 20

// Main is `ai-runner sandbox-exec`: it reads the spec from fd 4, builds the sandbox in the namespaces it was
// started in, reports the verdict on fd 3 and, when verified, runs the agent and exits with its status.
func Main(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: ai-runner sandbox-exec (started by the runner)")
		return ExitUsage
	}
	verdict, specIn := os.NewFile(3, "verdict"), os.NewFile(4, "spec")
	if verdict == nil || specIn == nil {
		return ExitUsage
	}
	unix.CloseOnExec(3)
	data, err := io.ReadAll(io.LimitReader(specIn, maxSpec))
	_ = specIn.Close()
	if err != nil {
		return report(verdict, domain.Verdict{Setup: "spec unreadable"}, stderr)
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return report(verdict, domain.Verdict{Setup: "spec malformed"}, stderr)
	}
	// Mounts, the loopback and the nft child act on this thread's namespaces (all threads share them here, but
	// keep the thread anyway: Pdeathsig of the agent follows the thread that started it).
	runtime.LockOSThread()
	// As the PID namespace's init this process gets only the signals it handles: catch the cancel signals from the
	// start, so a cancel during setup is not lost (the agent is then never started).
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, err := setup(ctx, s)
	if err != nil {
		return report(verdict, domain.Verdict{Setup: err.Error()}, stderr)
	}
	// Not dumpable: the agent (same kuid) can neither ptrace this helper, which holds CAP_NET_ADMIN over the
	// namespace, nor open its /proc entries.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return report(verdict, domain.Verdict{Setup: "not dumpable: " + err.Error()}, stderr)
	}
	if err := confineSockets(); err != nil {
		return report(verdict, domain.Verdict{Setup: err.Error()}, stderr)
	}
	v := domain.Verdict{Leaks: leaks(ctx, s)}
	if s.Probe != nil {
		v.ProxyChecked = true
		v.Probe = probe(ctx, s.Probe, port)
	}
	if code := report(verdict, v, stderr); code != 0 || len(s.Argv) == 0 {
		return code
	}
	return runAgent(s, port, sigs, stderr)
}

func report(f *os.File, v domain.Verdict, stderr io.Writer) int {
	b, _ := json.Marshal(v)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if err := v.Verified(true); err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox:", err)
		return ExitUnverified
	}
	if v.ProxyChecked && v.Probe != "" {
		fmt.Fprintln(stderr, "strict egress sandbox: proxy check failed:", v.Probe)
		return ExitUnverified
	}
	return 0
}

// setup builds the sandbox and returns the forwarder's port on the namespace's loopback.
func setup(ctx context.Context, s Spec) (int, error) {
	if err := hideHost(s); err != nil {
		return 0, err
	}
	if err := loopbackUp(); err != nil {
		return 0, fmt.Errorf("loopback: %w", err)
	}
	if err := loadRules(ctx, s); err != nil {
		return 0, err
	}
	return forward(ctx, s.Socket)
}

// hideHost covers the hidden directories with empty tmpfs mounts and binds the kept ones back.
func hideHost(s Spec) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private mounts: %w", err)
	}
	type kept struct {
		path string
		fd   int
	}
	var keep []kept
	for _, p := range append(append([]string{}, s.Keep...), filepath.Dir(s.Socket)) {
		fd, err := unix.Open(p, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("opening a kept directory: %w", err)
		}
		keep = append(keep, kept{p, fd})
	}
	defer func() {
		for _, k := range keep {
			_ = unix.Close(k.fd)
		}
	}()
	var covered []string
	for _, h := range s.Hide {
		if r, err := filepath.EvalSymlinks(h); err == nil {
			h = r // /var/run -> /run; a data directory reached through a symlink
		}
		fi, err := os.Lstat(h)
		if err != nil || !fi.IsDir() || slices.Contains(covered, h) { // missing or already covered
			continue
		}
		opts, flags := "mode=0755,size=16m", uintptr(unix.MS_NOSUID|unix.MS_NODEV)
		if h == "/tmp" || h == "/var/tmp" || h == "/dev/shm" {
			opts = "mode=1777,size=512m"
		}
		if err := unix.Mount("tmpfs", h, "tmpfs", flags, opts); err != nil {
			return fmt.Errorf("hiding %s: %w", h, err)
		}
		covered = append(covered, h)
	}
	// The adapter sockets' directory must be covered: other attempts' sockets live next to this one.
	if !under(filepath.Dir(filepath.Dir(s.Socket)), covered) {
		return errors.New("the adapter socket directory is not hidden")
	}
	// A /proc of the sandbox's PID namespace only.
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mounting /proc: %w", err)
	}
	for _, k := range keep {
		if !under(k.path, covered) {
			continue
		}
		if err := os.MkdirAll(k.path, 0o700); err != nil {
			return fmt.Errorf("binding a kept directory: %w", err)
		}
		src := "/proc/self/fd/" + strconv.Itoa(k.fd)
		if err := unix.Mount(src, k.path, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("binding a kept directory: %w", err)
		}
	}
	return nil
}

func under(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

func loadRules(ctx context.Context, s Spec) error {
	if !filepath.IsAbs(s.NFT) {
		return errors.New("nft: not an absolute path")
	}
	cmd := exec.CommandContext(ctx, s.NFT, "-f", "-") //nolint:gosec // the runner's resolved nft binary
	cmd.Stdin = strings.NewReader(s.Ruleset)
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("loading nftables rules: %w: %s", err, firstLine(out))
	}
	out, err := exec.CommandContext(ctx, s.NFT, "list", "table", "inet", domain.TableName).CombinedOutput() //nolint:gosec // as above
	if err != nil || !strings.Contains(string(out), "policy drop") {
		return fmt.Errorf("nftables rules not in place: %s", firstLine(out))
	}
	return nil
}

func firstLine(b []byte) string {
	l, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}

// forward listens on the namespace's loopback and relays every connection to the proxy adapter's socket.
func forward(ctx context.Context, socket string) (int, error) {
	dirfd, err := unix.Open(filepath.Dir(socket), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("adapter socket: %w", err)
	}
	// The socket's directory may be covered now; reach it through the descriptor opened before.
	target := "/proc/self/fd/" + strconv.Itoa(dirfd) + "/" + filepath.Base(socket)
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		_ = unix.Close(dirfd)
		return 0, fmt.Errorf("forwarder: %w", err)
	}
	go func() {
		defer func() { _ = unix.Close(dirfd) }()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go relay(c, target)
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func relay(c net.Conn, target string) {
	defer func() { _ = c.Close() }()
	up, err := net.Dial("unix", target) //nolint:noctx // a local socket
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = io.Copy(up, c)
		if u, ok := up.(*net.UnixConn); ok {
			_ = u.CloseWrite()
		}
	})
	_, _ = io.Copy(c, up)
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.CloseWrite()
	}
	wg.Wait()
}

// leaks runs every leak test; each must fail.
func leaks(ctx context.Context, s Spec) []domain.Leak {
	d := net.Dialer{Timeout: leakTimeout}
	tcp := func(test, addr string) domain.Leak {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return domain.Leak{Test: test, Target: addr}
		}
		return domain.Leak{Test: test, Blocked: true}
	}
	out := []domain.Leak{tcp(domain.LeakTCP4, leakIPv4), tcp(domain.LeakTCP6, leakIPv6), dnsLeak(ctx, d)}
	out = append(out, httpLeak(ctx), unixLeak(s), domain.Leak{Test: domain.LeakVsock, Blocked: vsockBlocked(), Target: "AF_VSOCK"})
	return out
}

// dnsLeak sends a query to the system's resolvers and public ones; any answer is a leak.
func dnsLeak(ctx context.Context, d net.Dialer) domain.Leak {
	servers := append([]string{}, leakDNS...)
	if f, err := os.Open("/etc/resolv.conf"); err == nil {
		sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
		for sc.Scan() {
			if fs := strings.Fields(sc.Text()); len(fs) == 2 && fs[0] == "nameserver" {
				servers = append(servers, net.JoinHostPort(fs[1], "53"))
			}
		}
		_ = f.Close()
	}
	// A query for "example.com" A (id 0x4b44).
	q := []byte{0x4b, 0x44, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	for _, srv := range servers {
		for _, network := range []string{"udp", "tcp"} {
			c, err := d.DialContext(ctx, network, srv)
			if err != nil {
				continue
			}
			msg := q
			if network == "tcp" {
				msg = append([]byte{0, 29}, q...) // the query's length
			}
			_ = c.SetDeadline(time.Now().Add(leakTimeout))
			_, werr := c.Write(msg)
			n, rerr := c.Read(make([]byte, 512))
			_ = c.Close()
			if werr == nil && rerr == nil && n > 0 {
				return domain.Leak{Test: domain.LeakDNS, Target: network + " " + srv}
			}
		}
	}
	return domain.Leak{Test: domain.LeakDNS, Blocked: true}
}

// httpLeak is a client that ignores HTTP_PROXY.
func httpLeak(ctx context.Context) domain.Leak {
	c := &http.Client{Timeout: leakTimeout, Transport: &http.Transport{Proxy: nil}}
	for _, u := range leakHTTP {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		if resp, err := c.Do(req); err == nil {
			_ = resp.Body.Close()
			return domain.Leak{Test: domain.LeakNoProxy, Target: u}
		}
	}
	return domain.Leak{Test: domain.LeakNoProxy, Blocked: true}
}

// unixLeak tries the well-known host sockets, every socket under the WSL interop and user runtime directories,
// the runner's extra ones and $WSL_INTEROP.
func unixLeak(s Spec) domain.Leak {
	cands := append(append([]string{}, leakSockets...), s.Sockets...)
	if p := os.Getenv("WSL_INTEROP"); p != "" {
		cands = append(cands, p)
	}
	for _, d := range leakSocketDirs {
		_ = filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err == nil && e.Type()&os.ModeSocket != 0 && len(cands) < 256 {
				cands = append(cands, p)
			}
			return nil
		})
	}
	for _, p := range cands {
		if p == s.Socket { // the attempt's own proxy adapter: the sanctioned way out
			continue
		}
		d := net.Dialer{Timeout: leakTimeout}
		c, err := d.DialContext(context.Background(), "unix", p) //nolint:gosec // G704: probing host sockets is the test
		if err == nil {
			_ = c.Close()
			return domain.Leak{Test: domain.LeakUnix, Target: p}
		}
	}
	return domain.Leak{Test: domain.LeakUnix, Blocked: true}
}

// probe checks the proxy path (through the forwarder and the adapter) and the egress IP.
func probe(ctx context.Context, p *ProbeSpec, port int) string {
	np := protocol.NetworkProfile{Enforcement: domain.EnforcementStrict, DNS: domain.DNSProxy,
		Proxy:        &protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: port},
		ExpectedIPv4: p.ExpectedIPv4, ExpectedIPv6: p.ExpectedIPv6, DiagnosticURL: p.DiagnosticURL}
	r := (&httpprobe.Prober{}).Probe(ctx, np, nil)
	if r.OK {
		return ""
	}
	return r.Code
}

// proxyEnv is the agent's environment: the runner's (already reduced) environment without variables that point
// at host sockets, plus the proxy variables.
func proxyEnv(env []string, port int) []string {
	drop := map[string]bool{"SSH_AUTH_SOCK": true, "DBUS_SESSION_BUS_ADDRESS": true, "DOCKER_HOST": true,
		"WSL_INTEROP": true, "XDG_RUNTIME_DIR": true, "DISPLAY": true, "WAYLAND_DISPLAY": true, "PULSE_SERVER": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true,
		"https_proxy": true, "all_proxy": true, "no_proxy": true}
	out := make([]string, 0, len(env)+8)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	px := "http://127.0.0.1:" + strconv.Itoa(port)
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		out = append(out, k+"="+px)
	}
	return append(out, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1")
}

// runAgent starts the agent in a nested user namespace where it keeps the runner's uid but has no capability
// over the sandbox's namespaces, then waits for it and exits like it.
func runAgent(s Spec, port int, sigs chan os.Signal, stderr io.Writer) int {
	path := s.Argv[0]
	if !filepath.IsAbs(path) {
		p, err := exec.LookPath(path)
		if err != nil {
			fmt.Fprintln(stderr, "strict egress sandbox: agent not found")
			return ExitAgentStart
		}
		path = p
	}
	cmd := &exec.Cmd{Path: path, Args: s.Argv, Env: proxyEnv(s.Env, port), Dir: s.Dir,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr} // stdin: e.g. a CLI's prompt
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: s.UID, HostID: 0, Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: s.GID, HostID: 0, Size: 1}},
		Pdeathsig:   syscall.SIGKILL, // the forwarder dies with this helper: so does the agent
	}
	// The process group receives cancel signals; the helper outlives the agent to report its status. Caught (in
	// Main), not ignored: an ignored disposition would be inherited by the agent across exec.
	select {
	case sig := <-sigs: // cancelled while the sandbox was being built: the agent never starts
		return 128 + int(sig.(syscall.Signal))
	default:
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox: starting the agent:", err)
		return ExitAgentStart
	}
	go func() {
		for sig := range sigs { // the group's later signals reach the agent itself; one that raced its start is passed on
			_ = cmd.Process.Signal(sig)
		}
	}()
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()) // as a shell reports it (the PID namespace's init cannot re-raise it)
		}
		return ee.ExitCode()
	}
	if err != nil {
		return ExitAgentStart
	}
	return 0
}
