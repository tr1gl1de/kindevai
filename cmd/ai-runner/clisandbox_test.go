package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/nsbox"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

func strictProfile() protocol.NetworkProfile {
	return protocol.NetworkProfile{Enforcement: "strict", DNS: "proxy", Proxy: &protocol.NetworkProxy{Scheme: "socks5",
		Host: "proxy.example", Port: 1080, Username: "u", Password: "p@ss"}, DiagnosticURL: "https://ip.example"}
}

// KIN-31 × KIN-32: a CLI run with a strict profile is wrapped into `ai-runner sandbox-run`; the CLI's own command
// line travels to it in RunEnv, never as arguments.
func TestCLISandboxWrapsTheCommand(t *testing.T) {
	t.Parallel()
	m := &nsbox.Manager{Program: "/usr/bin/ai-runner", Args: []string{"sandbox-exec"}, DataDir: t.TempDir()}
	ok := func(context.Context) (bool, string) { return true, "" }
	s := cliSandbox{supported: ok, boxes: m, profile: strictProfile(), dir: t.TempDir()}
	if !s.StrictEgress(t.Context()) {
		t.Fatal("StrictEgress = false on a verified runner")
	}
	in := cliagent.Command{Path: "/usr/bin/claude", Args: []string{"-p", "--dangerously-skip-permissions"},
		Env: []string{"HOME=/home/u"}, Dir: "/var/lib/kr/workspaces/w"}
	out, err := s.Wrap(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != m.Program || !slices.Equal(out.Args, []string{"sandbox-run"}) || out.Dir != in.Dir {
		t.Fatalf("wrapped %+v", out)
	}
	var rs nsbox.RunSpec
	for _, kv := range out.Env {
		if strings.Contains(kv, "p@ss") {
			t.Fatalf("the proxy password is in the environment: %s", kv)
		}
		if v, found := strings.CutPrefix(kv, nsbox.RunEnv+"="); found {
			b, err := os.ReadFile(v)
			if err != nil {
				t.Fatal(err)
			}
			if fi, _ := os.Stat(v); fi.Mode().Perm() != 0o600 {
				t.Fatalf("run description mode %v", fi.Mode())
			}
			if err := json.Unmarshal(b, &rs); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(rs.Argv, []string{"/usr/bin/claude", "-p", "--dangerously-skip-permissions"}) ||
		rs.Request.Workdir != in.Dir || rs.Request.Dir != s.dir || rs.Request.Profile.Enforcement != "strict" ||
		!slices.Equal(rs.HelperArgs, []string{"sandbox-exec"}) {
		t.Fatalf("run spec %+v", rs)
	}
}

// Unsupported here: no verified strict egress, and Wrap refuses (the CLI is never started on the host).
func TestCLISandboxUnsupportedRefuses(t *testing.T) {
	t.Parallel()
	no := func(context.Context) (bool, string) { return false, "unsupported on darwin" }
	s := cliSandbox{supported: no, boxes: &nsbox.Manager{Program: "/x"}, profile: strictProfile()}
	if s.StrictEgress(t.Context()) {
		t.Fatal("StrictEgress = true on an unsupported runner")
	}
	if _, err := s.Wrap(t.Context(), cliagent.Command{Path: "/usr/bin/claude"}); !errors.Is(err, egress.ErrUnsupported) {
		t.Fatalf("Wrap = %v", err)
	}
}

func TestNewCLISandbox(t *testing.T) {
	t.Parallel()
	cfg := func(enf string, ok bool) func() (protocol.ConfigUpdate, bool, error) {
		return func() (protocol.ConfigUpdate, bool, error) {
			p := strictProfile()
			p.Enforcement = enf
			return protocol.ConfigUpdate{Network: protocol.NetworkConfig{Default: p}}, ok, nil
		}
	}
	s := &egress.Sandboxes{Config: cfg("monitor", true), Boxes: &nsbox.Manager{}}
	if sb, err := newCLISandbox(t.Context(), s, "p1", "claude", "/d"); sb != nil || err != nil {
		t.Fatalf("not strict: %v, %v", sb, err)
	}
	s = &egress.Sandboxes{Config: cfg("strict", false), Boxes: &nsbox.Manager{}}
	if sb, err := newCLISandbox(t.Context(), s, "p1", "claude", "/d"); sb != nil || err == nil {
		t.Fatalf("no configuration: %v, %v", sb, err)
	}
}

// KIN-31 follow-up (KIN-83, KIN-88): only exit ExitUnverified with the refusal `sandbox-run` recorded outside the
// sandbox is the sandbox refusing the run (the step then waits for the network); a CLI exiting 121 on its own, even
// with UnverifiedMarker on stderr, is a CLI error.
func TestCLISandboxRecognisesAnUnverifiedRun(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	attempt := filepath.Join(data, "attempts", "a1")
	if err := os.MkdirAll(attempt, 0o700); err != nil {
		t.Fatal(err)
	}
	ok := func(context.Context) (bool, string) { return true, "" }
	s := cliSandbox{supported: ok, boxes: &nsbox.Manager{Program: "/x", DataDir: data}, profile: strictProfile(), dir: attempt}
	wrap := func(record bool) cliagent.Command {
		t.Helper()
		c, err := s.Wrap(t.Context(), cliagent.Command{Path: "/usr/bin/claude", Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if record { // what `sandbox-run` does before the CLI could start
			for _, kv := range c.Env {
				if desc, found := strings.CutPrefix(kv, nsbox.RunEnv+"="); found {
					st := filepath.Join(data, nsbox.StatusDir, strings.TrimSuffix(filepath.Base(desc), ".json")+".unverified")
					if err := os.WriteFile(st, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		return c
	}
	for _, c := range []struct {
		record bool
		exit   domain.Exit
		want   bool
	}{
		{true, domain.Exit{Code: nsbox.ExitUnverified}, true},
		{false, domain.Exit{Code: nsbox.ExitUnverified}, false}, // the CLI's own 121 (and marker)
		{true, domain.Exit{Code: 1}, false},
		{true, domain.Exit{Signaled: true}, false},
	} {
		if got := s.Refused(wrap(c.record), c.exit); got != c.want {
			t.Errorf("Refused(record %v, %+v) = %v, want %v", c.record, c.exit, got, c.want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(data, nsbox.StatusDir, "*")); len(left) != 0 {
		t.Fatalf("status left behind: %v", left)
	}
}
