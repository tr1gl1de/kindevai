// Package nsbox builds the strict egress sandbox of an attempt (KIN-31, KB KIN-A-22) on Linux and WSL2 without
// root: the runner starts a helper (`ai-runner sandbox-exec`) in new user, network and mount namespaces. In them
// the helper
//
//   - brings up the namespace's own loopback, its only interface: there is no route to the host or anywhere else;
//   - loads the nftables ruleset (domain.Ruleset) that rejects everything leaving that loopback, DNS included;
//   - pivots into a minimal filesystem view (fsview.go, KIN-80): the system directories read-only, the attempt's
//     worktree and state directory, a private /tmp and HOME holding only the CLI's login files; the runner's
//     HOME and data directory, /run (Docker, D-Bus, other attempts' adapters), the WSL mounts and Windows drives
//     are simply absent;
//   - forwards 127.0.0.1:<port> to the attempt's proxy adapter (a Unix socket the runner serves; it alone holds
//     the upstream proxy and its credentials) and points HTTP(S)_PROXY/ALL_PROXY at it;
//   - runs the preflight: every leak test must fail (network, host sockets, and the filesystem: the runner's HOME
//     and data directory must be invisible, / and the system directories unwritable), the proxy path must work
//     and the egress IP must match;
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/proxyadapter"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// Spec is what the helper builds; the runner sends it on the helper's fd 4 (never written to disk: Env is the
// agent's environment).
type Spec struct {
	Socket  string   `json:"socket"`  // the proxy adapter's Unix socket
	Ruleset string   `json:"ruleset"` // nftables ruleset of the namespace
	NFT     string   `json:"nft"`     // path of the nft binary
	Keep    []string `json:"keep"`    // directories bound read-write into the sandbox (state directory, worktree)
	// Home and DataDir are the runner user's HOME and the runner's data directory: the preflight checks that
	// nothing of them is visible but what the view places there.
	Home    string `json:"home,omitempty"`
	DataDir string `json:"dataDir,omitempty"`
	// Sockets are host Unix sockets the leak test tries (besides the well-known ones).
	Sockets []string `json:"sockets,omitempty"`
	// Probe checks the proxy path and egress IP; nil in a self-test.
	Probe *ProbeSpec `json:"probe,omitempty"`
	// The agent; no Argv in a self-test.
	Argv []string `json:"argv,omitempty"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	// HomeBinds are bound read-write below the agent's HOME (the task's CLI session store, KIN-86).
	HomeBinds []egress.HomeBind `json:"homeBinds,omitempty"`
	UID       int               `json:"uid"` // the runner's uid and gid, which the agent keeps (without capabilities)
	GID       int               `json:"gid"`
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
	// Program and Args start the helper: `<Program> <Args...>` (the runner's own binary with "sandbox-exec").
	Program string
	Args    []string
	// RunArgs start the outer launcher of a wrapped run (Wrap): `<Program> <RunArgs...>`; "sandbox-run" when nil.
	RunArgs []string
	Env     []string // the helper's environment (empty: none)
	DataDir string   // the runner's data directory, hidden from agents
	// Home is the runner user's HOME, hidden from agents (os.UserHomeDir when empty).
	Home string
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

func (m *Manager) home() string {
	if m.Home != "" {
		return m.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

// socketBase is where adapter sockets live: the user's runtime directory under /run, else /tmp. Neither is part of
// the sandbox's view, so an agent never sees another attempt's adapter.
func socketBase() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); strings.HasPrefix(d, "/run/") && len(d) < 64 {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() { //nolint:gosec // G703: the user's own runtime directory
			return d
		}
	}
	return "/tmp"
}

// staleAge is how old a socket directory without a socket must be before a sweep may remove it (one being created
// has no socket yet).
const staleAge = time.Minute

// sweepStale removes adapter socket directories nobody serves any more, left by a launcher or runner that was
// killed before it could clean up, and returns them: a socket that refuses connections, or no socket after
// staleAge. A directory whose socket still accepts connections is in use.
func sweepStale(base string, now time.Time) []string {
	dirs, _ := filepath.Glob(filepath.Join(base, "kindev-egress-*"))
	var removed []string
	for _, d := range dirs {
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() {
			continue
		}
		sock := filepath.Join(d, "s")
		if _, err := os.Lstat(sock); err != nil {
			if now.Sub(fi.ModTime()) < staleAge {
				continue // being created
			}
		} else {
			dl := net.Dialer{Timeout: time.Second}
			c, err := dl.DialContext(context.Background(), "unix", sock)
			if err == nil {
				_ = c.Close()
				continue
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				continue // not provably abandoned
			}
		}
		if os.RemoveAll(d) == nil { // fails harmlessly for another user's directory (sticky /tmp)
			removed = append(removed, d)
		}
	}
	return removed
}

// Box is one attempt's sandbox: its proxy adapter and the helper's verdict pipe.
type Box struct {
	m      *Manager
	spec   Spec
	stop   context.CancelFunc
	served chan struct{}
	once   sync.Once

	mu       sync.Mutex
	verdictR *os.File
	verdictW *os.File
	specR    *os.File // the helper's fd 4; the parent's copy is closed once it started
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
	sweepStale(socketBase(), time.Now())
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
	b := &Box{m: m, stop: stop, served: make(chan struct{})}
	srv := &proxyadapter.Server{Upstream: *p.Proxy, Allow: allow, Logger: m.Logger}
	go func() { defer close(b.served); _ = srv.Serve(ctx, l) }()
	keep := []string{r.Dir}
	if r.Workdir != "" {
		keep = append(keep, r.Workdir)
	}
	b.spec = Spec{Socket: sock, Ruleset: rules, NFT: nft, Keep: keep, Home: m.home(), DataDir: m.DataDir, Sockets: m.Sockets,
		HomeBinds: r.HomeBinds,
		Probe:     &ProbeSpec{DiagnosticURL: p.DiagnosticURL, ExpectedIPv4: p.ExpectedIPv4, ExpectedIPv6: p.ExpectedIPv6},
		UID:       os.Getuid(), GID: os.Getgid()}
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
	return b.m.command(b, s)
}

func (m *Manager) command(b *Box, s Spec) (*exec.Cmd, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	sr, sw, err := os.Pipe()
	if err != nil {
		_, _ = r.Close(), w.Close()
		return nil, err
	}
	go func() { // the helper reads it at once; a helper that never starts closes the pipe (EPIPE)
		_, _ = sw.Write(data)
		_ = sw.Close()
	}()
	b.mu.Lock()
	b.verdictR, b.verdictW, b.specR = r, w, sr
	b.mu.Unlock()
	//nolint:noctx,gosec // must outlive ctx like any attempt process; fixed program
	cmd := exec.Command(m.Program, m.Args...)
	cmd.Env = m.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{w, sr} // fd 3: the verdict, fd 4: the spec
	cmd.SysProcAttr = sysProcAttr()
	return cmd, nil
}

// ErrVerdict: the sandbox did not verify; the agent was not started.
var ErrVerdict = errors.New("strict egress sandbox not verified")

// Await reads the helper's verdict after the command started: nil once the sandbox verified (the agent then
// runs), else why not (the helper exits without starting the agent).
func (b *Box) Await(ctx context.Context) error {
	r, w, sr := b.take()
	if r == nil {
		return errors.New("the sandbox command was not prepared")
	}
	_, _ = w.Close(), sr.Close() // the helper holds the only other ends now: EOF when it ends
	defer func() { _ = r.Close() }()
	return readVerdict(ctx, r, b.m.verifyTimeout(), false)
}

func (b *Box) take() (r, w, sr *os.File) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, w, sr = b.verdictR, b.verdictW, b.specR
	b.verdictR, b.verdictW, b.specR = nil, nil, nil
	return r, w, sr
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
		for _, f := range []*os.File{b.verdictR, b.verdictW, b.specR} {
			if f != nil {
				_ = f.Close()
			}
		}
		b.mu.Unlock()
		_ = os.RemoveAll(filepath.Dir(b.spec.Socket))
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
	cmd, err := m.command(b, s)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the self-test sandbox: %w", err)
	}
	r, w, sr := b.take()
	_, _ = w.Close(), sr.Close()
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
