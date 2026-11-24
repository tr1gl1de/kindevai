// Package mockagent is the MockAgentAdapter's child process (KIN-23): a deterministic stand-in for an AI CLI that
// runs as a real process (`ai-runner mock-agent --scenario <s> --attempt <n>`) in the attempt's working
// directory. It prints a few lines, may fail, crash, hang or wait for a release file, and leaves its exit code in
// result.json there, so a runner restarted meanwhile can still report how it ended.
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
	scenario := fs.String("scenario", "success", "success|fail|flaky|crash|hang|gate")
	attempt := fs.Int64("attempt", 1, "attempt fencing token")
	poll := fs.Duration("poll", 20*time.Millisecond, "gate: how often to look for the release file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintf(stdout, "mock agent: scenario %s, attempt %d\n", *scenario, *attempt)
	switch *scenario {
	case "success":
		fmt.Fprintln(stdout, "mock agent: reading the task")
		fmt.Fprintln(stdout, "mock agent: done")
		return finish(0)
	case "fail":
		fmt.Fprintln(stderr, "mock agent: simulated failure")
		return finish(1)
	case "flaky":
		if *attempt <= 1 {
			fmt.Fprintln(stderr, "mock agent: simulated failure on the first attempt")
			return finish(1)
		}
		fmt.Fprintln(stdout, "mock agent: succeeded on retry")
		return finish(0)
	case "crash":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // unreachable: SIGKILL cannot be caught
	case "hang":
		fmt.Fprintln(stdout, "mock agent: hanging until cancelled")
		select {}
	case "gate":
		fmt.Fprintln(stdout, "mock agent: waiting for release")
		for {
			if _, err := os.Stat(ReleaseFile); err == nil {
				fmt.Fprintln(stdout, "mock agent: released")
				return finish(0)
			}
			time.Sleep(*poll)
		}
	}
	fmt.Fprintf(stderr, "mock agent: unknown scenario %q\n", *scenario)
	return finish(2)
}

// finish records the exit code atomically (temp file + rename) and returns it.
func finish(code int) int {
	b, _ := json.Marshal(Result{ExitCode: code})
	tmp := ResultFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, ResultFile)
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
