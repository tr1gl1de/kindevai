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

// Launcher starts the mock agent as `Program Args... --scenario <s> --attempt <n>`. Env is the child's whole
// environment: the runner's own variables (and any secret in them) are never inherited.
type Launcher struct {
	Program string
	Args    []string
	Env     []string
	// Poll is how often an adopted process is checked (default 200ms).
	Poll time.Duration
}

var _ app.Launcher = (*Launcher)(nil)

// Launch starts the attempt's process as the leader of a new process group.
func (l *Launcher) Launch(_ context.Context, s app.LaunchSpec) (app.Process, error) {
	// Not CommandContext: the process must outlive this runner process (it is adopted after a restart) and is
	// only stopped by an explicit cancel.
	args := append(append([]string{}, l.Args...), "--scenario", s.Scenario, "--attempt", strconv.FormatInt(s.Attempt, 10))
	//nolint:noctx,gosec // must outlive ctx (adopted after a restart); fixed program, validated scenario
	cmd := exec.Command(l.Program, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = s.Dir, s.Stdout, s.Stderr
	cmd.Env = l.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", l.Program, err)
	}
	p := &attached{cmd: cmd, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

type attached struct {
	cmd  *exec.Cmd
	done chan struct{}
	exit domain.Exit
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
func (p *attached) Wait() domain.Exit { <-p.done; return p.exit }
func (p *attached) Signal(kill bool) error {
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

// Adopt watches a recorded process again. It is the same process only while the pid still leads the recorded
// process group (a reused pid would not), and a zombie counts as ended.
func (l *Launcher) Adopt(r domain.Record, dir string) app.Process {
	poll := l.Poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	return &adopted{pid: r.PID, pgid: r.PGID, dir: dir, poll: poll}
}

type adopted struct {
	pid, pgid int
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

func (p *adopted) PID() int  { return p.pid }
func (p *adopted) PGID() int { return p.pgid }
func (p *adopted) Wait() domain.Exit {
	p.once.Do(func() {
		for Alive(p.pid, p.pgid) {
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
	if !Alive(p.pid, p.pgid) {
		return nil
	}
	return signalGroup(p.pgid, kill)
}
