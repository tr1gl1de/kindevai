//go:build unix

package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// agentRun prepares a fake Claude CLI attempt (KIN-79): the launcher starts agent-exec (the test binary), which
// runs the fake CLI replaying a recorded stream into the attempt's output files.
func agentRun(t *testing.T, stream string, env ...string) (*cliagent.Agents, string, app.AgentCommand) {
	t.Helper()
	root := t.TempDir()
	state, work, home, out := filepath.Join(root, "S1-1"), filepath.Join(root, "ws"), filepath.Join(root, "home"),
		filepath.Join(root, "out")
	for _, d := range []string{state, work, home, out} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin, _ := filepath.Abs("../cliagent/testdata/fakecli.sh")
	st, _ := filepath.Abs(stream)
	g := &cliagent.Agents{Poll: 5 * time.Millisecond, Save: time.Millisecond, Adapters: map[string]*cliagent.Adapter{
		domain.AgentClaude: {Dialect: cliagent.Claude{}, Config: cliagent.Config{Binary: bin, Home: home, Path: "/usr/bin:/bin",
			Env: append([]string{"FAKE_OUT=" + out, "FAKE_STREAM=" + st}, env...)}}}}
	cmd, err := g.Prepare(state, domain.AgentRequest{Agent: domain.AgentClaude, Workdir: work, Timeout: time.Minute},
		"Fix the bug", domain.PermissionsSafe)
	if err != nil {
		t.Fatal(err)
	}
	return g, state, cmd
}

func TestLaunchRunsTheCLIThroughAgentExec(t *testing.T) {
	t.Parallel()
	g, state, cmd := agentRun(t, "../cliagent/testdata/claude/read.jsonl")
	l := launcher(t)
	out := &syncBuf{}
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: state, Workdir: cmd.Dir, Attempt: 1, Stdout: out, Stderr: out, Agent: &cmd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Signal(true) })
	fl, err := g.Follow(domain.AgentClaude, state, app.AgentFollow{Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	res := fl.Finish(p.Wait(), "")
	if res.Failure != nil || res.Session == "" || res.Usage.OutputTokens == 0 {
		t.Fatalf("result %+v (wrapper output %q)", res, out.String())
	}
	if !strings.Contains(out.String(), `"type":"result"`) {
		t.Fatal("the CLI's raw output was not streamed")
	}
}

// A runner restart: the attempt's process is adopted by its record and its output read on from the recorded
// offsets; the run's result is still complete.
func TestAdoptedCLIAttemptIsReadOnAfterARestart(t *testing.T) {
	t.Parallel()
	g, state, cmd := agentRun(t, "../cliagent/testdata/claude/read.jsonl", "FAKE_MODE=hang")
	cmd.Timeout = 500 * time.Millisecond // the hanging CLI is stopped by agent-exec's timeout, runner or not
	l := launcher(t)
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: state, Workdir: cmd.Dir, Attempt: 1, Stdout: &syncBuf{},
		Stderr: &syncBuf{}, Agent: &cmd})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Signal(true) })
	// The first runner process read part of the output, then "restarted".
	rec := domain.Record{Step: "S1", Fence: 1, PID: p.PID(), PGID: p.PGID(), Start: p.StartTime(), Agent: domain.AgentClaude,
		Output: domain.OutputOffsets{Stdout: 10}}
	adopted := l.Adopt(rec, state)
	out := &syncBuf{}
	fl, err := g.Follow(domain.AgentClaude, state, app.AgentFollow{From: rec.Output, Stdout: out})
	if err != nil {
		t.Fatal(err)
	}
	res := fl.Finish(adopted.Wait(), "")
	if res.Failure == nil || res.Failure.Code != domain.FailureTimeout || res.Session == "" {
		t.Fatalf("result %+v", res)
	}
	all, _ := os.ReadFile("../cliagent/testdata/claude/read.jsonl")
	if out.String() != string(all[10:]) {
		t.Fatalf("output after the restart: %d bytes, want %d", len(out.String()), len(all)-10)
	}
}
