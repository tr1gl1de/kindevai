//go:build unix

package cliagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Command is the CLI's process: the launcher runs it through `agent-exec` (KIN-79), inside the strict egress
// sandbox when the attempt's profile is strict (KIN-31).
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// Defaults of Config and of agent-exec.
const (
	DefaultPath = "/usr/local/bin:/usr/bin:/bin"
	// DefaultGrace is agent-exec's time between SIGTERM and SIGKILL when the run's timeout passes.
	DefaultGrace   = 5 * time.Second
	DefaultMaxLine = 4 << 20
	// stderrTail is how much of the end of stderr is kept to explain a failure.
	stderrTail = 4 << 10
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
	// MaxLine bounds one output line kept for parsing (longer lines are passed through but not parsed);
	// DefaultMaxLine when zero.
	MaxLine int
}

// Adapter builds the command of one CLI.
type Adapter struct {
	Dialect Dialect
	Config  Config
}

// Command validates the request and returns the CLI's command in permission mode p (the caller decided p with
// domain.ChoosePermissions and runs the command with the prompt file on stdin).
func (a *Adapter) Command(req domain.AgentRequest, p domain.Permissions) (Command, error) {
	if err := a.check(req); err != nil {
		return Command{}, err
	}
	return a.command(req, p)
}

func (a *Adapter) command(req domain.AgentRequest, p domain.Permissions) (Command, error) {
	env, err := a.env()
	if err != nil {
		return Command{}, err
	}
	return Command{Path: a.Config.Binary, Args: a.Dialect.Args(req, p), Env: env, Dir: req.Workdir}, nil
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
func (a *Adapter) env() ([]string, error) {
	path := a.Config.Path
	if path == "" {
		path = DefaultPath + ":" + filepath.Dir(a.Config.Binary)
	}
	login, _, err := a.Dialect.LoginEnv(a.Config.Home)
	if err != nil {
		return nil, err
	}
	env := append([]string{}, a.Config.Env...)
	env = append(env, login...)
	env = append(env, a.Dialect.Env()...)
	return append(env, "HOME="+a.Config.Home, "PATH="+path, "LANG=C.UTF-8", "TERM=dumb", "NO_COLOR=1"), nil
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
