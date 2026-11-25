// Package nsbox builds the strict egress sandbox of an attempt (KIN-31, KB KIN-A-22) on Linux and WSL2 without
// root: the runner starts a helper (`ai-runner sandbox-exec`) in new user, network and mount namespaces. In them
// the helper
//
//   - brings up the namespace's own loopback, its only interface: there is no route to the host or anywhere else;
//   - loads the nftables ruleset (domain.Ruleset) that rejects everything leaving that loopback, DNS included;
//   - covers /run, /tmp, the WSL mounts and the runner's data directory with empty tmpfs mounts, so no host Unix
//     socket (Docker daemon, D-Bus, WSL interop, other attempts' adapters) is reachable, and binds the attempt's
//     worktree and state directory back;
//   - forwards 127.0.0.1:<port> to the attempt's proxy adapter (a Unix socket the runner serves; it alone holds
//     the upstream proxy and its credentials) and points HTTP(S)_PROXY/ALL_PROXY at it;
//   - runs the preflight: every leak test must fail, the proxy path must work and the egress IP must match;
//   - reports the verdict on fd 3 and only then starts the agent, in a nested user namespace without any
//     capability (it cannot change the rules or the mounts).
//
// Nothing changes on the host: the namespaces vanish with their last process, so a crash leaves nothing behind.
package nsbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/proxyadapter"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// Spec is what the helper builds; the runner writes it to the attempt's state directory. It holds no secret.
type Spec struct {
	Socket  string   `json:"socket"`  // the proxy adapter's Unix socket
	Ruleset string   `json:"ruleset"` // nftables ruleset of the namespace
	NFT     string   `json:"nft"`     // path of the nft binary
	Hide    []string `json:"hide"`    // directories covered with an empty tmpfs
	Keep    []string `json:"keep"`    // directories bound back inside hidden ones
	// Sockets are host Unix sockets the leak test tries (besides the well-known ones).
	Sockets []string `json:"sockets,omitempty"`
	// Probe checks the proxy path and egress IP; nil in a self-test.
	Probe *ProbeSpec `json:"probe,omitempty"`
	// The agent; no Argv in a self-test.
	Argv []string `json:"argv,omitempty"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	UID  int      `json:"uid"` // the runner's uid and gid, which the agent keeps (without capabilities)
	GID  int      `json:"gid"`
}

// ProbeSpec is the proxy and egress IP check of the preflight.
type ProbeSpec struct {
	DiagnosticURL string `json:"diagnosticUrl"`
	ExpectedIPv4  string `json:"expectedIpv4,omitempty"`
	ExpectedIPv6  string `json:"expectedIpv6,omitempty"`
}

// Exit codes of the helper before the agent runs.
const (
	ExitUsage      = 120
	ExitUnverified = 121
	ExitAgentStart = 122
)

// Manager builds sandboxes.
type Manager struct {
	// Program and Args start the helper: `<Program> <Args...> <spec file>` (the runner's own binary with
	// "sandbox-exec").
	Program string
	Args    []string
	Env     []string // the helper's environment (empty: none)
	DataDir string   // the runner's data directory, hidden from agents
	// Sockets are extra host Unix sockets the leak test must not reach.
	Sockets []string
	Logger  *slog.Logger
	// VerifyTimeout bounds the preflight (default 90s).
	VerifyTimeout time.Duration
	// LookPath finds nft (exec.LookPath, plus /usr/sbin and /sbin, when nil).
	LookPath func(string) (string, error)
}

var _ egress.Boxes = (*Manager)(nil)

func (m *Manager) logger() *slog.Logger {
	if m.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Logger
}

func (m *Manager) verifyTimeout() time.Duration {
	if m.VerifyTimeout <= 0 {
		return 90 * time.Second
	}
	return m.VerifyTimeout
}

func (m *Manager) nft() (string, error) {
	look := m.LookPath
	if look == nil {
		look = exec.LookPath
	}
	if p, err := look("nft"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/sbin/nft", "/sbin/nft"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", errors.New("nft not found")
}

// hidden are the host directories covered in every sandbox (those that exist).
var hidden = []string{"/run", "/var/run", "/tmp", "/var/tmp", "/dev/shm", "/mnt/wsl", "/mnt/wslg"}

func (m *Manager) hide() []string {
	out := slices.Clone(hidden)
	if m.DataDir != "" {
		out = append(out, m.DataDir)
	}
	return out
}

// socketBase is where adapter sockets live: the user's runtime directory, else the temporary directory.
func socketBase() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && len(d) < 64 {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() { //nolint:gosec // G703: the user's own runtime directory
			return d
		}
	}
	return os.TempDir()
}

// Box is one attempt's sandbox: its proxy adapter and the helper's verdict pipe.
type Box struct {
	m        *Manager
	spec     Spec
	specPath string
	stop     context.CancelFunc
	served   chan struct{}
	once     sync.Once

	mu       sync.Mutex
	verdictR *os.File
	verdictW *os.File
}

// Open starts the attempt's proxy adapter and prepares the sandbox; the agent starts with Command.
func (m *Manager) Open(_ context.Context, r egress.BoxRequest) (egress.Box, error) {
	return m.open(r)
}

func (m *Manager) open(r egress.BoxRequest) (*Box, error) {
	if runtime.GOOS != "linux" {
		return nil, egress.ErrUnsupported
	}
	p := r.Profile
	rules, err := domain.Ruleset(domain.Policy{Enforcement: p.Enforcement, DNS: p.DNS, HasProxy: p.Proxy != nil})
	if err != nil {
		return nil, err
	}
	nft, err := m.nft()
	if err != nil {
		return nil, err
	}
	allow := domain.Allowlist{Restricted: p.Restricted}
	for _, a := range p.Allow {
		allow.Rules = append(allow.Rules, domain.Rule{Host: a.Host, CIDR: a.CIDR, Ports: a.Ports})
	}
	// A short private directory: Unix socket paths are limited to 107 bytes, which a data directory path can
	// exceed. Inside the sandbox only this attempt's socket directory is visible.
	sockDir, err := os.MkdirTemp(socketBase(), "kindev-egress-")
	if err != nil {
		return nil, fmt.Errorf("adapter socket directory: %w", err)
	}
	sock := filepath.Join(sockDir, "s")
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		_ = os.RemoveAll(sockDir)
		return nil, fmt.Errorf("listening for the sandbox: %w", err)
	}
	ctx, stop := context.WithCancel(context.Background()) // lives as long as the attempt, not the command
	b := &Box{m: m, stop: stop, served: make(chan struct{}), specPath: filepath.Join(r.Dir, "sandbox.json")}
	srv := &proxyadapter.Server{Upstream: *p.Proxy, Allow: allow, Logger: m.Logger}
	go func() { defer close(b.served); _ = srv.Serve(ctx, l) }()
	keep := []string{r.Dir}
	if r.Workdir != "" {
		keep = append(keep, r.Workdir)
	}
	b.spec = Spec{Socket: sock, Ruleset: rules, NFT: nft, Hide: m.hide(), Keep: keep, Sockets: m.Sockets,
		Probe: &ProbeSpec{DiagnosticURL: p.DiagnosticURL, ExpectedIPv4: p.ExpectedIPv4, ExpectedIPv6: p.ExpectedIPv6},
		UID:   os.Getuid(), GID: os.Getgid()}
	return b, nil
}

// Command returns the helper command that runs argv (with env, in dir) inside the sandbox. The caller sets its
// output, starts it, then calls Await.
func (b *Box) Command(argv, env []string, dir string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command")
	}
	s := b.spec
	s.Argv, s.Env, s.Dir = argv, env, dir
	return b.m.command(b, s, b.specPath)
}

func (m *Manager) command(b *Box, s Spec, specPath string) (*exec.Cmd, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("writing the sandbox spec: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.verdictR, b.verdictW = r, w
	b.mu.Unlock()
	//nolint:noctx,gosec // must outlive ctx like any attempt process; fixed program
	cmd := exec.Command(m.Program, append(slices.Clone(m.Args), specPath)...)
	cmd.Env = m.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{w} // fd 3: the verdict
	cmd.SysProcAttr = sysProcAttr()
	return cmd, nil
}

// ErrVerdict: the sandbox did not verify; the agent was not started.
var ErrVerdict = errors.New("strict egress sandbox not verified")

// Await reads the helper's verdict after the command started: nil once the sandbox verified (the agent then
// runs), else why not (the helper exits without starting the agent).
func (b *Box) Await(ctx context.Context) error {
	b.mu.Lock()
	r, w := b.verdictR, b.verdictW
	b.verdictR, b.verdictW = nil, nil
	b.mu.Unlock()
	if r == nil {
		return errors.New("the sandbox command was not prepared")
	}
	_ = w.Close() // the helper holds the only write end now: EOF when it ends
	defer func() { _ = r.Close() }()
	return readVerdict(ctx, r, b.m.verifyTimeout(), false)
}

func readVerdict(ctx context.Context, r *os.File, timeout time.Duration, selfTest bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = r.SetReadDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { _ = r.SetReadDeadline(time.Now()) })
	defer stop()
	line, err := bufio.NewReader(io.LimitReader(r, 64<<10)).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("%w: no verdict from the sandbox (%s)", ErrVerdict, verdictReadError(err))
	}
	var v domain.Verdict
	if err := json.Unmarshal(line, &v); err != nil {
		return fmt.Errorf("%w: unreadable verdict", ErrVerdict)
	}
	if err := v.Verified(selfTest); err != nil {
		return fmt.Errorf("%w: %w", ErrVerdict, err)
	}
	return nil
}

func verdictReadError(err error) string {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return "timed out"
	}
	if errors.Is(err, io.EOF) {
		return "the helper ended"
	}
	return "read failed"
}

// Release stops the proxy adapter (cutting the sandbox off) and removes the socket and spec.
func (b *Box) Release() {
	b.once.Do(func() {
		b.stop()
		<-b.served
		b.mu.Lock()
		for _, f := range []*os.File{b.verdictR, b.verdictW} {
			if f != nil {
				_ = f.Close()
			}
		}
		b.mu.Unlock()
		_ = os.RemoveAll(filepath.Dir(b.spec.Socket))
		_ = os.Remove(b.specPath)
	})
}

// Support checks the platform and runs a self-test sandbox (no agent, no proxy): ok only when it verified.
func (m *Manager) Support(ctx context.Context) (bool, string) {
	pl := domain.Platform{GOOS: runtime.GOOS}
	if pl.GOOS != "linux" {
		return domain.Support(pl)
	}
	_, err := m.nft()
	pl.NFT = err == nil
	pl.Namespace = namespacesAvailable()
	if pl.NFT && pl.Namespace {
		if err := m.selfTest(ctx); err != nil {
			pl.SelfTest = strings.TrimPrefix(err.Error(), ErrVerdict.Error()+": ")
		}
	}
	return domain.Support(pl)
}

// selfTest builds a sandbox with a strict policy and a dummy proxy adapter and checks the leak tests.
func (m *Manager) selfTest(ctx context.Context) error {
	dir, err := os.MkdirTemp(m.DataDir, "egress-selftest-")
	if err != nil {
		return fmt.Errorf("self-test directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	b, err := m.open(egress.BoxRequest{Dir: dir, Profile: protocol.NetworkProfile{Enforcement: domain.EnforcementStrict,
		DNS: domain.DNSProxy, Proxy: &protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: 9}}})
	if err != nil {
		return err
	}
	defer b.Release()
	s := b.spec
	s.Probe = nil
	cmd, err := m.command(b, s, b.specPath)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the self-test sandbox: %w", err)
	}
	b.mu.Lock()
	r, w := b.verdictR, b.verdictW
	b.verdictR, b.verdictW = nil, nil
	b.mu.Unlock()
	_ = w.Close()
	verr := readVerdict(ctx, r, m.verifyTimeout(), true)
	_ = r.Close()
	if verr != nil {
		_ = cmd.Process.Kill()
	}
	werr := cmd.Wait()
	if verr != nil {
		m.logger().Warn("strict egress self-test failed", "err", verr)
		return verr
	}
	if werr != nil {
		return fmt.Errorf("self-test sandbox: %w", werr)
	}
	return nil
}
