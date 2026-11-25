//go:build unix

package cliagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Command is a process to start: what the adapter builds and a Sandbox may rewrite.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// Sandbox confines a CLI run (KIN-31's strict-egress sandbox implements it).
type Sandbox interface {
	// StrictEgress reports that the run will be confined by the verified strict-egress sandbox: its preflight
	// passed (proxy reachable, egress IP matches, leak tests blocked) and the runner reports strict_egress. Only
	// then is the permission bypass allowed.
	StrictEgress(ctx context.Context) bool
	// Wrap returns the command that runs c inside the sandbox (e.g. in its network namespace, with its proxy
	// environment). It is called for every run, in safe mode too.
	Wrap(ctx context.Context, c Command) (Command, error)
}

// Defaults of Config.
const (
	DefaultPath    = "/usr/local/bin:/usr/bin:/bin"
	DefaultGrace   = 5 * time.Second
	DefaultMaxLine = 4 << 20
	// stderrTail is how much of the end of stderr is kept to explain a failure.
	stderrTail = 4 << 10
	// drainWait bounds how long output is read after the process group was killed (a process that left the group
	// may still hold the pipes).
	drainWait = 2 * time.Second
)

// Config is the runner's configuration of one CLI. It comes from the runner's operator, never from a task.
type Config struct {
	// Binary is the CLI's absolute path (found on the runner's PATH).
	Binary string
	// Home is the CLI's HOME: the runner user's home, where the CLI keeps its local login (~/.claude, ~/.codex).
	// It must not lie inside a workspace, so the agent never sees the login.
	Home string
	// Path is the CLI's PATH; DefaultPath plus the binary's directory when empty.
	Path string
	// Env is extra KEY=VALUE variables set by the operator (e.g. a proxy on the host, CODEX_HOME). Nothing else of
	// the runner's environment (where Control Plane secrets may live) is inherited.
	Env []string
	// Sandbox confines the run; nil runs on the host (safe mode only).
	Sandbox Sandbox
	// Grace is the time between SIGTERM and SIGKILL on a cancel or timeout; DefaultGrace when zero.
	Grace time.Duration
	// MaxLine bounds one output line kept for parsing (longer lines are passed through but not parsed);
	// DefaultMaxLine when zero.
	MaxLine int
}

// Adapter runs one CLI.
type Adapter struct {
	Dialect Dialect
	Config  Config
}

// Hooks receive a run's output. They are called from the run's reader goroutines; a slow hook slows the reading,
// and so the CLI (backpressure), never buffers without bound.
type Hooks struct {
	// OnEvent receives each parsed event in order.
	OnEvent func(domain.AgentEvent)
	// Stdout and Stderr receive the raw output (e.g. the attempt's output stream); nil discards it.
	Stdout, Stderr io.Writer
}

// Run is a started CLI process.
type Run struct {
	pid   int
	grace time.Duration
	done  chan struct{}

	mu        sync.Mutex
	exited    bool // the CLI ended: a later cancel or timeout changes nothing
	stopping  bool
	cancelled string
	timedOut  bool
	result    domain.AgentResult
}

// Start validates the request, decides the permission mode and starts the CLI. It refuses the permission bypass
// unless the configured sandbox is verified strict-egress (domain.ErrBypassWithoutSandbox): the CLI is not started.
// ctx bounds the run: when it ends the run is cancelled.
func (a *Adapter) Start(ctx context.Context, req domain.AgentRequest, h Hooks) (*Run, error) {
	if err := a.check(req); err != nil {
		return nil, err
	}
	c := a.Config
	strict := c.Sandbox != nil && c.Sandbox.StrictEgress(ctx)
	perms, err := domain.ChoosePermissions(req.BypassPermissions, strict)
	if err != nil {
		return nil, err
	}
	prompt, err := openPrompt(req.PromptFile)
	if err != nil {
		return nil, err
	}
	defer prompt.Close()
	cmd := a.command(req, perms)
	if c.Sandbox != nil {
		if cmd, err = c.Sandbox.Wrap(ctx, cmd); err != nil {
			return nil, fmt.Errorf("wrapping the CLI in the sandbox: %w", err)
		}
	}
	return a.start(ctx, req, cmd, prompt, h)
}

// Command validates the request and returns the CLI's command in permission mode p (the caller decided p with
// domain.ChoosePermissions and runs the command with the prompt file on stdin).
func (a *Adapter) Command(req domain.AgentRequest, p domain.Permissions) (Command, error) {
	if err := a.check(req); err != nil {
		return Command{}, err
	}
	return a.command(req, p), nil
}

func (a *Adapter) command(req domain.AgentRequest, p domain.Permissions) Command {
	return Command{Path: a.Config.Binary, Args: a.Dialect.Args(req, p), Env: a.env(), Dir: req.Workdir}
}

// check validates the request against the adapter's configuration.
func (a *Adapter) check(req domain.AgentRequest) error {
	if err := req.Check(); err != nil {
		return err
	}
	if req.Agent != a.Dialect.Name() {
		return fmt.Errorf("%w: agent %q on the %s adapter", domain.ErrInvalidAgentRequest, req.Agent, a.Dialect.Name())
	}
	c := a.Config
	if !filepath.IsAbs(c.Binary) || !filepath.IsAbs(c.Home) {
		return errors.New("the CLI's binary and HOME must be absolute paths")
	}
	// Compare resolved paths: a symlink must not place HOME (the CLI's login) or the prompt inside the workspace.
	// (While agent and runner share a uid the agent can still reach HOME at run time; isolating it is the
	// sandbox's job, KIN-31.)
	wd, home, pf, err := resolve(req.Workdir, c.Home, req.PromptFile)
	if err != nil {
		return err
	}
	if domain.Within(home, wd) {
		return errors.New("the CLI's HOME lies inside the workspace: its login would be exposed to the agent")
	}
	if domain.Within(pf, wd) {
		return fmt.Errorf("%w: prompt file", domain.ErrInvalidAgentRequest)
	}
	return nil
}

func resolve(paths ...string) (string, string, string, error) {
	out := make([]string, len(paths))
	for i, p := range paths {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", "", "", fmt.Errorf("resolving %s: %w", filepath.Base(p), err)
		}
		out[i] = r
	}
	return out[0], out[1], out[2], nil
}

func openPrompt(name string) (*os.File, error) {
	f, err := os.Open(name) //nolint:gosec // G304: the attempt's own prompt file, checked to lie outside the workspace
	if err != nil {
		return nil, fmt.Errorf("opening the prompt: %w", err)
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("the prompt is not a regular file")
	}
	return f, nil
}

// env is the CLI's whole environment: operator variables, the dialect's, then the fixed base (which wins).
func (a *Adapter) env() []string {
	path := a.Config.Path
	if path == "" {
		path = DefaultPath + ":" + filepath.Dir(a.Config.Binary)
	}
	env := append([]string{}, a.Config.Env...)
	env = append(env, a.Dialect.Env()...)
	return append(env, "HOME="+a.Config.Home, "PATH="+path, "LANG=C.UTF-8", "TERM=dumb", "NO_COLOR=1")
}

func (a *Adapter) start(ctx context.Context, req domain.AgentRequest, c Command, stdin *os.File, h Hooks) (*Run, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	// Not CommandContext: a cancel stops the whole process group, gracefully (Run.stop).
	cmd := exec.Command(c.Path, c.Args...) //nolint:gosec,noctx // G204: a fixed binary and validated arguments
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Dir, c.Env, stdin, outW, errW
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	_ = outW.Close()
	_ = errW.Close()
	if err != nil {
		_ = outR.Close()
		_ = errR.Close()
		return nil, fmt.Errorf("starting %s: %w", a.Dialect.Name(), err)
	}
	r := &Run{pid: cmd.Process.Pid, grace: a.Config.Grace, done: make(chan struct{})}
	if r.grace <= 0 {
		r.grace = DefaultGrace
	}
	maxLine := a.Config.MaxLine
	if maxLine <= 0 {
		maxLine = DefaultMaxLine
	}
	var (
		trMu    sync.Mutex
		tr      domain.Transcript
		tail    tailBuffer
		readers sync.WaitGroup
	)
	parser := a.Dialect.Parser()
	readers.Go(func() {
		readLines(outR, maxLine, h.Stdout, func(line []byte) {
			for _, e := range parser.Parse(line) {
				trMu.Lock()
				tr.Apply(e)
				trMu.Unlock()
				if h.OnEvent != nil {
					h.OnEvent(e)
				}
			}
		})
	})
	readers.Go(func() {
		w := io.Writer(&tail)
		if h.Stderr != nil {
			w = io.MultiWriter(&tail, h.Stderr)
		}
		_, _ = io.Copy(w, errR)
	})
	timer := time.AfterFunc(req.Timeout, func() { r.stop("", true) })
	go func() {
		select {
		case <-ctx.Done():
			r.stop("cancelled", false)
		case <-r.done:
		}
	}()
	go func() {
		state, werr := cmd.Process.Wait()
		timer.Stop()
		r.mu.Lock()
		r.exited = true
		// The CLI ended: whatever it left in its group must not outlive the run (or hold the pipes open). Under the
		// lock, so no cancel signals the group after this.
		_ = signalGroup(r.pid, true)
		r.mu.Unlock()
		drained := make(chan struct{})
		go func() { readers.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(drainWait): // a process outside the group still holds the pipes
			_ = outR.Close()
			_ = errR.Close()
			select {
			case <-drained:
			case <-time.After(drainWait): // a hook blocks: finish without it (it may still be running)
			}
		}
		_ = outR.Close()
		_ = errR.Close()
		r.mu.Lock()
		end := domain.End{Exit: exitOf(state, werr), StderrTail: tail.String(), TimedOut: r.timedOut, Cancelled: r.cancelled}
		trMu.Lock()
		r.result = tr.Result(end)
		trMu.Unlock()
		r.mu.Unlock()
		close(r.done)
	}()
	return r, nil
}

func exitOf(state *os.ProcessState, err error) domain.Exit {
	if err != nil || state == nil {
		return domain.Exit{Code: -1}
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return domain.Exit{Signaled: true}
	}
	return domain.Exit{Code: state.ExitCode()}
}

// PID is the CLI's process id (the leader of its process group).
func (r *Run) PID() int { return r.pid }

// Done is closed when the run ended and its result is known.
func (r *Run) Done() <-chan struct{} { return r.done }

// Wait blocks until the run ended and returns its result.
func (r *Run) Wait() domain.AgentResult {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}

// Cancel stops the run with reason: SIGTERM to its process group, SIGKILL after the grace period.
func (r *Run) Cancel(reason string) {
	if reason == "" {
		reason = "cancelled"
	}
	r.stop(reason, false)
}

func (r *Run) stop(reason string, timeout bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping || r.exited {
		return
	}
	r.stopping, r.cancelled, r.timedOut = true, reason, timeout
	_ = signalGroup(r.pid, false)
	go func() {
		t := time.NewTimer(r.grace)
		defer t.Stop()
		select {
		case <-r.done:
		case <-t.C:
			r.mu.Lock()
			if !r.exited {
				_ = signalGroup(r.pid, true)
			}
			r.mu.Unlock()
		}
	}()
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

// readLines passes everything read to raw and each line (up to maxLine bytes; a longer line is skipped) to fn.
func readLines(r io.Reader, maxLine int, raw io.Writer, fn func([]byte)) {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && raw != nil {
			_, _ = raw.Write(chunk)
		}
		complete := err == nil
		if !skipping {
			if len(line)+len(chunk) > maxLine {
				skipping, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if complete || (err != nil && !errors.Is(err, bufio.ErrBufferFull)) {
			if !skipping && len(line) > 0 {
				fn(line)
			}
			line, skipping = line[:0], false
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

// tailBuffer keeps the last stderrTail bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - stderrTail; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
