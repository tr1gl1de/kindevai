//go:build unix

package cliagent

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Files of an attempt's CLI run in its state directory (KIN-79). The CLI writes its output to files, not to the
// runner's pipes, so a restarted runner reads on where the last one stopped.
const (
	StdoutFile = "agent.stdout"
	StderrFile = "agent.stderr"
	EndFile    = "agent.end.json"
	PromptFile = "prompt.md"
	// The test run after the CLI (KIN-35): the command (written by the runner), its output and how it ended.
	TestsCommandFile = "tests.cmd"
	TestsOutputFile  = "tests.out"
	TestsEndFile     = "tests.json"
)

// TestsEnd is how the test command ended, as `ai-runner agent-exec` recorded it (Code -1: killed or not started).
type TestsEnd struct {
	Code       int   `json:"code"`
	TimedOut   bool  `json:"timedOut,omitempty"`
	DurationMS int64 `json:"durationMs"`
}

// DefaultTestsTimeout bounds a test run when the runner names none; MaxTestsOutput bounds its output file.
const (
	DefaultTestsTimeout = 30 * time.Minute
	MaxTestsOutput      = 8 << 20
)

// testEnv are the variables a test run inherits from the CLI's environment: what builds need, never the CLI's
// credentials or configuration (a repository's tests are untrusted code).
var testEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TERM",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "GOPATH", "GOCACHE", "GOMODCACHE",
	"GOFLAGS", "GOPROXY", "GOTOOLCHAIN", "CGO_ENABLED", "NODE_OPTIONS", "npm_config_cache", "PIP_CACHE_DIR"}

// ExecEnd is how the CLI ended, as `ai-runner agent-exec` recorded it.
type ExecEnd struct {
	Code     int  `json:"code"`
	Signaled bool `json:"signaled,omitempty"`
	TimedOut bool `json:"timedOut,omitempty"`
}

// ExecMain is `ai-runner agent-exec --state <dir> --prompt <file> --timeout <d> [--grace <d>] -- <cli> <args>...`:
// the attempt's process (the leader of its process group, or the strict egress sandbox's agent). It runs the CLI in
// its own process group with the prompt file on stdin and its stdout and stderr appended to the state directory's
// files, stops it when the timeout passes (SIGTERM to the group, SIGKILL after the grace period), records how it
// ended in EndFile and then ends the whole group, so nothing the CLI left behind keeps running. Its environment is
// the CLI's (the launcher passes the CLI's clean environment).
func ExecMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "", "the attempt's state directory")
	prompt := fs.String("prompt", "", "the prompt file (the CLI's stdin)")
	timeout := fs.Duration("timeout", 0, "the run's timeout")
	grace := fs.Duration("grace", DefaultGrace, "time between SIGTERM and SIGKILL")
	tests := fs.String("tests", "", "a file with the test command to run in the working directory after the CLI succeeded")
	testsTimeout := fs.Duration("tests-timeout", DefaultTestsTimeout, "the test run's timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if *state == "" || *prompt == "" || *timeout <= 0 || len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		fmt.Fprintln(stderr, "agent-exec: --state, --prompt, --timeout and an absolute CLI path are required")
		return 2
	}
	// The CLI runs in this process's group (the attempt's): a cancel's SIGTERM reaches it, and this process ignores
	// it (until it recorded the end) to record the CLI's end. SIGKILL after the grace period ends them both (the end
	// is then unknown). A handler, not SIG_IGN: the CLI must not inherit an ignored SIGTERM.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	end, err := execCLI(*state, *prompt, *timeout, *grace, argv)
	if err != nil {
		fmt.Fprintln(stderr, "agent-exec:", err)
		end = ExecEnd{Code: 127}
	}
	if *tests != "" && end.Code == 0 && !end.Signaled && !end.TimedOut {
		// Before the CLI's end is recorded: a follower that sees the end finds the test result too.
		runTests(*state, *tests, *testsTimeout, *grace, stderr)
	}
	if err := writeEnd(*state, end); err != nil {
		fmt.Fprintln(stderr, "agent-exec: recording the end:", err)
	}
	killGroup()
	if end.Code > 255 || end.Code < 0 {
		return 1
	}
	return end.Code
}

func execCLI(state, prompt string, timeout, grace time.Duration, argv []string) (ExecEnd, error) {
	in, err := openPrompt(prompt)
	if err != nil {
		return ExecEnd{}, err
	}
	defer in.Close()
	out, err := openOutput(filepath.Join(state, StdoutFile))
	if err != nil {
		return ExecEnd{}, err
	}
	defer out.Close()
	errf, err := openOutput(filepath.Join(state, StderrFile))
	if err != nil {
		return ExecEnd{}, err
	}
	defer errf.Close()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // the adapter's binary and validated arguments
	cmd.Env, cmd.Stdin = os.Environ(), in
	cmd.Stdout = &capped{f: out, left: MaxOutputFile, name: "stdout"}
	cmd.Stderr = &capped{f: errf, left: MaxOutputFile, name: "stderr"}
	cmd.WaitDelay = grace // a process the CLI left holding its output does not keep the run open
	if err := cmd.Start(); err != nil {
		return ExecEnd{}, fmt.Errorf("starting the CLI: %w", err)
	}
	// leader: this process leads the attempt's group (not so inside the strict egress sandbox, whose helper does).
	leader := syscall.Getpgrp() == os.Getpid() && os.Getpid() > 1
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		if leader {
			_ = syscall.Kill(-os.Getpid(), syscall.SIGTERM) // the CLI and its children; ignored here
		} else {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		time.AfterFunc(grace, func() { _ = cmd.Process.Kill() })
	})
	err = cmd.Wait()
	timer.Stop()
	end := ExecEnd{TimedOut: timedOut.Load()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			end.Signaled, end.Code = true, -1
		} else {
			end.Code = ee.ExitCode()
		}
	default:
		return ExecEnd{}, err
	}
	return end, nil
}

// killGroup ends whatever the CLI left in the attempt's group, this process included, once its end is recorded
// (only when this process leads the group: never another group).
func killGroup() {
	if syscall.Getpgrp() == os.Getpid() && os.Getpid() > 1 {
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	}
}

// MaxOutputFile bounds each of the CLI's output files: beyond it the output is discarded (the CLI is not blocked)
// and a marker line notes the cut, so a looping CLI cannot fill the runner's disk.
const MaxOutputFile = 256 << 20

type capped struct {
	f    *os.File
	left int64
	name string
	cut  bool
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if c.left > 0 {
		w := p[:min(int64(len(p)), c.left)]
		if _, err := c.f.Write(w); err != nil {
			return 0, err
		}
		c.left -= int64(len(w))
	}
	if c.left <= 0 && !c.cut && n > 0 {
		c.cut = true
		_, _ = fmt.Fprintf(c.f, "\n[agent-exec: %s cut at %d bytes]\n", c.name, int64(MaxOutputFile))
	}
	return n, nil
}

func openOutput(name string) (*os.File, error) {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the attempt's own file
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", filepath.Base(name), err)
	}
	return f, nil
}

func writeEnd(state string, e ExecEnd) error { return writeJSON(state, EndFile, e) }

// ReadEnd reads how the CLI of the attempt in state ended; ok=false when agent-exec recorded nothing (it was
// killed, or never ran).
func ReadEnd(state string) (ExecEnd, bool) {
	b, err := os.ReadFile(filepath.Join(state, EndFile)) //nolint:gosec // the attempt's own file
	if err != nil {
		return ExecEnd{}, false
	}
	var e ExecEnd
	if json.Unmarshal(b, &e) != nil {
		return ExecEnd{}, false
	}
	return e, true
}

// runTests runs the test command of file with /bin/sh in the working directory, in the attempt's process group (a
// cancel reaches it; ExecMain ends what it leaves behind), stopped when the timeout passes, with an allowlisted
// environment and its output in TestsOutputFile, and records how it ended in TestsEndFile.
func runTests(state, file string, timeout, grace time.Duration, stderr io.Writer) {
	end := TestsEnd{Code: -1}
	defer func() {
		if err := writeJSON(state, TestsEndFile, end); err != nil {
			fmt.Fprintln(stderr, "agent-exec: recording the test result:", err)
		}
	}()
	cmdText, err := os.ReadFile(file) //nolint:gosec // the runner's own file in the attempt's state directory
	if err != nil {
		fmt.Fprintln(stderr, "agent-exec: reading the test command:", err)
		return
	}
	out, err := openOutput(filepath.Join(state, TestsOutputFile))
	if err != nil {
		fmt.Fprintln(stderr, "agent-exec:", err)
		return
	}
	defer out.Close()
	cmd := exec.Command("/bin/sh", "-c", string(cmdText)) //nolint:gosec,noctx // the repository's configured test command
	cmd.Env = append(filterEnv(os.Environ(), testEnv), "CI=true")
	w := &capped{f: out, left: MaxTestsOutput, name: "test output"}
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = grace
	start := time.Now()
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(out, "starting the test command: %v\n", err)
		return
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		time.AfterFunc(grace, func() { _ = cmd.Process.Kill() })
	})
	err = cmd.Wait()
	timer.Stop()
	end.DurationMS, end.TimedOut = time.Since(start).Milliseconds(), timedOut.Load()
	var ee *exec.ExitError
	switch {
	case err == nil:
		end.Code = 0
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		end.Code = ee.ExitCode()
	}
}

func filterEnv(env, keep []string) []string {
	var out []string
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && slices.Contains(keep, k) {
			out = append(out, kv)
		}
	}
	return out
}

func writeJSON(state, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(state, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(state, name))
}

// ReadTests reads the test result of the attempt in state: ok=false when no test run was recorded. The output is
// the tail of TestsOutputFile, at most n bytes.
func ReadTests(state string, n int) (end TestsEnd, output string, ok bool) {
	b, err := os.ReadFile(filepath.Join(state, TestsEndFile)) //nolint:gosec // the attempt's own file
	if err != nil || json.Unmarshal(b, &end) != nil {
		return TestsEnd{}, "", false
	}
	f, err := os.Open(filepath.Join(state, TestsOutputFile)) //nolint:gosec // the attempt's own file
	if err != nil {
		return end, "", true
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > int64(n) {
		_, _ = f.Seek(st.Size()-int64(n), io.SeekStart)
	}
	tail, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	return end, string(tail), true
}
