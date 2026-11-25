//go:build unix

package cliagent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

const testToken = "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz0123456789_-ABCDEF"

// `ai-runner cli-login claude` keeps the long-lived token on the host, owner-only, in the CLI's home.
func TestSaveAndLoadClaudeToken(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	if err := SaveClaudeToken(home, "  "+testToken+"\n", now); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(home, ClaudeTokenFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %v, %v", fi, err)
	}
	tok, ok, err := LoadClaudeToken(home)
	if err != nil || !ok || tok.Value != testToken || !tok.Created.Equal(now) {
		t.Fatalf("loaded %+v %v %v", tok, ok, err)
	}
	if tok.Expired(now.Add(364*24*time.Hour)) || !tok.Expired(now.Add(366*24*time.Hour)) {
		t.Fatal("a setup-token token lives a year")
	}
	if tok.ExpiresSoon(now.Add(300*24*time.Hour)) || !tok.ExpiresSoon(now.Add(340*24*time.Hour)) {
		t.Fatal("the warning starts a month before the token expires")
	}
	if _, ok, err := LoadClaudeToken(t.TempDir()); ok || err != nil {
		t.Fatalf("no token: ok %v, err %v", ok, err)
	}
	for _, bad := range []string{"", "short", "with space inside the token value here", "sk-ant-\x00" + testToken,
		strings.Repeat("a", 5000)} {
		if err := SaveClaudeToken(t.TempDir(), bad, now); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	// A token file others can read is refused, not used.
	if err := os.Chmod(filepath.Join(home, ClaudeTokenFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadClaudeToken(home); err == nil {
		t.Fatal("a world-readable token file was used")
	}
}

// The token reaches the CLI as CLAUDE_CODE_OAUTH_TOKEN (it never needs a refresh) and is known as a secret, so the
// attempt's output masks it.
func TestClaudeTokenReachesTheCLI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if err := SaveClaudeToken(f.home, testToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	g := agentsFor(f, Claude{})
	dir := t.TempDir()
	cmd, err := g.Prepare(dir, f.request(domain.AgentClaude), "hi", domain.PermissionsSafe)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+testToken) {
		t.Fatalf("env %v", cmd.Env)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), testToken) {
		t.Fatal("the token is on the command line")
	}
	if !slices.Contains(g.Secrets(), testToken) {
		t.Fatalf("secrets %v", g.Secrets())
	}
}

// In the strict sandbox only the copied login files and the token count: a CLI without any login fails before it
// starts, as a logged-out CLI (cli_limit/auth: the step is held and the owner alerted, KIN-83), and so does an
// expired token.
func TestConfinedRunNeedsALogin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		agent Dialect
		setup func(t *testing.T, home string)
		env   []string
		ok    bool
	}{
		{"claude without a login", Claude{}, nil, nil, false},
		{"claude with a token", Claude{}, func(t *testing.T, h string) { saveToken(t, h, time.Now()) }, nil, true},
		{"claude with an expired token", Claude{}, func(t *testing.T, h string) {
			saveToken(t, h, time.Now().Add(-400*24*time.Hour))
		}, nil, false},
		{"claude with a credentials file", Claude{}, func(t *testing.T, h string) { writeFile(t, h, ".claude/.credentials.json") }, nil, true},
		{"claude with an API key from the operator", Claude{}, nil, []string{"ANTHROPIC_API_KEY=k"}, true},
		{"codex without a login", Codex{}, nil, nil, false},
		{"codex logged in", Codex{}, func(t *testing.T, h string) { writeFile(t, h, ".codex/auth.json") }, nil, true},
		{"codex with an API key from the operator", Codex{}, nil, []string{"OPENAI_API_KEY=k"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if tc.setup != nil {
				tc.setup(t, f.home)
			}
			g := agentsFor(f, tc.agent, tc.env...)
			req := f.request(tc.agent.Name())
			req.Confined = true
			_, err := g.Prepare(t.TempDir(), req, "hi", domain.PermissionsBypass)
			if tc.ok != (err == nil) || !tc.ok && !errors.Is(err, app.ErrAgentLoggedOut) {
				t.Fatalf("err %v, want ok %v", err, tc.ok)
			}
			// On the host the CLI's own login may live elsewhere (macOS keychain): never refused there.
			req.Confined = false
			if _, err := g.Prepare(t.TempDir(), req, "hi", domain.PermissionsSafe); err != nil {
				t.Fatalf("unconfined: %v", err)
			}
		})
	}
}

func saveToken(t *testing.T, home string, created time.Time) {
	t.Helper()
	if err := SaveClaudeToken(home, testToken, created); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, home, rel string) {
	t.Helper()
	p := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}
