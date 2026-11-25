//go:build unix

package cliagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

// CLI logins of a runner (KIN-86, KB KIN-A-22). In the strict sandbox the agent's HOME holds only copies of the
// CLI's login files and nothing is copied back, so a short-lived OAuth login refreshed inside is lost (and a rotated
// refresh token may log the host out). Claude Code therefore runs on a long-lived token (`claude setup-token`, one
// year) the operator stores with `ai-runner cli-login claude`: it stays on the host, owner-only, in the CLI's home,
// and reaches the CLI as CLAUDE_CODE_OAUTH_TOKEN; nothing needs refreshing and the agent cannot write it.

// ClaudeTokenFile is where, relative to the CLI's home, `ai-runner cli-login claude` keeps the token. It is not
// one of the login files copied into the sandbox (egress domain.CLIHomeFiles).
const ClaudeTokenFile = ".kindev/claude-oauth-token" //nolint:gosec // G101: a file name, not a credential

// ClaudeTokenLifetime is how long a `claude setup-token` token is valid; ClaudeTokenWarn how long before its end
// the runner warns.
const (
	ClaudeTokenLifetime = 365 * 24 * time.Hour
	ClaudeTokenWarn     = 30 * 24 * time.Hour
)

// ClaudeTokenEnv is the variable Claude Code reads a long-lived OAuth token from.
const ClaudeTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // G101: a variable name, not a credential

// Token is a stored long-lived CLI token.
type Token struct {
	Value   string    `json:"token"`
	Created time.Time `json:"created"`
}

// Expired reports a token past its lifetime.
func (t Token) Expired(now time.Time) bool { return !now.Before(t.Created.Add(ClaudeTokenLifetime)) }

// ExpiresSoon reports a token within ClaudeTokenWarn of its end (or past it).
func (t Token) ExpiresSoon(now time.Time) bool {
	return !now.Before(t.Created.Add(ClaudeTokenLifetime - ClaudeTokenWarn))
}

// Expires is when the token ends.
func (t Token) Expires() time.Time { return t.Created.Add(ClaudeTokenLifetime) }

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{20,1000}$`)

// CheckToken validates a token as pasted (surrounding whitespace is dropped) and returns it.
func CheckToken(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if !tokenRe.MatchString(v) {
		return "", errors.New("this does not look like a token from `claude setup-token`")
	}
	return v, nil
}

// SaveClaudeToken stores the token (created now) in home, owner-only, replacing an older one.
func SaveClaudeToken(home, raw string, now time.Time) error {
	v, err := CheckToken(raw)
	if err != nil {
		return err
	}
	p := filepath.Join(home, ClaudeTokenFile)
	if err := secfile.EnsureDir(filepath.Dir(p)); err != nil {
		return err
	}
	b, err := json.Marshal(Token{Value: v, Created: now.UTC()})
	if err != nil {
		return err
	}
	return secfile.WriteReplace(p, b)
}

// LoadClaudeToken reads the stored token: ok false when there is none; an error when the file is unusable (not
// owner-only, not the runner user's, unreadable), so a token others could have written is never used.
func LoadClaudeToken(home string) (Token, bool, error) {
	b, err := secfile.ReadPrivate(filepath.Join(home, ClaudeTokenFile), 16<<10)
	if errors.Is(err, fs.ErrNotExist) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("the stored Claude token: %w", err)
	}
	var t Token
	if json.Unmarshal(b, &t) != nil || !tokenRe.MatchString(t.Value) || t.Created.IsZero() {
		return Token{}, false, errors.New("the stored Claude token is unreadable: run `ai-runner cli-login claude` again")
	}
	return t, true, nil
}

// regular reports a regular file at rel in home.
func regular(home, rel string) bool {
	fi, err := os.Stat(filepath.Join(home, rel))
	return err == nil && fi.Mode().IsRegular()
}

// hasEnv reports one of keys set (non-empty) in env.
func hasEnv(env []string, keys ...string) bool {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		for _, want := range keys {
			if k == want && v != "" {
				return true
			}
		}
	}
	return false
}

// SessionDir: Claude Code keeps its transcripts per working directory under ~/.claude/projects.
func (Claude) SessionDir() string { return ".claude/projects" }

// LoggedIn: the runner's token (unexpired), a login file or an operator-provided key.
func (Claude) LoggedIn(home string, env []string, now time.Time) bool {
	if hasEnv(env, ClaudeTokenEnv, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN") || regular(home, ".claude/.credentials.json") {
		return true
	}
	t, ok, err := LoadClaudeToken(home)
	return err == nil && ok && !t.Expired(now)
}

// LoginEnv passes the runner's stored token (if any) as CLAUDE_CODE_OAUTH_TOKEN.
func (Claude) LoginEnv(home string) ([]string, []string, error) {
	t, ok, err := LoadClaudeToken(home)
	if err != nil || !ok {
		return nil, nil, err
	}
	return []string{ClaudeTokenEnv + "=" + t.Value}, []string{t.Value}, nil
}

// SessionDir: Codex keeps its rollouts under ~/.codex/sessions.
func (Codex) SessionDir() string { return ".codex/sessions" }

// LoggedIn: `codex login` (ChatGPT or API key) wrote auth.json, or the operator provides a key.
func (Codex) LoggedIn(home string, env []string, _ time.Time) bool {
	return hasEnv(env, "OPENAI_API_KEY", "CODEX_API_KEY") || regular(home, ".codex/auth.json")
}

// LoginEnv: Codex reads its login from auth.json (copied into the sandbox).
func (Codex) LoginEnv(string) ([]string, []string, error) { return nil, nil, nil }
