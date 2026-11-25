package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/configstore"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/datadir"
	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// fakeBoxes stands in for nsbox (real namespaces are covered by its integration tests): a box "verifies" and runs
// the command it is given on the host, recording it, so the test sees exactly what the daemon asks the sandbox to run.
type fakeBoxes struct {
	supported bool
	mu        sync.Mutex
	opened    int
	argv      [][]string
}

func (f *fakeBoxes) Open(context.Context, egress.BoxRequest) (egress.Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return &fakeBox{f: f}, nil
}

func (f *fakeBoxes) Support(context.Context) (bool, string) {
	if f.supported {
		return true, ""
	}
	return false, "unsupported here"
}

func (f *fakeBoxes) Reattach(context.Context, egress.BoxRequest, string) (egress.Box, error) {
	return nil, os.ErrNotExist
}

func (f *fakeBoxes) state() (int, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, slices.Clone(f.argv)
}

type fakeBox struct{ f *fakeBoxes }

var _ sessionapp.Sandbox = (*fakeBox)(nil)

func (b *fakeBox) Command(argv, env []string, dir string) (*exec.Cmd, error) {
	b.f.mu.Lock()
	b.f.argv = append(b.f.argv, slices.Clone(argv))
	b.f.mu.Unlock()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // the test binary as agent-exec
	cmd.Env, cmd.Dir = env, dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}
func (b *fakeBox) Await(context.Context) error { return nil }
func (b *fakeBox) Cancel()                     {}
func (b *fakeBox) Release()                    {}
func (b *fakeBox) Endpoint() string            { return "" }
func (b *fakeBox) Profile() string             { return "fp" }
func (b *fakeBox) Fault() <-chan string        { return nil }
func (b *fakeBox) Summary() string             { return "" }

type wiringFixture struct {
	x      *sessionapp.Executor
	boxes  *fakeBoxes
	events func() ([]string, []protocol.Failure)
	ran    string // the file the fake CLI writes its arguments to
}

// newWiringFixture wires an executor exactly like the daemon (newExecutor + wireAgents) with a fake `claude` on
// PATH, a network configuration of the given enforcement, and fake sandboxes in place of nsbox.
func newWiringFixture(t *testing.T, enforcement string, supported bool) *wiringFixture {
	t.Helper()
	d, err := datadir.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch, closeSpool, err := commandChannel(d, logger)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable() // TestMain serves agent-exec
	if err != nil {
		t.Fatal(err)
	}
	bin, home := t.TempDir(), t.TempDir()
	ran := filepath.Join(t.TempDir(), "claude-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + ran + "\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // executable fake CLI
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := strictProfile()
	p.Enforcement = enforcement
	if err := (configstore.File{Path: networkConfigPath(d)}).Save(
		protocol.ConfigUpdate{Network: protocol.NetworkConfig{Default: p}}); err != nil {
		t.Fatal(err)
	}
	strict := strictEgress(d, exe, logger)
	boxes := &fakeBoxes{supported: supported}
	strict.Boxes = boxes
	x := newExecutor(ch, d.Path(), exe, nil, logger)
	lookPath := func(name string) (string, error) {
		if name == "claude" {
			return filepath.Join(bin, name), nil
		}
		return "", exec.ErrNotFound
	}
	wireAgents(x, d, cliAgents(home, lookPath, logger), strict)
	t.Cleanup(func() { x.KillAll(); x.Close(); closeSpool() })
	events := func() ([]string, []protocol.Failure) {
		evs, err := ch.Spool.Read(0, 1000, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		var fails []protocol.Failure
		for _, e := range evs {
			kinds = append(kinds, e.Kind)
			if e.Kind == protocol.EventFailed {
				var f protocol.Failure
				_ = json.Unmarshal(e.Body, &f)
				fails = append(fails, f)
			}
		}
		return kinds, fails
	}
	return &wiringFixture{x: x, boxes: boxes, events: events, ran: ran}
}

func cliAssign(bypass bool) sessiondomain.Assignment {
	return sessiondomain.Assignment{Step: "S1", Fence: 1, Execution: "E1", Task: "T1", Project: "P1",
		Repository: "acme/app", Adapter: sessiondomain.AgentClaude, Prompt: "Fix the bug", Model: "sonnet",
		BypassPermissions: bypass}
}

func (f *wiringFixture) waitEnd(t *testing.T) ([]string, []protocol.Failure) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		kinds, fails := f.events()
		if slices.Contains(kinds, protocol.EventCompleted) || slices.Contains(kinds, protocol.EventFailed) {
			return kinds, fails
		}
		if time.Now().After(deadline) {
			t.Fatalf("the attempt did not end: %v", kinds)
		}
		time.Sleep(20 * time.Millisecond) // polling with a deadline
	}
}

// KIN-101: a real execution.assign for claude under a strict profile runs the CLI only through the sandbox's
// command (`agent-exec … -- claude …` handed to the sandbox helper), and only there with the permission bypass.
func TestStrictCLIAttemptRunsOnlyInsideTheSandbox(t *testing.T) {
	t.Parallel()
	f := newWiringFixture(t, "strict", true)
	if err := f.x.Assign(t.Context(), cliAssign(true)); err != nil {
		t.Fatal(err)
	}
	kinds, _ := f.waitEnd(t)
	opened, argv := f.boxes.state()
	if opened != 1 || len(argv) != 1 {
		t.Fatalf("sandboxes opened %d, commands %v (events %v)", opened, argv, kinds)
	}
	cmd := argv[0]
	sep := slices.Index(cmd, "--")
	if !slices.Contains(cmd, "agent-exec") || sep < 0 || filepath.Base(cmd[sep+1]) != "claude" ||
		!slices.Contains(cmd[sep+1:], "--dangerously-skip-permissions") {
		t.Fatalf("sandbox command %q", cmd)
	}
	b, err := os.ReadFile(f.ran)
	if err != nil || !strings.Contains(string(b), "--dangerously-skip-permissions") {
		t.Fatalf("the CLI did not run through the sandbox with the bypass: %q, %v", b, err)
	}
}

// KIN-101: strict, but this runner cannot verify a sandbox: nothing is launched, the step waits for the network.
func TestStrictCLIAttemptWithoutASandboxNeverStarts(t *testing.T) {
	t.Parallel()
	f := newWiringFixture(t, "strict", false)
	if err := f.x.Assign(t.Context(), cliAssign(false)); err != nil {
		t.Fatal(err)
	}
	_, fails := f.waitEnd(t)
	if len(fails) != 1 || fails[0].Code != protocol.FailureNetwork {
		t.Fatalf("failures %+v, want code network", fails)
	}
	if opened, argv := f.boxes.state(); opened != 0 || len(argv) != 0 {
		t.Fatalf("sandboxes opened %d, commands %v", opened, argv)
	}
	if _, err := os.Stat(f.ran); !os.IsNotExist(err) {
		t.Fatalf("the CLI ran without its strict egress sandbox: %v", err)
	}
}

// KIN-101: a permission bypass for an attempt outside a strict sandbox is refused before anything starts.
func TestBypassOutsideTheSandboxIsRefused(t *testing.T) {
	t.Parallel()
	f := newWiringFixture(t, "monitor", true)
	if err := f.x.Assign(t.Context(), cliAssign(true)); err != nil {
		t.Fatal(err)
	}
	_, fails := f.waitEnd(t)
	if len(fails) != 1 || !strings.Contains(fails[0].Reason, "permission bypass refused") {
		t.Fatalf("failures %+v", fails)
	}
	if opened, _ := f.boxes.state(); opened != 0 {
		t.Fatalf("sandboxes opened %d", opened)
	}
	if _, err := os.Stat(f.ran); !os.IsNotExist(err) {
		t.Fatalf("the CLI ran with the bypass outside the sandbox: %v", err)
	}
}

func strictProfile() protocol.NetworkProfile {
	return protocol.NetworkProfile{Enforcement: "strict", DNS: "proxy", Proxy: &protocol.NetworkProxy{Scheme: "socks5",
		Host: "proxy.example", Port: 1080, Username: "u", Password: "p@ss"}, DiagnosticURL: "https://ip.example"}
}
