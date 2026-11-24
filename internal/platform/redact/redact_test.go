package redact

import (
	"strings"
	"testing"
)

func TestSecretKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"password", true}, {"newPassword", true}, {"api_key", true}, {"X-Api-Key", true}, {"Authorization", true},
		{"cookie", true}, {"client_secret", true}, {"accessToken", true}, {"private-key", true}, {"session", true},
		{"login", false}, {"project", false}, {"role", false}, {"tokenCount", false}, {"kind", false},
	} {
		if got := SecretKey(tc.key); got != tc.want {
			t.Errorf("SecretKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestSecretValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, v string
		want    bool
	}{
		{"bearer header", "Bearer abc.def", true},
		{"basic header", "basic dXNlcjpwYXNz", true},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl", true},
		{"github pat", "ghp_" + strings.Repeat("a1B", 12), true},
		{"github fine-grained", "github_pat_11ABCDEFG0123456789_abcdefghijklmnop", true},
		{"slack", "xoxb-1234-5678-abcdefghij", true},
		{"openai-like", "sk-" + strings.Repeat("Ab1", 12), true},
		{"youtrack permanent", "perm:YWRtaW4=.NDMtMQ==.abcdefghijklmnopqrst", true},
		{"telegram bot", "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw0", true},
		{"aws key id", "AKIAIOSFODNN7EXAMPLE", true},
		{"pem", "-----BEGIN PRIVATE KEY-----\nMIIB", true},
		{"url with credentials", "https://user:hunter2@example.com/repo.git", true},
		{"high-entropy mixed", "q8Zr2LmX9vB4nT7wY1kP6sD3fG5hJ0aQ", true},
		{"login", "alice", false},
		{"id base32", "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", false},
		{"commit sha", "3688ffd1c1a2b3c4d5e6f708192a3b4c5d6e7f80", false},
		{"url", "https://github.com/tr1gl1de/kindevai/pull/23", false},
		{"sentence", "Invitation accepted by the new member of Alpha", false},
		{"role", "project_admin", false},
		{"empty", "", false},
	} {
		if got := SecretValue(tc.v); got != tc.want {
			t.Errorf("%s: SecretValue(%q) = %v, want %v", tc.name, tc.v, got, tc.want)
		}
	}
}

func TestFields(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"login":    "alice",
		"password": "correct horse battery staple",
		"note":     "Bearer abc",
		"repo":     "https://github.com/a/b",
	}
	out := Fields(in)
	want := map[string]string{"login": "alice", "password": Placeholder, "note": Placeholder, "repo": "https://github.com/a/b"}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("Fields[%q] = %q, want %q", k, out[k], v)
		}
	}
	if in["password"] == Placeholder {
		t.Error("Fields must not modify its input")
	}
	if Fields(nil) != nil {
		t.Error("Fields(nil) must be nil")
	}
}

func TestText(t *testing.T) {
	t.Parallel()
	got := Text("push failed: https://bot:ghp_" + strings.Repeat("x1Y", 12) + "@github.com/a/b token=abc")
	if strings.Contains(got, "ghp_") || strings.Contains(got, "bot:") || strings.Contains(got, "abc") {
		t.Fatalf("Text left a secret: %q", got)
	}
	if !strings.HasPrefix(got, "push failed: ") {
		t.Fatalf("Text dropped plain words: %q", got)
	}
}
