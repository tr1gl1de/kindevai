//go:build linux

package nsbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
)

// RunMain is `ai-runner sandbox-run`, the outer launcher of a wrapped run (see Wrap). It exits with the agent's
// status (128+signal when the agent was killed), or ExitUnverified when the sandbox did not verify (the agent
// never started), or ExitUsage when its description is missing.
func RunMain(stderr io.Writer) int {
	// Stays dumpable: a non-dumpable parent cannot write its child's uid_map, which the sandbox needs.
	path := os.Getenv(RunEnv)
	_ = os.Unsetenv(RunEnv)
	var rs RunSpec
	if path == "" || !filepath.IsAbs(path) {
		fmt.Fprintln(stderr, "strict egress sandbox: no run description")
		return ExitUsage
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the description the runner wrote for this run
	_ = os.Remove(path)           //nolint:gosec // G703: as above
	if err != nil || json.Unmarshal(raw, &rs) != nil || len(rs.Argv) == 0 {
		fmt.Fprintln(stderr, "strict egress sandbox: no run description")
		return ExitUsage
	}
	if !profileStrict(rs.Request.Profile) || rs.DataDir == "" {
		return unverified(stderr, rs.DataDir, path, errors.New("the run description is not a strict sandbox"))
	}
	exe, err := os.Executable()
	if err != nil {
		return unverified(stderr, rs.DataDir, path, err)
	}
	m := &Manager{Program: exe, Args: rs.HelperArgs, Env: rs.HelperEnv, DataDir: rs.DataDir, Home: rs.Home, Sockets: rs.Sockets}
	b, err := m.open(rs.Request)
	if err != nil {
		return unverified(stderr, rs.DataDir, path, err)
	}
	defer b.Release()
	cmd, err := b.Command(rs.Argv, rs.Env, rs.Dir)
	if err != nil {
		return unverified(stderr, rs.DataDir, path, err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The sandbox stays in this process group (the launcher's cancel reaches the agent) and dies with this
	// process, which serves its only way out.
	cmd.SysProcAttr.Setpgid = false
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	// Pdeathsig follows the thread that started the child: RunMain runs on main's goroutine, which keeps this
	// thread until the process exits.
	runtime.LockOSThread()
	// Signals to the group reach the agent itself; this process waits to report its status. A signal that came
	// before the helper could catch it (the helper is its PID namespace's init: an uncaught signal is dropped) is
	// remembered: the sandbox is then not started, or the signal is passed on once the helper verified.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	var mu sync.Mutex
	var first os.Signal
	go func() {
		for sig := range sigs {
			mu.Lock()
			if first == nil {
				first = sig
			}
			mu.Unlock()
		}
	}()
	received := func() os.Signal {
		mu.Lock()
		defer mu.Unlock()
		return first
	}
	if sig := received(); sig != nil {
		return 128 + int(sig.(syscall.Signal))
	}
	if err := cmd.Start(); err != nil {
		return unverified(stderr, rs.DataDir, path, err)
	}
	if err := b.Await(context.Background()); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return unverified(stderr, rs.DataDir, path, err)
	}
	if sig := received(); sig != nil { // the helper catches it now and passes it to the agent
		_ = cmd.Process.Signal(sig)
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return relayed(ee.ExitCode())
	}
	return ExitAgentStart
}

// unverified reports a sandbox that did not verify: the status file in the data directory (the launcher's signal,
// KIN-88), UnverifiedMarker and the reason on stderr (for people), ExitUnverified. The agent has not started.
func unverified(stderr io.Writer, dataDir, desc string, err error) int {
	recordUnverified(dataDir, desc)
	fmt.Fprintf(stderr, "%s: %v\n", UnverifiedMarker, err)
	return ExitUnverified
}

// relayed is the agent's exit code, moved out of the codes the sandbox reserves for itself.
func relayed(code int) int {
	if code >= ExitUsage && code <= ExitAgentStart {
		return 1
	}
	return code
}
