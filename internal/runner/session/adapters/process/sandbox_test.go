//go:build unix

package process

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// stubSandbox wraps the agent's command line (no namespaces) and returns a fixed verdict.
type stubSandbox struct {
	verdict error
	argv    []string
	// alive records, at Cancel, whether the helper's process group still ran (Cancel must precede the SIGTERM).
	cancels atomic.Int32
	alive   atomic.Bool
	pgid    int
}

func (s *stubSandbox) Command(argv, env []string, dir string) (*exec.Cmd, error) {
	s.argv = argv
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:noctx // test
	cmd.Env, cmd.Dir = env, dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}
func (s *stubSandbox) Await(context.Context) error { return s.verdict }
func (s *stubSandbox) Release()                    {}
func (s *stubSandbox) Cancel() {
	s.cancels.Add(1)
	s.alive.Store(s.pgid > 0 && syscall.Kill(-s.pgid, 0) == nil)
}
func (s *stubSandbox) Endpoint() string     { return "" }
func (s *stubSandbox) Fault() <-chan string { return nil }
func (s *stubSandbox) Summary() string      { return "" }
func (s *stubSandbox) Profile() string      { return "" }

func TestLaunchRunsThroughTheSandbox(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	sb := &stubSandbox{}
	out := &syncBuf{}
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: t.TempDir(), Scenario: "success", Attempt: 1, Stdout: out,
		Stderr: out, Sandbox: sb})
	if err != nil {
		t.Fatal(err)
	}
	if ex := p.Wait(); ex != (domain.Exit{}) {
		t.Fatalf("exit %+v\n%s", ex, out.String())
	}
	if len(sb.argv) == 0 || sb.argv[0] != l.Program || !slices.Contains(sb.argv, "--scenario") {
		t.Fatalf("sandbox got argv %v", sb.argv)
	}
}

// An unverified sandbox: Launch fails with ErrNetwork and stops the helper's process group.
func TestLaunchRefusesAnUnverifiedSandbox(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	sb := &stubSandbox{verdict: errors.New("egress_leak: dns")}
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: t.TempDir(), Scenario: "hang", Attempt: 1, Sandbox: sb})
	if !errors.Is(err, app.ErrNetwork) || p != nil {
		t.Fatalf("Launch = %v, %v; want ErrNetwork", p, err)
	}
}

// KIN-103: a cancel marks the sandbox cancelled before it signals the group, so the helper (its PID namespace's init,
// which drops a SIGTERM that arrives before it handles it) still never starts the agent; SIGKILL needs no mark.
func TestSignalCancelsTheSandboxFirst(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	sb := &stubSandbox{}
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: t.TempDir(), Scenario: "hang", Attempt: 1, Sandbox: sb})
	if err != nil {
		t.Fatal(err)
	}
	sb.pgid = p.PGID()
	if err := p.Signal(false); err != nil {
		t.Fatal(err)
	}
	p.Wait()
	if sb.cancels.Load() != 1 || !sb.alive.Load() {
		t.Fatalf("cancels %d, group alive at the cancel %v; want 1 before the SIGTERM", sb.cancels.Load(), sb.alive.Load())
	}
	if err := p.Signal(true); err != nil {
		t.Fatal(err)
	}
	if sb.cancels.Load() != 1 {
		t.Fatalf("SIGKILL cancelled the sandbox again (%d)", sb.cancels.Load())
	}
}
