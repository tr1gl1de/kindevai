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
	"runtime"
	"syscall"
)

// RunMain is `ai-runner sandbox-run`, the outer launcher of a wrapped run (see Wrap). It exits with the agent's
// status (128+signal when the agent was killed), or ExitUnverified when the sandbox did not verify (the agent
// never started), or ExitUsage when its description is missing.
func RunMain(stderr io.Writer) int {
	raw := os.Getenv(RunEnv)
	_ = os.Unsetenv(RunEnv)
	var rs RunSpec
	if raw == "" || json.Unmarshal([]byte(raw), &rs) != nil || len(rs.Argv) == 0 {
		fmt.Fprintln(stderr, "strict egress sandbox: no run description")
		return ExitUsage
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox: locating the runner binary:", err)
		return ExitUnverified
	}
	m := &Manager{Program: exe, Args: rs.HelperArgs, Env: rs.HelperEnv, DataDir: rs.DataDir, Sockets: rs.Sockets}
	b, err := m.open(rs.Request)
	if err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox:", err)
		return ExitUnverified
	}
	defer b.Release()
	cmd, err := b.Command(rs.Argv, rs.Env, rs.Dir)
	if err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox:", err)
		return ExitUnverified
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The sandbox stays in this process group (the launcher's cancel reaches the agent) and dies with this
	// process, which serves its only way out.
	cmd.SysProcAttr.Setpgid = false
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	runtime.LockOSThread() // Pdeathsig follows the thread that started the child
	// Signals to the group reach the agent itself; this process waits to report its status.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for range sigs { //nolint:revive // drained
		}
	}()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox: starting the helper:", err)
		return ExitUnverified
	}
	if err := b.Await(context.Background()); err != nil {
		fmt.Fprintln(stderr, "strict egress sandbox:", err)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ExitUnverified
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
		return ee.ExitCode()
	}
	return ExitAgentStart
}
