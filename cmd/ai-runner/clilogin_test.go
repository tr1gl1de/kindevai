package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

const claudeToken = "sk-ant-oat01-0123456789abcdefghijklmnopqrstuvwxyz-ABCDEFGH"

// AC (KIN-86): the operator logs Claude Code in for the runner with an explicit command: the long-lived token from
// `claude setup-token` is read from stdin / the hidden prompt / a file (never an argument) and kept on the host in
// the dedicated CLI home, owner-only.
func TestCLILoginClaudeStoresTheToken(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "cli-home")
	data := filepath.Join(t.TempDir(), "data")
	r := cli(t.Context(), data, claudeToken+"\n", nil, "cli-login", "claude", "--cli-home", home)
	if r.err != nil {
		t.Fatalf("err %v\n%s", r.err, r.stderr)
	}
	tok, ok, err := cliagent.LoadClaudeToken(home)
	if err != nil || !ok || tok.Value != claudeToken {
		t.Fatalf("stored %v %v", ok, err)
	}
	if fi, err := os.Stat(home); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cli home %v %v", fi, err)
	}
	if strings.Contains(r.stdout+r.stderr, claudeToken) {
		t.Fatal("the token was printed")
	}
	// On a terminal through the no-echo prompt.
	var asked string
	prompt := func(p string) (string, error) { asked = p; return claudeToken, nil }
	if r := cli(t.Context(), data, "", prompt, "cli-login", "claude", "--cli-home", home); r.err != nil || !strings.Contains(asked, "setup-token") {
		t.Fatalf("err %v, prompt %q", r.err, asked)
	}
	// Never as an argument; garbage is refused.
	if r := cli(t.Context(), data, "", nil, "cli-login", "claude", "--cli-home", home, "--token", claudeToken); r.err == nil ||
		strings.Contains(r.err.Error()+r.stderr, claudeToken) {
		t.Fatalf("token argument: %v %s", r.err, r.stderr)
	}
	if r := cli(t.Context(), data, "not a token\n", nil, "cli-login", "claude", "--cli-home", t.TempDir()); r.err == nil {
		t.Fatal("garbage accepted")
	}
	// status shows what is configured and when the token ends, never the token.
	r = cli(t.Context(), data, "", nil, "cli-login", "status", "--cli-home", home)
	if r.err != nil || !strings.Contains(r.stdout, "claude: long-lived token") || !strings.Contains(r.stdout, "expires") ||
		strings.Contains(r.stdout, claudeToken) || !strings.Contains(r.stdout, "codex: not logged in") {
		t.Fatalf("status %v\n%s", r.err, r.stdout)
	}
}

// The CLI home must not lie inside the runner's data directory (the sandbox never copies login files from there).
func TestCLIHomeOutsideTheDataDir(t *testing.T) {
	t.Parallel()
	data := filepath.Join(t.TempDir(), "data")
	r := cli(t.Context(), data, claudeToken, nil, "cli-login", "claude", "--data-dir", data, "--cli-home", filepath.Join(data, "cli"))
	if r.err == nil || !strings.Contains(r.err.Error(), "data directory") {
		t.Fatalf("err %v", r.err)
	}
	if r := cli(t.Context(), data, claudeToken, nil, "cli-login", "claude", "--cli-home", "relative"); r.err == nil {
		t.Fatal("relative CLI home accepted")
	}
}

// `ai-runner cli-login codex` runs `codex login` (its own flow: ChatGPT or --with-api-key) with HOME set to the
// CLI home, so the login lands there and not in the runner user's HOME.
func TestCLILoginCodexUsesTheCLIHome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n[ \"$1\" = login ] || exit 3\nmkdir -p \"$HOME/.codex\" && echo \"$2\" > \"$HOME/.codex/auth.json\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable test script
		t.Fatal(err)
	}
	home := filepath.Join(dir, "cli-home")
	var out, errOut bytes.Buffer
	e := &env{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut, defaultDataDir: filepath.Join(dir, "data"),
		hostname: func() (string, error) { return "h", nil }, lookPath: func(string) (string, error) { return fake, nil }}
	if err := run(t.Context(), []string{"cli-login", "codex", "--cli-home", home, "--", "--device-auth"}, e); err != nil {
		t.Fatalf("err %v\n%s", err, errOut.String())
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil || strings.TrimSpace(string(b)) != "--device-auth" {
		t.Fatalf("auth.json %q %v", b, err)
	}
}

// AC: the confiner binds the task's own session store into the sandbox; a mock-agent attempt gets none.
func TestConfinerSessionBinds(t *testing.T) {
	t.Parallel()
	store := &cliagent.SessionStore{Root: filepath.Join(t.TempDir(), "cli-sessions")}
	binds := sessionBinds(store)
	a := sessiondomain.Assignment{Project: "P1", Task: "T1", Adapter: sessiondomain.AgentClaude}
	got, notice, err := binds(a)
	if err != nil || notice != "" || len(got) != 1 || got[0].Source != filepath.Join(store.Root, "P1", "T1", "claude") ||
		got[0].Rel != ".claude/projects" {
		t.Fatalf("binds %+v %v", got, err)
	}
	if got, _, err := binds(sessiondomain.Assignment{Project: "P1", Adapter: sessiondomain.AgentClaude}); err != nil || got != nil {
		t.Fatalf("no task: %+v %v", got, err)
	}
}

// AC (KIN-96): a session store over its limit is not shrunk silently: the bind carries the notice, and the
// sandbox's summary (shown in the attempt's output) leads with it.
func TestConfinerSessionStoreOverLimit(t *testing.T) {
	t.Parallel()
	store := &cliagent.SessionStore{Root: filepath.Join(t.TempDir(), "cli-sessions"), Limit: 10}
	binds := sessionBinds(store)
	a := sessiondomain.Assignment{Project: "P1", Task: "T1", Adapter: sessiondomain.AgentClaude}
	got, _, err := binds(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(got[0].Source, "s.jsonl"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	_, notice, err := binds(a)
	if err != nil || !strings.Contains(notice, "limit") {
		t.Fatalf("notice %q %v", notice, err)
	}
	sb := withNotice(summarySandbox{summary: "denied: evil.example"}, notice)
	if s := sb.Summary(); !strings.HasPrefix(s, notice) || !strings.Contains(s, "denied: evil.example") {
		t.Fatalf("summary %q", s)
	}
	if s := withNotice(summarySandbox{}, notice).Summary(); s != notice {
		t.Fatalf("summary without the sandbox's own %q", s)
	}
	plain := summarySandbox{summary: "x"}
	if withNotice(plain, "") != sessionapp.Sandbox(plain) {
		t.Fatal("no notice: the sandbox is not wrapped")
	}
}

type summarySandbox struct {
	sessionapp.Sandbox
	summary string
}

func (s summarySandbox) Summary() string { return s.summary }

// The daemon warns about CLIs a sandboxed run could not log in with, and about a token near its end.
func TestLogCLILogins(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := cliagent.SaveClaudeToken(home, claudeToken, time.Now().Add(-340*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	g := cliAgents(home, func(n string) (string, error) { return "/usr/bin/" + n, nil }, slog.New(slog.DiscardHandler))
	logCLILogins(g, slog.New(slog.NewTextHandler(&buf, nil)), time.Now())
	if s := buf.String(); !strings.Contains(s, "expires") || !strings.Contains(s, "codex") || strings.Contains(s, claudeToken) {
		t.Fatalf("log:\n%s", s)
	}
}
