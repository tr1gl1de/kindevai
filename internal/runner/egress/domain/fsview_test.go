package domain

import (
	"path"
	"slices"
	"strings"
	"testing"
)

func TestFilesystemLeakTestsAreRequired(t *testing.T) {
	t.Parallel()
	for _, test := range []string{LeakFSHome, LeakFSData, LeakFSWrite} {
		if !slices.Contains(LeakTests, test) {
			t.Fatalf("%s is not a leak test", test)
		}
		var ls []Leak
		for _, l := range allBlocked() {
			if l.Test != test {
				ls = append(ls, l)
			}
		}
		if err := (Verdict{Leaks: ls}).Verified(true); err == nil || !strings.Contains(err.Error(), test) {
			t.Fatalf("a verdict without %s verified: %v", test, err)
		}
		ls = append(ls, Leak{Test: test, Target: "/home/u/.ssh"})
		if err := (Verdict{Leaks: ls}).Verified(true); err == nil || !strings.Contains(err.Error(), "/home/u/.ssh") {
			t.Fatalf("a verdict with a visible %s verified: %v", test, err)
		}
	}
}

func TestCLIHomeFiles(t *testing.T) {
	t.Parallel()
	creds := 0
	for _, f := range CLIHomeFiles {
		if f.Path == "" || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "..") {
			t.Errorf("%q is not a clean path relative to HOME", f.Path)
		}
		if f.Credential {
			creds++
		}
	}
	for _, want := range []string{".claude/.credentials.json", ".codex/auth.json"} {
		i := slices.IndexFunc(CLIHomeFiles, func(f HomeFile) bool { return f.Path == want })
		if i < 0 || !CLIHomeFiles[i].Credential {
			t.Errorf("%s is not a credential file of the CLI home", want)
		}
	}
	for _, f := range CLIHomeFiles {
		if strings.Contains(f.Path, ".ssh") || strings.Contains(f.Path, "kindevai") {
			t.Errorf("%s must never be copied into the sandbox", f.Path)
		}
	}
}

func TestCredentialUpdate(t *testing.T) {
	t.Parallel()
	orig := `{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1","expiresAt":1}}`
	refreshed := `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2","expiresAt":2}}`
	codex := `{"OPENAI_API_KEY":null,"tokens":{"access_token":"a","refresh_token":"r","account_id":"acc-1"}}`
	tests := []struct {
		name                   string
		orig, current, updated string
		want                   bool
	}{
		{"a refreshed token is written back", orig, orig, refreshed, true},
		{"unchanged", orig, orig, orig, false},
		{"the host copy changed meanwhile", orig, `{"claudeAiOauth":{"accessToken":"a3","refreshToken":"r3","expiresAt":3}}`, refreshed, false},
		{"not JSON", orig, orig, `token=x`, false},
		{"not an object", orig, orig, `["a"]`, false},
		{"other keys", orig, orig, `{"claudeAiOauth":{"accessToken":"a2"},"hooks":{"x":"y"}}`, false},
		{"a key dropped", orig, orig, `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`, false},
		{"too large", orig, orig, `{"claudeAiOauth":{"accessToken":"` + strings.Repeat("a", MaxCredentialSize) + `","refreshToken":"r","expiresAt":1}}`, false},
		{"same account refreshed", codex, codex, strings.ReplaceAll(codex, `"a"`, `"a2"`), true},
		{"another account", codex, codex, strings.ReplaceAll(codex, "acc-1", "acc-2"), false},
		{"no original", "", "", refreshed, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CredentialUpdate([]byte(tc.orig), []byte(tc.current), []byte(tc.updated)); got != tc.want {
				t.Fatalf("CredentialUpdate = %v, want %v", got, tc.want)
			}
		})
	}
}
