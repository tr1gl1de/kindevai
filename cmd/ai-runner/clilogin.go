package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// The AI CLIs' home on the runner (KIN-86, KB KIN-A-22): the HOME the CLIs run with and keep their login in. By
// default the runner user's HOME (as in KIN-79); --cli-home (or KINDEV_RUNNER_CLI_HOME) gives the CLIs a dedicated
// directory, so the runner's logins are separate from the operator's own and nothing else of the user's HOME is
// near them.

func cliHomeFlag(fs *flag.FlagSet) *string {
	return fs.String("cli-home", os.Getenv("KINDEV_RUNNER_CLI_HOME"),
		"the AI CLIs' HOME, where their logins live (default: the runner user's HOME)")
}

// cliHome resolves and prepares the CLI home: absolute, outside the runner's data directory (whose files never
// enter a sandbox as login files), created owner-only when missing.
func cliHome(flagValue, dataDir string) (string, error) {
	home := flagValue
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("the runner user's HOME: %w", err)
		}
		home = h
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" {
		return "", fmt.Errorf("--cli-home %q: not a clean absolute directory", home)
	}
	if dataDir != "" {
		d, h := resolved(dataDir), resolved(home)
		if h == d || strings.HasPrefix(h, d+"/") {
			return "", fmt.Errorf("--cli-home %s lies inside the runner's data directory", home)
		}
	}
	if err := os.MkdirAll(home, 0o700); err != nil { //nolint:gosec // G703: the operator's own --cli-home, checked above
		return "", fmt.Errorf("creating the CLI home: %w", err)
	}
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() { //nolint:gosec // G703: the operator's own --cli-home
		return "", fmt.Errorf("--cli-home %s is not a directory", home)
	}
	return home, nil
}

// resolved is p with its symlinks resolved as far as it exists.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if parent := filepath.Dir(p); parent != p {
		return filepath.Join(resolved(parent), filepath.Base(p))
	}
	return p
}

var errCLILoginUsage = errors.New("usage: ai-runner cli-login claude|codex|status [--cli-home DIR]")

// cliLogin is `ai-runner cli-login claude|codex|status`.
func cliLogin(ctx context.Context, args []string, e *env) error {
	if len(args) == 0 {
		return errCLILoginUsage
	}
	what := args[0]
	fs, dataDir := flags("cli-login "+what, e)
	home := cliHomeFlag(fs)
	var tokenFile *string
	if what == "claude" {
		tokenFile = fs.String("token-file", "", "read the token from this file (mode 0600)")
		fs.Func("token", "not supported: see --token-file", func(string) error { return errTokenArg })
		fs.SetOutput(io.Discard) // flag errors would echo the offending value, which may be the token
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid flags; see `ai-runner help`")
	}
	dir, err := cliHome(*home, *dataDir)
	if err != nil {
		return err
	}
	switch what {
	case "claude":
		if fs.NArg() > 0 {
			return errTokenArg
		}
		return loginClaude(dir, *tokenFile, e)
	case "codex":
		return loginCodex(ctx, dir, fs.Args(), e)
	case "status":
		printCLILogins(e.stdout, dir, e.lookPath, time.Now())
		return nil
	}
	return errCLILoginUsage
}

func loginClaude(home, tokenFile string, e *env) error {
	if tokenFile == "" && e.readSecret != nil {
		fmt.Fprintln(e.stderr, "Run `claude setup-token` (on any machine with a browser) and paste the token it prints.")
	}
	raw, err := readSecretInput(tokenFile, "Claude Code token from `claude setup-token` (input hidden): ", e)
	if err != nil {
		return err
	}
	now := time.Now()
	if err := cliagent.SaveClaudeToken(home, raw, now); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Stored the Claude Code token in %s (valid until about %s).\n",
		filepath.Join(home, cliagent.ClaudeTokenFile), now.Add(cliagent.ClaudeTokenLifetime).Format(time.DateOnly))
	fmt.Fprintln(e.stdout, "The runner passes it to Claude Code as "+cliagent.ClaudeTokenEnv+"; nothing needs refreshing.")
	return nil
}

// loginCodex runs `codex login <flags>` with HOME set to the CLI home and the terminal attached (its own flow:
// ChatGPT in a browser, --device-auth, or --with-api-key on stdin).
func loginCodex(ctx context.Context, home string, flags []string, e *env) error {
	look := e.lookPath
	if look == nil {
		look = exec.LookPath
	}
	bin, err := look("codex")
	if err != nil || !filepath.IsAbs(bin) {
		return errors.New("codex is not installed on this runner's PATH")
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"login"}, flags...)...) //nolint:gosec // G204: the operator's own command
	cmd.Env = append(withoutEnv(os.Environ(), "HOME", "CODEX_HOME"), "HOME="+home)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.stdin, e.stdout, e.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	return nil
}

func withoutEnv(env []string, keys ...string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, key := range keys {
			drop = drop || k == key
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// loginState describes a CLI's login in home for the operator (never a secret).
func loginState(agent, home string, now time.Time) (state string, ok bool) {
	switch agent {
	case sessiondomain.AgentClaude:
		t, found, err := cliagent.LoadClaudeToken(home)
		switch {
		case err != nil:
			return "unusable token file: " + err.Error(), false
		case found && t.Expired(now):
			return "long-lived token EXPIRED on " + t.Expires().Format(time.DateOnly) + ": run `ai-runner cli-login claude`", false
		case found:
			return "long-lived token, expires " + t.Expires().Format(time.DateOnly), true
		}
		if fi, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil && fi.Mode().IsRegular() { //nolint:gosec // G703: the CLI home
			return "interactive login (short-lived; a refresh inside the sandbox is lost: prefer `ai-runner cli-login claude`)", true
		}
	case sessiondomain.AgentCodex:
		if fi, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); err == nil && fi.Mode().IsRegular() { //nolint:gosec // G703: the CLI home
			return "logged in (auth.json; a ChatGPT login refreshed inside the sandbox is lost: prefer " +
				"`ai-runner cli-login codex -- --with-api-key`)", true
		}
	}
	return "not logged in", false
}

// printCLILogins is `ai-runner cli-login status`.
func printCLILogins(w io.Writer, home string, look func(string) (string, error), now time.Time) {
	if look == nil {
		look = exec.LookPath
	}
	fmt.Fprintf(w, "cli home: %s\n", home)
	for _, a := range []string{sessiondomain.AgentClaude, sessiondomain.AgentCodex} {
		state, _ := loginState(a, home, now)
		if _, err := look(a); err != nil {
			state += " (not installed)"
		}
		fmt.Fprintf(w, "%s: %s\n", a, state)
	}
}

// logCLILogins warns at start-up about a CLI a sandboxed run could not log in with, or a token near its end (the
// owner is alerted when an attempt fails for it: KIN-83).
func logCLILogins(g *cliagent.Agents, logger *slog.Logger, now time.Time) {
	for name, a := range g.Adapters {
		state, ok := loginState(name, a.Config.Home, now)
		if a.Dialect.LoggedIn(a.Config.Home, a.Config.Env, now) && !ok {
			continue // logged in through the operator's environment (an API key)
		}
		if !ok {
			logger.Warn("AI CLI not logged in for sandboxed runs: run `ai-runner cli-login "+name+"`", "agent", name,
				"state", state, "cliHome", a.Config.Home)
			continue
		}
		if t, found, _ := cliagent.LoadClaudeToken(a.Config.Home); name == sessiondomain.AgentClaude && found && t.ExpiresSoon(now) {
			logger.Warn("the Claude Code token expires soon: run `claude setup-token` and `ai-runner cli-login claude`",
				"expires", t.Expires().Format(time.DateOnly))
		}
	}
}

// sessionBinds returns, for an AI CLI attempt with a task, its task's session store bound where the CLI keeps its
// sessions (KIN-86).
func sessionBinds(s *cliagent.SessionStore) func(sessiondomain.Assignment) ([]egress.HomeBind, error) {
	return func(a sessiondomain.Assignment) ([]egress.HomeBind, error) {
		if a.Task == "" || !a.CLI() {
			return nil, nil
		}
		src, rel, err := s.Prepare(a.Project, a.Task, a.Adapter)
		if err != nil {
			return nil, err
		}
		return []egress.HomeBind{{Source: src, Rel: rel}}, nil
	}
}
