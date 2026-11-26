//go:build unix

// Package process launches attempt processes (KIN-23): each runs in its own process group, so a cancel stops the
// whole tree, and survives a runner restart, after which Adopt watches it again by its recorded pid/pgid.
package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/mockagent"
	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Launcher starts the mock agent as `Program Args... --scenario <s> --attempt <n> --state <dir>` in the attempt's
// worktree (or its state dir). Env is the child's whole
// environment: the runner's own variables (and any secret in them) are never inherited.
//
// An attempt with an AI CLI (LaunchSpec.Agent, KIN-79) runs as `Program AgentArgs... --state <dir> --prompt <file>
// --timeout <d> [--tests <file> [--tests-timeout <d>]] -- <cli> <args>...` (`ai-runner agent-exec`) in the worktree, with the CLI's environment after Env.
type Launcher struct {
	Program string
	Args    []string
	Env     []string
	// AgentArgs start the CLI wrapper (default "agent-exec").
	AgentArgs []string
	// Poll is how often an adopted process is checked (default 200ms).
	Poll time.Duration
}

var _ app.Launcher = (*Launcher)(nil)

// Launch starts the attempt's process as the leader of a new process group. With a sandbox (strict egress,
// KIN-31) the process is the sandbox's helper, and Launch returns only once the sandbox verified; otherwise the
// helper is stopped and the error wraps app.ErrNetwork (the agent never ran).
func (l *Launcher) Launch(ctx context.Context, s app.LaunchSpec) (app.Process, error) {
	// Not CommandContext: the process must outlive this runner process (it is adopted after a restart) and is
	// only stopped by an explicit cancel.
	args := append(append([]string{}, l.Args...), "--scenario", s.Scenario, "--attempt", strconv.FormatInt(s.Attempt, 10),
		"--state", s.Dir)
	dir := s.Dir
	if s.Workdir != "" {
		dir = s.Workdir
	}
	env := append([]string{}, l.Env...)
	if ag := s.Agent; ag != nil {
		wrap := l.AgentArgs
		if wrap == nil {
			wrap = []string{"agent-exec"}
		}
		args = append(append([]string{}, wrap...), "--state", s.Dir, "--prompt", ag.PromptFile, "--timeout", ag.Timeout.String())
		if ag.TestsFile != "" { // the review loop's test run after the CLI (KIN-35)
			args = append(args, "--tests", ag.TestsFile)
			if ag.TestsTimeout > 0 {
				args = append(args, "--tests-timeout", ag.TestsTimeout.String())
			}
		}
		args = append(append(args, "--"), append([]string{ag.Path}, ag.Args...)...)
		env = append(env, ag.Env...)
		dir = ag.Dir
	}
	var cmd *exec.Cmd
	if s.Sandbox != nil {
		c, err := s.Sandbox.Command(append([]string{l.Program}, args...), env, dir)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", app.ErrNetwork, err)
		}
		cmd = c
	} else {
		//nolint:noctx,gosec // must outlive ctx (adopted after a restart); fixed program, validated scenario
		cmd = exec.Command(l.Program, args...)
		cmd.Dir, cmd.Env = dir, env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	if err := cmd.Start(); err != nil {
		if s.Sandbox != nil {
			return nil, fmt.Errorf("%w: starting the sandbox: %w", app.ErrNetwork, err)
		}
		return nil, fmt.Errorf("starting %s: %w", l.Program, err)
	}
	p := &attached{cmd: cmd, sandbox: s.Sandbox, done: make(chan struct{})}
	// Read before anyone waits for the child: until it is reaped its pid cannot be reused. On failure the record has
	// no start time and adoption falls back to the process group check (the executor logs a warning).
	p.start, _ = StartTime(cmd.Process.Pid)
	go p.wait()
	if s.Sandbox != nil {
		if err := s.Sandbox.Await(ctx); err != nil {
			_ = p.Signal(true)
			<-p.done
			return nil, fmt.Errorf("%w: %w", app.ErrNetwork, err)
		}
	}
	return p, nil
}

type attached struct {
	cmd     *exec.Cmd
	sandbox app.Sandbox // nil without strict egress
	start   string
	done    chan struct{}
	exit    domain.Exit
}

func (p *attached) wait() {
	err := p.cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			p.exit.Signaled = true
		} else {
			p.exit.Code = ee.ExitCode()
		}
	default:
		p.exit.Code = -1
	}
	close(p.done)
}

func (p *attached) PID() int          { return p.cmd.Process.Pid }
func (p *attached) PGID() int         { return p.cmd.Process.Pid }
func (p *attached) StartTime() string { return p.start }
func (p *attached) Wait() domain.Exit { <-p.done; return p.exit }

// Signal stops the process group: SIGTERM (a sandbox is marked cancelled first, KIN-103), or SIGKILL.
func (p *attached) Signal(kill bool) error {
	if !kill && p.sandbox != nil {
		p.sandbox.Cancel()
	}
	return signalGroup(p.cmd.Process.Pid, kill)
}

func signalGroup(pgid int, kill bool) error {
	if pgid <= 1 {
		return errors.New("no process group")
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Adopt watches a recorded process again. It is the same process only while the pid has the recorded start time
// (KIN-64: a reused pid has another) and still leads the recorded process group, and a zombie counts as ended. A
// record without a start time (written before KIN-64) relies on the process group check alone.
func (l *Launcher) Adopt(r domain.Record, dir string) app.Process {
	poll := l.Poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	return &adopted{pid: r.PID, pgid: r.PGID, start: r.Start, dir: dir, poll: poll}
}

type adopted struct {
	pid, pgid int
	start     string
	dir       string
	poll      time.Duration
	once      sync.Once
	exit      domain.Exit
}

// Alive reports whether pid still runs and leads pgid.
func Alive(pid, pgid int) bool {
	if pid <= 1 || syscall.Kill(pid, 0) != nil {
		return false
	}
	if g, err := syscall.Getpgid(pid); err != nil || g != pgid {
		return false
	}
	// Linux: a zombie (exited, not yet reaped by its parent) still answers signal 0.
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i > 0 && i+2 < len(b) && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// groupAlive reports a process group with at least one member.
func groupAlive(pgid int) bool { return pgid > 1 && syscall.Kill(-pgid, 0) == nil }

// reused reports that the recorded pid now belongs to another process (its start time differs). Linux and XNU do
// not hand out a pid while a process group of that id exists, so then the recorded group is gone too and any group
// with that id is a stranger's. When the pid does not exist (StartTime fails) the group check decides: by the same
// guarantee a live group with that id can only be the recorded one.
func (p *adopted) reused() bool {
	if p.start == "" {
		return false
	}
	st, err := StartTime(p.pid)
	return err == nil && st != p.start
}

// running reports that the attempt still runs: its leader or any process of its group (children of an exited CLI).
func (p *adopted) running() bool {
	return !p.reused() && (Alive(p.pid, p.pgid) || groupAlive(p.pgid))
}

// Running reports whether the adopted attempt still runs (KIN-81: only then does it get its sandbox back).
func (p *adopted) Running() bool { return p.running() }

func (p *adopted) PID() int          { return p.pid }
func (p *adopted) PGID() int         { return p.pgid }
func (p *adopted) StartTime() string { return p.start }
func (p *adopted) Wait() domain.Exit {
	p.once.Do(func() {
		for p.running() {
			time.Sleep(p.poll)
		}
		if code, ok := mockagent.ReadResult(p.dir); ok {
			p.exit = domain.Exit{Code: code}
		} else {
			p.exit = domain.Exit{Lost: true}
		}
	})
	return p.exit
}
func (p *adopted) Signal(kill bool) error {
	if !p.running() {
		return nil // never signal a stranger's group
	}
	return signalGroup(p.pgid, kill)
}
