//go:build unix

package process

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"syscall"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// stubSandbox wraps the agent's command line (no namespaces) and returns a fixed verdict.
type stubSandbox struct {
	verdict error
	argv    []string
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
