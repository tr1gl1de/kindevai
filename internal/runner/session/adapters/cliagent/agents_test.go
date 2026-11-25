//go:build unix

package cliagent

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// The test binary doubles as `ai-runner agent-exec`.
func TestMain(m *testing.M) {
	if os.Getenv("KINDEV_TEST_AGENT_EXEC") == "1" {
		os.Exit(ExecMain(os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func agentsFor(f *fixture, d Dialect, env ...string) *Agents {
	return &Agents{Adapters: map[string]*Adapter{d.Name(): f.adapter(d, env...)}, Poll: 5 * time.Millisecond,
		Save: time.Millisecond}
}

func TestPrepareWritesThePromptOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "S1-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := agentsFor(f, Claude{})
	req := domain.AgentRequest{Agent: domain.AgentClaude, Model: "sonnet", Timeout: time.Minute}
	cmd, err := g.Prepare(dir, req, "Fix the bug; $(id)", domain.PermissionsSafe)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.PromptFile != filepath.Join(dir, PromptFile) || cmd.Dir != filepath.Join(dir, "work") || cmd.Timeout != time.Minute {
		t.Fatalf("command %+v", cmd)
	}
	if b, _ := os.ReadFile(cmd.PromptFile); string(b) != "Fix the bug; $(id)" {
		t.Fatalf("prompt %q", b)
	}
	if st, _ := os.Stat(cmd.PromptFile); st.Mode().Perm() != 0o600 {
		t.Fatalf("prompt mode %v", st.Mode())
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "Fix the bug") || !strings.Contains(strings.Join(cmd.Args, " "), "--model sonnet") {
		t.Fatalf("args %q", cmd.Args)
	}
	if _, err := g.Prepare(dir, domain.AgentRequest{Agent: domain.AgentCodex, Timeout: time.Minute}, "x", domain.PermissionsSafe); !errors.Is(err, app.ErrNoAgent) {
		t.Fatalf("codex without an adapter: %v", err)
	}
	bad := req
	bad.Model = "--dangerously-skip-permissions"
	if _, err := g.Prepare(dir, bad, "x", domain.PermissionsSafe); !errors.Is(err, domain.ErrInvalidAgentRequest) {
		t.Fatalf("flag as a model: %v", err)
	}
}

// runExec runs agent-exec (the test binary) as the leader of its own process group, like the launcher does.
func runExec(t *testing.T, f *fixture, state string, timeout time.Duration, env ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := filepath.Abs("testdata/fakecli.sh")
	cmd := exec.Command(exe, "--state", state, "--prompt", f.prompt, "--timeout", timeout.String(), "--grace", "100ms", //nolint:gosec // test
		"--", bin, "-p")
	cmd.Env = append([]string{"KINDEV_TEST_AGENT_EXEC=1", "FAKE_OUT=" + f.out, "PATH=/usr/bin:/bin"}, env...)
	cmd.Dir = f.workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	return cmd
}

func TestExecRunsTheCLIWithThePromptAndRecordsItsOutputAndEnd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	state := t.TempDir()
	cmd := runExec(t, f, state, time.Minute, "FAKE_STREAM="+abs(t, "testdata/claude/read.jsonl"), "FAKE_EXIT=3",
		"FAKE_MODE=stderr", "FAKE_STDERR=boom")
	_ = cmd.Wait()
	if got := f.recorded("stdin"); !strings.Contains(got, "$(rm -rf /)") {
		t.Fatalf("stdin %q", got)
	}
	out, _ := os.ReadFile(filepath.Join(state, StdoutFile))
	want, _ := os.ReadFile("testdata/claude/read.jsonl")
	if !bytes.Equal(out, want) {
		t.Fatal("stdout file differs from the CLI's output")
	}
	if b, _ := os.ReadFile(filepath.Join(state, StderrFile)); strings.TrimSpace(string(b)) != "boom" {
		t.Fatalf("stderr %q", b)
	}
	if e, ok := ReadEnd(state); !ok || e.Code != 3 || e.TimedOut {
		t.Fatalf("end %+v %v", e, ok)
	}
}

func TestExecStopsTheCLIWhenTheTimeoutPasses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	state := t.TempDir()
	cmd := runExec(t, f, state, 200*time.Millisecond, "FAKE_MODE=hang")
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("agent-exec did not stop the hanging CLI")
	}
	if e, ok := ReadEnd(state); !ok || !e.TimedOut {
		t.Fatalf("end %+v %v", e, ok)
	}
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFollowParsesTheOutputFilesIntoEventsAndResult(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	g := &Agents{Poll: 5 * time.Millisecond, Save: time.Millisecond}
	var (
		mu     sync.Mutex
		events []domain.AgentEvent
		offs   []domain.OutputOffsets
	)
	out := &lockedBuf{}
	fl, err := g.Follow(domain.AgentClaude, state, app.AgentFollow{Stdout: out, Stderr: out,
		OnEvent:  func(e domain.AgentEvent) { mu.Lock(); events = append(events, e); mu.Unlock() },
		Progress: func(o domain.OutputOffsets) { mu.Lock(); offs = append(offs, o); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	// The files appear after the follower started (the CLI starts later).
	copyFile(t, "testdata/claude/read.jsonl", filepath.Join(state, StdoutFile))
	if err := writeEnd(state, ExecEnd{Code: 0}); err != nil {
		t.Fatal(err)
	}
	res := fl.Finish(domain.Exit{Signaled: true}, "")
	if res.Failure != nil || res.Session == "" || res.Version == "" || res.Usage.OutputTokens == 0 {
		t.Fatalf("result %+v", res)
	}
	want, _ := os.ReadFile("testdata/claude/read.jsonl")
	if out.String() != string(want) {
		t.Fatal("raw output differs")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || events[0].Kind != domain.EventSession {
		t.Fatalf("events %+v", events)
	}
	if last := offs[len(offs)-1]; last.Stdout != int64(len(want)) {
		t.Fatalf("offsets %+v", offs)
	}
}

// After a restart the follower reads the files again for the result but sends only what lies beyond From.
func TestFollowAfterARestartSendsOnlyNewOutput(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	copyFile(t, "testdata/claude/rate-limit.jsonl", filepath.Join(state, StdoutFile))
	all, _ := os.ReadFile(filepath.Join(state, StdoutFile))
	firstLine := int64(bytes.IndexByte(all, '\n') + 1)
	if err := os.WriteFile(filepath.Join(state, StderrFile), []byte("old\nnew\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Agents{Poll: 5 * time.Millisecond}
	var events []domain.AgentEvent
	out, errOut := &lockedBuf{}, &lockedBuf{}
	fl, err := g.Follow(domain.AgentClaude, state, app.AgentFollow{From: domain.OutputOffsets{Stdout: firstLine, Stderr: 4},
		Stdout: out, Stderr: errOut, OnEvent: func(e domain.AgentEvent) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	res := fl.Finish(domain.Exit{Lost: true}, "")
	if out.String() != string(all[firstLine:]) || errOut.String() != "new\n" {
		t.Fatalf("resent output: %q / %q", out.String(), errOut.String())
	}
	for _, e := range events {
		if e.Kind == domain.EventSession {
			t.Fatal("the session event of the first line was sent again")
		}
	}
	// The first line still counts for the result (the session id), and no end record means the exit decides.
	if res.Session == "" || res.Failure == nil || res.Failure.Code != domain.FailureCLILimit {
		t.Fatalf("result %+v", res)
	}
}

func TestFollowReportsTheTimeoutAgentExecRecorded(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	if err := writeEnd(state, ExecEnd{Code: -1, Signaled: true, TimedOut: true}); err != nil {
		t.Fatal(err)
	}
	fl, err := (&Agents{}).Follow(domain.AgentCodex, state, app.AgentFollow{})
	if err != nil {
		t.Fatal(err)
	}
	if res := fl.Finish(domain.Exit{Signaled: true}, ""); res.Failure == nil || res.Failure.Code != domain.FailureTimeout {
		t.Fatalf("result %+v", res)
	}
	if _, err := (&Agents{}).Follow("zcode", state, app.AgentFollow{}); err == nil {
		t.Fatal("unknown agent followed")
	}
}

func TestCappedOutputStopsAtTheLimitAndMarksTheCut(t *testing.T) {
	t.Parallel()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := &capped{f: f, left: 10, name: "stdout"}
	for _, w := range []string{"0123456", "789abc", "def"} {
		if n, err := c.Write([]byte(w)); err != nil || n != len(w) {
			t.Fatalf("write %q = %d %v", w, n, err)
		}
	}
	b, _ := os.ReadFile(f.Name())
	if !strings.HasPrefix(string(b), "0123456789\n[agent-exec: stdout cut at") || strings.Count(string(b), "cut at") != 1 {
		t.Fatalf("file %q", b)
	}
}
