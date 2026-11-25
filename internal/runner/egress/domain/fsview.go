package domain

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// The filesystem view of a strict sandbox (KIN-80, KB KIN-A-22): the agent sees the system directories read-only,
// its worktree and attempt directory, the repository mirror read-only (its own worktree's index and HEAD writable),
// private /tmp and HOME, and nothing else of the host. Its HOME holds copies of the CLI's login files only.

// HomeFile is a file copied from the CLI's HOME into the agent's private HOME. A Credential file the CLI refreshed
// is written back (CredentialUpdate), so a rotated refresh token does not log the CLI out on the host.
type HomeFile struct {
	Path       string // relative to HOME
	Credential bool
}

// CLIHomeFiles are the CLI login and settings files the agent's HOME receives (Claude Code, Codex); nothing else
// of the CLI's HOME (keys, other projects, shell history) enters the sandbox.
var CLIHomeFiles = []HomeFile{
	{Path: ".claude/.credentials.json", Credential: true},
	{Path: ".claude.json"},
	{Path: ".claude/settings.json"},
	{Path: ".codex/auth.json", Credential: true},
	{Path: ".codex/config.toml"},
}

// MaxCredentialSize bounds a credential file written back to the host.
const MaxCredentialSize = 64 << 10

// identityKeys are fields that name the account a credential belongs to: a write-back must not change them.
var identityKeys = []string{"account_id", "accountUuid", "organizationUuid", "email"}

// CredentialUpdate decides whether a credential file the agent's CLI rewrote (updated) replaces the host's copy:
// only when it changed, the host copy still holds what was copied in (orig; another attempt did not refresh it
// meanwhile), and it is a bounded JSON object of exactly the original's shape for the same account. The agent
// controls updated, so nothing but a refreshed token of the same login may reach the host.
func CredentialUpdate(orig, current, updated []byte) bool {
	if len(orig) == 0 || len(updated) > MaxCredentialSize || bytes.Equal(orig, updated) || !bytes.Equal(orig, current) {
		return false
	}
	var o, u map[string]any
	if json.Unmarshal(orig, &o) != nil || json.Unmarshal(updated, &u) != nil || o == nil || u == nil {
		return false
	}
	ok, oi, ui := slices.Equal(keyPaths(o, ""), keyPaths(u, "")), map[string]any{}, map[string]any{}
	identities(o, "", oi)
	identities(u, "", ui)
	for k, v := range oi {
		if ui[k] != v {
			return false
		}
	}
	return ok
}

func keyPaths(m map[string]any, prefix string) []string {
	var out []string
	for k, v := range m {
		p := prefix + "/" + k
		out = append(out, p)
		if sub, ok := v.(map[string]any); ok {
			out = append(out, keyPaths(sub, p)...)
		}
	}
	slices.Sort(out)
	return out
}

func identities(m map[string]any, prefix string, out map[string]any) {
	for k, v := range m {
		p := prefix + "/" + k
		if sub, ok := v.(map[string]any); ok {
			identities(sub, p, out)
		} else if slices.ContainsFunc(identityKeys, func(id string) bool { return strings.EqualFold(id, k) }) {
			out[p] = v
		}
	}
}
