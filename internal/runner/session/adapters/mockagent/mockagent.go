// Package mockagent is the MockAgentAdapter's child process (KIN-23): a deterministic stand-in for an AI CLI that
// runs as a real process (`ai-runner mock-agent --scenario <s> --attempt <n> --state <dir>`) in the attempt's
// working directory. It prints a few lines, may fail, crash, hang or wait for a release file in the state
// directory, and leaves its exit code in result.json there, so a runner restarted meanwhile can still report how
// it ended.
package mockagent

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// ResultFile is where the process leaves its exit code (in its working directory).
const ResultFile = "result.json"

// ReleaseFile releases a process of the gate scenario.
const ReleaseFile = "release"

// Result is the content of ResultFile.
type Result struct {
	ExitCode int `json:"exitCode"`
}

// Main runs the mock agent and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	// Output goes to the runner through pipes; when the runner restarts they break. The agent keeps running (as a
	// real CLI would) instead of dying of SIGPIPE, so the new runner process can adopt it.
	signal.Ignore(syscall.SIGPIPE)
	fs := flag.NewFlagSet("mock-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenario := fs.String("scenario", "success", "success|fail|flaky|crash|hang|gate|scope")
	attempt := fs.Int64("attempt", 1, "attempt fencing token")
	poll := fs.Duration("poll", 20*time.Millisecond, "gate: how often to look for the release file")
	state := fs.String("state", ".", "directory of the result and release files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintf(stdout, "mock agent: scenario %s, attempt %d\n", *scenario, *attempt)
	switch *scenario {
	case "success":
		fmt.Fprintln(stdout, "mock agent: reading the task")
		fmt.Fprintln(stdout, "mock agent: done")
		return finish(*state, 0)
	case "fail":
		fmt.Fprintln(stderr, "mock agent: simulated failure")
		return finish(*state, 1)
	case "flaky":
		if *attempt <= 1 {
			fmt.Fprintln(stderr, "mock agent: simulated failure on the first attempt")
			return finish(*state, 1)
		}
		fmt.Fprintln(stdout, "mock agent: succeeded on retry")
		return finish(*state, 0)
	case "crash":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // unreachable: SIGKILL cannot be caught
	case "hang":
		fmt.Fprintln(stdout, "mock agent: hanging until cancelled")
		select {}
	case "gate":
		return gate(stdout, *state, *poll)
	case "scope": // KIN-51: changes files in the working directory (the attempt's worktree)
		files := []string{filepath.Join("mock", "scope.txt")}
		if *attempt <= 1 {
			files = append(files, "outside.txt") // beyond the mock plan's scope
		}
		for _, f := range files {
			if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
				fmt.Fprintln(stderr, "mock agent:", err)
				return finish(*state, 1)
			}
			if err := os.WriteFile(f, fmt.Appendf(nil, "attempt %d\n", *attempt), 0o600); err != nil {
				fmt.Fprintln(stderr, "mock agent:", err)
				return finish(*state, 1)
			}
			fmt.Fprintln(stdout, "mock agent: wrote", f)
		}
		if *attempt <= 1 {
			return gate(stdout, *state, *poll)
		}
		return finish(*state, 0)
	}
	fmt.Fprintf(stderr, "mock agent: unknown scenario %q\n", *scenario)
	return finish(*state, 2)
}

// finish records the exit code atomically (temp file + rename) and returns it.
func finish(state string, code int) int {
	b, _ := json.Marshal(Result{ExitCode: code})
	tmp := filepath.Join(state, ResultFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, filepath.Join(state, ResultFile))
	}
	return code
}

// ReadResult reads the exit code a mock agent left in dir; ok=false when it left none.
func ReadResult(dir string) (code int, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, ResultFile))
	if errors.Is(err, os.ErrNotExist) || err != nil {
		return 0, false
	}
	var r Result
	if json.Unmarshal(b, &r) != nil {
		return 0, false
	}
	return r.ExitCode, true
}

// gate waits for the release file in the state directory, then exits 0.
func gate(stdout io.Writer, state string, poll time.Duration) int {
	fmt.Fprintln(stdout, "mock agent: waiting for release")
	for {
		if _, err := os.Stat(filepath.Join(state, ReleaseFile)); err == nil {
			fmt.Fprintln(stdout, "mock agent: released")
			return finish(state, 0)
		}
		time.Sleep(poll)
	}
}
