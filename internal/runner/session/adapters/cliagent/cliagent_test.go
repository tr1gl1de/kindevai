//go:build unix

package cliagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
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
		Env: append([]string{"FAKE_OUT=" + f.out}, env...)}}
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

// ran is what a CLI run left: its result, raw output and parsed events.
type ran struct {
	res            domain.AgentResult
	stdout, stderr string
	events         []domain.AgentEvent
}

// runCLI runs the adapter's command for req the way the daemon does (KIN-79, the one production path since
// KIN-102): `agent-exec` (the test binary) leads its own process group with the command's environment and working
// directory and the prompt file on the CLI's stdin, and Agents.Follow parses the output files it writes. cancel,
// when set, runs once the CLI produced output, with the group's id; it stops the group as the executor does and
// returns the cancel reason.
func runCLI(t *testing.T, a *Adapter, req domain.AgentRequest, p domain.Permissions, cancel func(pgid int) string) ran {
	t.Helper()
	c, err := a.Command(req, p)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	args := []string{"--state", state, "--prompt", req.PromptFile, "--timeout", req.Timeout.String(), "--grace", "200ms",
		"--", c.Path}
	cmd := exec.Command(exe, append(args, c.Args...)...) //nolint:gosec,noctx // test; killed with its group
	cmd.Env, cmd.Dir = append([]string{"KINDEV_TEST_AGENT_EXEC=1"}, c.Env...), c.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	var (
		mu  sync.Mutex
		r   ran
		out lockedBuf
		eo  lockedBuf
	)
	fl, err := (&Agents{Adapters: map[string]*Adapter{a.Dialect.Name(): a}, Poll: 5 * time.Millisecond,
		Save: time.Millisecond}).Follow(a.Dialect.Name(), state, app.AgentFollow{Stdout: &out, Stderr: &eo,
		OnEvent: func(e domain.AgentEvent) { mu.Lock(); r.events = append(r.events, e); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	reason := ""
	if cancel != nil {
		deadline := time.Now().Add(20 * time.Second)
		for out.String() == "" {
			if time.Now().After(deadline) {
				t.Fatal("the CLI wrote nothing")
			}
			time.Sleep(10 * time.Millisecond)
		}
		reason = cancel(pgid)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end")
	}
	exit := domain.Exit{Code: cmd.ProcessState.ExitCode()}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		exit = domain.Exit{Signaled: true}
	}
	res := fl.Finish(exit, reason)
	mu.Lock()
	defer mu.Unlock()
	r.res, r.stdout, r.stderr = res, out.String(), eo.String()
	return r
}

// args is the argv the adapter builds for req in permission mode p.
func args(t *testing.T, a *Adapter, req domain.AgentRequest, p domain.Permissions) []string {
	t.Helper()
	c, err := a.Command(req, p)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	return c.Args
}

// AC: Claude Code runs headless with stream-json; the prompt comes on stdin from a file outside the workspace; the
// stream is parsed into typed events and the result carries the native session id and usage.
func TestClaudeRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := runCLI(t, f.adapter(Claude{}, stream("claude/read.jsonl")), f.request(domain.AgentClaude), domain.PermissionsSafe, nil)
	if ok, reason := r.res.Outcome(); !ok {
		t.Fatalf("run failed: %s", reason)
	}
	if r.res.Session != "8bc06756-ac80-4061-889c-c0ee7d40571c" || r.res.Text != "kindevai fixture" || r.res.Usage.Turns != 2 {
		t.Fatalf("result = %+v", r.res)
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
	for _, e := range r.events {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []domain.EventKind{domain.EventSession, domain.EventToolUse, domain.EventText, domain.EventResult}) {
		t.Fatalf("events = %v", kinds)
	}
	if want, _ := os.ReadFile("testdata/claude/read.jsonl"); r.stdout != string(want) {
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
			if got := args(t, f.adapter(c.d), req, domain.PermissionsSafe); !slices.Equal(got, c.want) {
				t.Fatalf("argv = %q, want %q", got, c.want)
			}
		})
	}
}

// AC: Codex runs `exec --json` with its own sandbox (safe mode); the stream is parsed.
func TestCodexRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := runCLI(t, f.adapter(Codex{}, stream("codex/success.jsonl")), f.request(domain.AgentCodex), domain.PermissionsSafe, nil)
	if ok, reason := r.res.Outcome(); !ok || r.res.Session != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("result = %+v (%s)", r.res, reason)
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
	runCLI(t, f.adapter(Claude{}, stream("claude/read.jsonl")), f.request(domain.AgentClaude), domain.PermissionsSafe, nil)
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
	if _, err := a.Command(f.request(domain.AgentClaude), domain.PermissionsSafe); err == nil {
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
	if _, err := a.Command(f.request(domain.AgentClaude), domain.PermissionsSafe); err == nil {
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
	if _, err := f.adapter(Claude{}).Command(req, domain.PermissionsSafe); !errors.Is(err, domain.ErrInvalidAgentRequest) {
		t.Fatalf("a prompt linked into the workspace: %v", err)
	}
}

// AC (owner decision 24.11.2026): the permission bypass drops the CLI's own permission prompts and sandbox. Only the
// session executor chooses it, and only for a run inside the verified strict egress sandbox (domain.ChoosePermissions,
// session/app TestPermissionBypassOnlyInsideTheStrictSandbox).
func TestPermissionBypassArgs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		d    Dialect
		flag string
	}{
		{Claude{}, "--dangerously-skip-permissions"},
		{Codex{}, "--dangerously-bypass-approvals-and-sandbox"},
	} {
		f := newFixture(t)
		argv := args(t, f.adapter(c.d), f.request(c.d.Name()), domain.PermissionsBypass)
		if !slices.Contains(argv, c.flag) || slices.Contains(argv, "--permission-mode") || slices.Contains(argv, "--sandbox") {
			t.Fatalf("%s argv = %q", c.d.Name(), argv)
		}
	}
}

// AC: timeouts: the process group gets SIGTERM, then SIGKILL after the grace period (the fake ignores SIGTERM); the
// session id seen before the timeout is kept (the run can be resumed).
func TestTimeoutKillsTheGroup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	req := f.request(domain.AgentClaude)
	req.Timeout = 300 * time.Millisecond
	r := runCLI(t, f.adapter(Claude{}, stream("claude/read.jsonl"), "FAKE_MODE=hang"), req, domain.PermissionsSafe, nil)
	if r.res.Failure == nil || r.res.Failure.Code != domain.FailureTimeout || !r.res.Exit.Signaled {
		t.Fatalf("result = %+v", r.res)
	}
	if r.res.Session == "" {
		t.Fatal("the session id seen before the timeout is lost")
	}
}

// AC: a cancel stops the run's process group (SIGTERM, SIGKILL after the grace period, as the executor does) and the
// run ends cancelled with its reason.
func TestCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := runCLI(t, f.adapter(Claude{}, stream("claude/read.jsonl"), "FAKE_MODE=hang"), f.request(domain.AgentClaude),
		domain.PermissionsSafe, func(pgid int) string {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			time.AfterFunc(300*time.Millisecond, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			return "runner_disabled"
		})
	if _, reason := r.res.Outcome(); r.res.Failure == nil || r.res.Failure.Code != domain.FailureCancelled ||
		reason != "cancelled: runner_disabled" {
		t.Fatalf("result = %+v (%s)", r.res, reason)
	}
}

// A process the CLI leaves behind holding stdout neither keeps the run open nor survives it.
func TestLeftoverProcessesAreKilled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := runCLI(t, f.adapter(Claude{}, stream("claude/read.jsonl"), "FAKE_MODE=leftover"), f.request(domain.AgentClaude),
		domain.PermissionsSafe, nil)
	if ok, reason := r.res.Outcome(); !ok {
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
	r := runCLI(t, f.adapter(Codex{}, "FAKE_MODE=stderr", "FAKE_EXIT=1",
		"FAKE_STDERR=ERROR: exceeded retry limit, last status: 429 Too Many Requests"), f.request(domain.AgentCodex),
		domain.PermissionsSafe, nil)
	if r.res.Failure == nil || r.res.Failure.Code != domain.FailureCLILimit || r.res.Failure.Cause != domain.CauseRateLimit {
		t.Fatalf("failure = %+v", r.res.Failure)
	}
	if !strings.Contains(r.stderr, "429") {
		t.Fatal("stderr is not passed through")
	}
}

// AC: a logout reported in the stream fails the run as cli_limit/auth (the step goes BLOCKED(cli_limit)).
func TestClaudeLogoutIsCLILimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := runCLI(t, f.adapter(Claude{}, stream("claude/auth.jsonl"), "FAKE_EXIT=1"), f.request(domain.AgentClaude),
		domain.PermissionsSafe, nil)
	if _, reason := r.res.Outcome(); r.res.Failure == nil || r.res.Failure.Code != domain.FailureCLILimit ||
		r.res.Failure.Cause != domain.CauseAuth || !strings.Contains(reason, "Not logged in") {
		t.Fatalf("failure = %+v", r.res.Failure)
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
	r := runCLI(t, a, f.request(domain.AgentClaude), domain.PermissionsSafe, nil)
	if ok, reason := r.res.Outcome(); !ok || r.res.Text != "kindevai fixture" {
		t.Fatalf("result = %+v (%s)", r.res, reason)
	}
}

func TestCommandValidates(t *testing.T) {
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a, r := f.adapter(Claude{}), f.request(domain.AgentClaude)
			c.mutate(a, &r)
			if _, err := a.Command(r, domain.PermissionsSafe); err == nil {
				t.Fatal("Command accepted it")
			}
		})
	}
}

// A prompt that is not a regular file never reaches the CLI: agent-exec refuses to start it.
func TestExecRefusesAPromptThatIsNotAFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := filepath.Abs("testdata/fakecli.sh")
	state := t.TempDir()
	cmd := exec.Command(exe, "--state", state, "--prompt", f.home, "--timeout", "1m", "--", bin) //nolint:gosec,noctx // test
	cmd.Env = []string{"KINDEV_TEST_AGENT_EXEC=1", "FAKE_OUT=" + f.out, "PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = cmd.Run()
	if e, ok := ReadEnd(state); !ok || e.Code != 127 {
		t.Fatalf("end %+v %v, want 127", e, ok)
	}
	if _, err := os.Stat(filepath.Join(f.out, "argv")); err == nil {
		t.Fatal("the CLI was started")
	}
}

// KIN-35: a read-only run (a reviewer) never gets the write or shell tools, nor the bypass.
func TestReadOnlyArgs(t *testing.T) {
	t.Parallel()
	req := domain.AgentRequest{}
	claude := Claude{}.Args(req, domain.PermissionsReadOnly)
	if want := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "default", "--disallowedTools",
		"Edit,MultiEdit,Write,NotebookEdit,Bash", "--setting-sources", "user", "--settings", `{"disableAllHooks":true}`}; !slices.Equal(claude, want) {
		t.Fatalf("claude argv = %q", claude)
	}
	if codex := (Codex{}).Args(req, domain.PermissionsReadOnly); !slices.Contains(codex, "read-only") || slices.Contains(codex, "workspace-write") {
		t.Fatalf("codex argv = %q", codex)
	}
}
