//go:build unix

package cliagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

type fixture struct {
	t       *testing.T
	out     string // where the fake CLI records argv, env and stdin
	workdir string
	prompt  string
	home    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, out: filepath.Join(root, "out"), workdir: filepath.Join(root, "ws"),
		prompt: filepath.Join(root, "attempt", "prompt.md"), home: filepath.Join(root, "home")}
	for _, d := range []string{f.out, f.workdir, filepath.Dir(f.prompt), f.home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Shell metacharacters: the prompt reaches the CLI only on stdin, never through a shell or argv.
	if err := os.WriteFile(f.prompt, []byte("Fix the bug; $(rm -rf /) `id` --dangerously-skip-permissions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) adapter(d Dialect, env ...string) *Adapter {
	bin, err := filepath.Abs("testdata/fakecli.sh")
	if err != nil {
		f.t.Fatal(err)
	}
	return &Adapter{Dialect: d, Config: Config{Binary: bin, Home: f.home, Path: "/usr/bin:/bin",
		Env: append([]string{"FAKE_OUT=" + f.out}, env...), Grace: 200 * time.Millisecond}}
}

func (f *fixture) request(agent string) domain.AgentRequest {
	return domain.AgentRequest{Agent: agent, Workdir: f.workdir, PromptFile: f.prompt, Timeout: time.Minute}
}

func (f *fixture) recorded(name string) string {
	b, err := os.ReadFile(filepath.Join(f.out, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func stream(name string) string {
	p, _ := filepath.Abs(filepath.Join("testdata", name))
	return "FAKE_STREAM=" + p
}

type collector struct {
	mu  sync.Mutex
	evs []domain.AgentEvent
	ch  chan domain.AgentEvent
}

func newCollector() *collector { return &collector{ch: make(chan domain.AgentEvent, 64)} }

func (c *collector) on(e domain.AgentEvent) {
	c.mu.Lock()
	c.evs = append(c.evs, e)
	c.mu.Unlock()
	select {
	case c.ch <- e:
	default:
	}
}

func start(t *testing.T, a *Adapter, req domain.AgentRequest, h Hooks) *Run {
	t.Helper()
	r, err := a.Start(t.Context(), req, h)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { r.Cancel("test cleanup"); r.Wait() })
	return r
}

func waitResult(t *testing.T, r *Run) domain.AgentResult {
	t.Helper()
	select {
	case <-r.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not end")
	}
	return r.Wait()
}

// AC: Claude Code runs headless with stream-json; the prompt comes on stdin from a file outside the workspace; the
// stream is parsed into typed events and the result carries the native session id and usage.
func TestClaudeRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := newCollector()
	var raw bytes.Buffer
	r := start(t, f.adapter(Claude{}, stream("claude/read.jsonl")), f.request(domain.AgentClaude),
		Hooks{OnEvent: c.on, Stdout: &raw})
	res := waitResult(t, r)
	if ok, reason := res.Outcome(); !ok {
		t.Fatalf("run failed: %s", reason)
	}
	if res.Session != "8bc06756-ac80-4061-889c-c0ee7d40571c" || res.Text != "kindevai fixture" || res.Usage.Turns != 2 {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits",
		"--setting-sources", "user", "--settings", `{"disableAllHooks":true}`}
	if got := strings.Split(strings.TrimSuffix(f.recorded("argv"), "\n"), "\n"); !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if got, _ := os.ReadFile(f.prompt); f.recorded("stdin") != string(got) {
		t.Fatalf("stdin = %q", f.recorded("stdin"))
	}
	kinds := []domain.EventKind{}
	for _, e := range c.evs {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []domain.EventKind{domain.EventSession, domain.EventToolUse, domain.EventText, domain.EventResult}) {
		t.Fatalf("events = %v", kinds)
	}
	want2, _ := os.ReadFile("testdata/claude/read.jsonl")
	if raw.String() != string(want2) {
		t.Fatal("raw stdout is not passed through unchanged")
	}
}

// AC: resume continues a native session; the model is passed as a validated argument.
func TestResumeArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    Dialect
		want []string
	}{
		{Claude{}, []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits",
			"--setting-sources", "user", "--settings", `{"disableAllHooks":true}`, "--model", "sonnet", "--resume", "sess-1"}},
		{Codex{}, []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "--model", "sonnet",
			"resume", "sess-1", "-"}},
	}
	for _, c := range cases {
		t.Run(c.d.Name(), func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			req := f.request(c.d.Name())
			req.Resume, req.Model = "sess-1", "sonnet"
			waitResult(t, start(t, f.adapter(c.d), req, Hooks{}))
			if got := strings.Split(strings.TrimSuffix(f.recorded("argv"), "\n"), "\n"); !slices.Equal(got, c.want) {
				t.Fatalf("argv = %q, want %q", got, c.want)
			}
		})
	}
}

// AC: Codex runs `exec --json` with its own sandbox (safe mode); the stream is parsed.
func TestCodexRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := start(t, f.adapter(Codex{}, stream("codex/success.jsonl")), f.request(domain.AgentCodex), Hooks{})
	res := waitResult(t, r)
	if ok, reason := res.Outcome(); !ok || res.Session != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("result = %+v (%s)", res, reason)
	}
	want := []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "-"}
	if got := strings.Fields(f.recorded("argv")); !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// AC: the CLI's environment is the runner's minimal one: no inherited variables (Control Plane secrets), HOME is the
// runner's (the CLI's login stays there, never in the workspace).
func TestEnvironmentIsMinimal(t *testing.T) {
	// Not parallel: sets a variable in the test process to prove the CLI does not inherit it.
	t.Setenv("KINDEV_SECRET_TOKEN", "s3cr3t")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	f := newFixture(t)
	waitResult(t, start(t, f.adapter(Claude{}, stream("claude/read.jsonl")), f.request(domain.AgentClaude), Hooks{}))
	env := f.recorded("env")
	for _, leak := range []string{"s3cr3t", "sk-leak", "KINDEV_SECRET_TOKEN"} {
		if strings.Contains(env, leak) {
			t.Fatalf("the CLI inherited %s", leak)
		}
	}
	for _, want := range []string{"HOME=" + f.home, "PATH=/usr/bin:/bin"} {
		if !slices.Contains(strings.Split(env, "\n"), want) {
			t.Fatalf("env lacks %s:\n%s", want, env)
		}
	}
	if wd, _ := filepath.EvalSymlinks(f.workdir); strings.TrimSpace(f.recorded("pwd")) != wd {
		t.Fatalf("the CLI ran in %s, not the workspace", f.recorded("pwd"))
	}
}

func TestHomeInsideTheWorkspaceIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.adapter(Claude{})
	a.Config.Home = filepath.Join(f.workdir, "home")
	if _, err := a.Start(t.Context(), f.request(domain.AgentClaude), Hooks{}); err == nil {
		t.Fatal("a HOME inside the workspace would expose the CLI's login to the agent")
	}
}

// A symlink must not smuggle HOME or the prompt into the workspace.
func TestSymlinksAreResolved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(f.workdir, link); err != nil {
		t.Fatal(err)
	}
	a := f.adapter(Claude{})
	a.Config.Home = link
	if _, err := a.Start(t.Context(), f.request(domain.AgentClaude), Hooks{}); err == nil {
		t.Fatal("a HOME linked into the workspace was accepted")
	}
	inside := filepath.Join(f.workdir, "prompt.md")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plink := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.Symlink(inside, plink); err != nil {
		t.Fatal(err)
	}
	req := f.request(domain.AgentClaude)
	req.PromptFile = plink
	if _, err := f.adapter(Claude{}).Start(t.Context(), req, Hooks{}); !errors.Is(err, domain.ErrInvalidAgentRequest) {
		t.Fatalf("a prompt linked into the workspace: %v", err)
	}
}

// A cancel that arrives after the CLI ended does not turn a success into a failure.
func TestCancelAfterExitKeepsTheResult(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := start(t, f.adapter(Claude{}, stream("claude/read.jsonl")), f.request(domain.AgentClaude), Hooks{})
	res := waitResult(t, r)
	r.Cancel("late")
	if ok, reason := r.Wait().Outcome(); !ok || res.Failure != nil {
		t.Fatalf("late cancel changed the result: %s", reason)
	}
}

type fakeSandbox struct {
	strict  bool
	wrapped bool
	mu      sync.Mutex
}

func (s *fakeSandbox) StrictEgress(context.Context) bool { return s.strict }
func (s *fakeSandbox) Wrap(_ context.Context, c Command) (Command, error) {
	s.mu.Lock()
	s.wrapped = true
	s.mu.Unlock()
	c.Env = append(c.Env, "SANDBOXED=1")
	return c, nil
}

// AC (owner decision 24.11.2026): the permission bypass only inside the verified strict-egress sandbox, otherwise
// the CLI is refused (never started on the host with the bypass).
func TestPermissionBypass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		d       Dialect
		sandbox *fakeSandbox
		flag    string
		refused bool
	}{
		{"claude on the host", Claude{}, nil, "", true},
		{"claude in an unverified sandbox", Claude{}, &fakeSandbox{}, "", true},
		{"claude in the strict sandbox", Claude{}, &fakeSandbox{strict: true}, "--dangerously-skip-permissions", false},
		{"codex on the host", Codex{}, nil, "", true},
		{"codex in the strict sandbox", Codex{}, &fakeSandbox{strict: true}, "--dangerously-bypass-approvals-and-sandbox", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			a := f.adapter(c.d)
			if c.sandbox != nil {
				a.Config.Sandbox = c.sandbox
			}
			req := f.request(c.d.Name())
			req.BypassPermissions = true
			r, err := a.Start(t.Context(), req, Hooks{})
			if c.refused {
				if !errors.Is(err, domain.ErrBypassWithoutSandbox) {
					t.Fatalf("Start = %v, want ErrBypassWithoutSandbox", err)
				}
				if _, statErr := os.Stat(filepath.Join(f.out, "argv")); statErr == nil {
					t.Fatal("the CLI was started")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			waitResult(t, r)
			argv := strings.Fields(f.recorded("argv"))
			if !slices.Contains(argv, c.flag) || slices.Contains(argv, "--permission-mode") || slices.Contains(argv, "--sandbox") {
				t.Fatalf("argv = %q", argv)
			}
			if !strings.Contains(f.recorded("env"), "SANDBOXED=1") || !c.sandbox.wrapped {
				t.Fatal("the command was not wrapped by the sandbox")
			}
		})
	}
}

// Safe mode in a sandbox still wraps the command (the strict-egress sandbox confines it either way).
func TestSafeModeInSandboxIsWrapped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.adapter(Claude{}, stream("claude/read.jsonl"))
	sb := &fakeSandbox{}
	a.Config.Sandbox = sb
	waitResult(t, start(t, a, f.request(domain.AgentClaude), Hooks{}))
	if !sb.wrapped || !strings.Contains(f.recorded("argv"), "acceptEdits") {
		t.Fatal("safe mode run was not wrapped")
	}
}

// AC: timeouts: the process group gets SIGTERM, then SIGKILL after the grace period (the fake ignores SIGTERM).
func TestTimeoutKillsTheGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	req := f.request(domain.AgentClaude)
	req.Timeout = 300 * time.Millisecond
	r := start(t, f.adapter(Claude{}, stream("claude/read.jsonl"), "FAKE_MODE=hang"), req, Hooks{})
	res := waitResult(t, r)
	if res.Failure == nil || res.Failure.Code != domain.FailureTimeout || !res.Exit.Signaled {
		t.Fatalf("result = %+v", res)
	}
	if res.Session == "" {
		t.Fatal("the session id seen before the timeout is kept (the run can be resumed)")
	}
}

// AC: cancel stops the run's process group.
func TestCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := newCollector()
	r := start(t, f.adapter(Claude{}, stream("claude/read.jsonl"), "FAKE_MODE=hang"), f.request(domain.AgentClaude),
		Hooks{OnEvent: c.on})
	select {
	case <-c.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	r.Cancel("runner_disabled")
	res := waitResult(t, r)
	if _, reason := res.Outcome(); res.Failure == nil || res.Failure.Code != domain.FailureCancelled ||
		reason != "cancelled: runner_disabled" {
		t.Fatalf("result = %+v", res)
	}
}

func TestContextCancelStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	r, err := f.adapter(Claude{}, "FAKE_MODE=hang").Start(ctx, f.request(domain.AgentClaude), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if res := waitResult(t, r); res.Failure == nil || res.Failure.Code != domain.FailureCancelled {
		t.Fatalf("result = %+v", res.Failure)
	}
}

// A process the CLI leaves behind holding stdout neither keeps the run open nor survives it.
func TestLeftoverProcessesAreKilled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := start(t, f.adapter(Claude{}, stream("claude/read.jsonl"), "FAKE_MODE=leftover"), f.request(domain.AgentClaude), Hooks{})
	if ok, reason := waitResult(t, r).Outcome(); !ok {
		t.Fatal(reason)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(f.recorded("leftover")))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("leftover process %d still runs", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AC: a rate limit or logout reported only on stderr with a failing exit is cli_limit.
func TestStderrLimitIsCLILimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var errOut bytes.Buffer
	r := start(t, f.adapter(Codex{}, "FAKE_MODE=stderr", "FAKE_EXIT=1",
		"FAKE_STDERR=ERROR: exceeded retry limit, last status: 429 Too Many Requests"), f.request(domain.AgentCodex),
		Hooks{Stderr: &errOut})
	res := waitResult(t, r)
	if res.Failure == nil || res.Failure.Code != domain.FailureCLILimit || res.Failure.Cause != domain.CauseRateLimit {
		t.Fatalf("failure = %+v", res.Failure)
	}
	if !strings.Contains(errOut.String(), "429") {
		t.Fatal("stderr is not passed through")
	}
}

// AC: a logout reported in the stream fails the run as cli_limit/auth (the step goes BLOCKED(cli_limit)).
func TestClaudeLogoutIsCLILimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := start(t, f.adapter(Claude{}, stream("claude/auth.jsonl"), "FAKE_EXIT=1"), f.request(domain.AgentClaude), Hooks{})
	res := waitResult(t, r)
	if _, reason := res.Outcome(); res.Failure == nil || res.Failure.Code != domain.FailureCLILimit ||
		res.Failure.Cause != domain.CauseAuth || !strings.Contains(reason, "Not logged in") {
		t.Fatalf("failure = %+v", res.Failure)
	}
}

// Resource limits: an oversized output line is cut (not buffered without bound) and the run goes on.
func TestOversizedLineIsBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	big := filepath.Join(t.TempDir(), "big.jsonl")
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 3<<20) + `"}]}}` + "\n"
	rest, _ := os.ReadFile("testdata/claude/read.jsonl")
	if err := os.WriteFile(big, append([]byte(line), rest...), 0o600); err != nil {
		t.Fatal(err)
	}
	a := f.adapter(Claude{}, "FAKE_STREAM="+big)
	a.Config.MaxLine = 1 << 20
	res := waitResult(t, start(t, a, f.request(domain.AgentClaude), Hooks{}))
	if ok, reason := res.Outcome(); !ok || res.Text != "kindevai fixture" {
		t.Fatalf("result = %+v (%s)", res, reason)
	}
}

func TestStartValidates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cases := []struct {
		name   string
		mutate func(a *Adapter, r *domain.AgentRequest)
	}{
		{"invalid request", func(_ *Adapter, r *domain.AgentRequest) { r.Resume = "--evil" }},
		{"agent of another dialect", func(_ *Adapter, r *domain.AgentRequest) { r.Agent = domain.AgentCodex }},
		{"relative binary", func(a *Adapter, _ *domain.AgentRequest) { a.Config.Binary = "claude" }},
		{"missing prompt file", func(_ *Adapter, r *domain.AgentRequest) { r.PromptFile = filepath.Join(f.home, "none") }},
		{"prompt is a directory", func(_ *Adapter, r *domain.AgentRequest) { r.PromptFile = f.home }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a, r := f.adapter(Claude{}), f.request(domain.AgentClaude)
			c.mutate(a, &r)
			if run, err := a.Start(t.Context(), r, Hooks{}); err == nil {
				run.Cancel("test")
				run.Wait()
				t.Fatal("Start accepted it")
			}
		})
	}
}

// refusingSandbox ends the wrapped command like `ai-runner sandbox-run` whose sandbox did not verify: a marker on
// stderr and exit code 121, the CLI never started.
type refusingSandbox struct{ marker string }

func (refusingSandbox) StrictEgress(context.Context) bool { return true }
func (s refusingSandbox) Wrap(_ context.Context, c Command) (Command, error) {
	return Command{Path: "/bin/sh", Args: []string{"-c", "echo \"$0: egress ip mismatch\" >&2; exit 121", s.marker},
		Env: c.Env, Dir: c.Dir}, nil
}
func (s refusingSandbox) Refused(exit domain.Exit, stderrTail string) bool {
	return exit.Code == 121 && strings.Contains(stderrTail, "kindev: strict egress sandbox not verified")
}

// KIN-31 follow-up (KIN-83): a CLI run whose strict egress sandbox did not verify fails with code network (the step
// waits for the network, no retry budget used), not as a CLI error; the same exit without the marker stays a CLI
// error.
func TestUnverifiedSandboxFailsWithNetwork(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string]domain.FailureCode{
		"kindev: strict egress sandbox not verified": domain.FailureNetwork,
		"some CLI": domain.FailureCLI,
	} {
		f := newFixture(t)
		a := f.adapter(Claude{})
		a.Config.Sandbox = refusingSandbox{marker: marker}
		res := waitResult(t, start(t, a, f.request(domain.AgentClaude), Hooks{}))
		if res.Failure == nil || res.Failure.Code != want || res.Exit.Code != 121 {
			t.Fatalf("%s: result %+v %+v, want %s", marker, res, res.Failure, want)
		}
	}
}
