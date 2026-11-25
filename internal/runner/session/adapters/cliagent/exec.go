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
)

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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if *state == "" || *prompt == "" || *timeout <= 0 || len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		fmt.Fprintln(stderr, "agent-exec: --state, --prompt, --timeout and an absolute CLI path are required")
		return 2
	}
	end, err := execCLI(*state, *prompt, *timeout, *grace, argv)
	if err != nil {
		fmt.Fprintln(stderr, "agent-exec:", err)
		end = ExecEnd{Code: 127}
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
	// The CLI runs in this process's group (the attempt's): a cancel's SIGTERM reaches it, and this process ignores
	// it to record the CLI's end. SIGKILL after the grace period ends them both (the end is then unknown).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // the adapter's binary and validated arguments
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Environ(), in, out, errf
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

func openOutput(name string) (*os.File, error) {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the attempt's own file
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", filepath.Base(name), err)
	}
	return f, nil
}

func writeEnd(state string, e ExecEnd) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp := filepath.Join(state, EndFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(state, EndFile))
}

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
