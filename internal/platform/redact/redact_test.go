package redact

import (
	"strings"
	"testing"
	"time"
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
		{"prose after token", "token expired", false},
		{"prose after basic", "basic setup done", false},
		{"name starting with token", "Token Service", false},
		{"word-separated slug", "Feature-Add-Login-Page-2024-Fix", false},
		{"branch path", "feature/KIN-16-Append-Only-Audit-Log", false},
		{"token header", "token Zm9vYmFyYmF6cXV4MTIzNDU2", true},
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
	for _, plain := range []string{"token expired for alice", "basic setup done", "Token Service renamed"} {
		if got := Text(plain); got != plain {
			t.Errorf("Text(%q) = %q, want unchanged", plain, got)
		}
	}
	if got := Text("sent Bearer abc.def"); got != "sent Bearer "+Placeholder {
		t.Errorf("bearer value kept: %q", got)
	}
	got := Text("push failed: https://bot:ghp_" + strings.Repeat("x1Y", 12) + "@github.com/a/b token=abc")
	if strings.Contains(got, "ghp_") || strings.Contains(got, "bot:") || strings.Contains(got, "abc") {
		t.Fatalf("Text left a secret: %q", got)
	}
	if !strings.HasPrefix(got, "push failed: ") {
		t.Fatalf("Text dropped plain words: %q", got)
	}
}

// KIN-45: credentials passed as URL query parameters or quoted JSON fields inside error texts are masked.
func TestTextMasksSecretParameters(t *testing.T) {
	t.Parallel()
	in := `Get "https://api.example.com/v1/me?page=2&access_token=abc123&x=1": dial tcp: timeout; ` +
		`body {"error":"bad_request","client_secret":"zz9"} /hook?token=t0k3n`
	got := Text(in)
	for _, leak := range []string{"abc123", "zz9", "t0k3n"} {
		if strings.Contains(got, leak) {
			t.Errorf("Text leaks %q: %s", leak, got)
		}
	}
	for _, keep := range []string{"page=2", "x=1", "dial tcp: timeout;", `"error":"bad_request"`, "access_token=", "/hook?token="} {
		if !strings.Contains(got, keep) {
			t.Errorf("Text lost %q: %s", keep, got)
		}
	}
	for _, plain := range []string{"sha256=3f2a9c", "https://github.com/a/b/pulls?state=open&page=2", `{"tokens_used":3}`} {
		if got := Text(plain); got != plain {
			t.Errorf("Text(%q) = %q, want unchanged", plain, got)
		}
	}
}

// KIN-45: Credentials masks only unambiguous credential shapes, so ids in webhook payloads survive.
func TestCredentials(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"Authorization: Bearer abcDEF123456",
		"use ghp_" + strings.Repeat("a1B", 12) + " here",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
		"https://user:hunter2@example.com/repo.git",
		"https://example.com/hook?access_token=abc123",
		"password=hunter2",
		"-----BEGIN PRIVATE KEY-----\nMIIB",
	} {
		if got := Credentials(s); got == s || !strings.Contains(got, Placeholder) {
			t.Errorf("Credentials(%q) = %q, want masked", s, got)
		}
	}
	for _, s := range []string{
		"MDExOlB1bGxSZXF1ZXN0MQ==", "PR_kwDOJx8Yrs5abcdEFGH12", "q8Zr2LmX9vB4nT7wY1kP6sD3fG5hJ0aQ",
		"3688ffd1c1a2b3c4d5e6f708192a3b4c5d6e7f80", "Token refresh fails for basic auth users",
		"https://github.com/tr1gl1de/kindevai/pull/23", "feature/KIN-15-outbox-inbox", "",
		"Bearer of bad news", "the token expired", "page=2&state=open",
	} {
		if got := Credentials(s); got != s {
			t.Errorf("Credentials(%q) = %q, want unchanged", s, got)
		}
	}
}

// KIN-45 review: masking a pair keeps the rest of the word; tokens in quotes or brackets are found; header and
// "key: value" forms in error texts are masked; prose with sk-/perm-/token words is not a credential.
func TestScrubEdgeCases(t *testing.T) {
	t.Parallel()
	pat := "ghp_" + strings.Repeat("a1B", 12)
	for _, tc := range []struct{ in, leak, keep string }{
		{"https://x/cb?client_secret=abc9&state=open", "abc9", "&state=open"},
		{`use "` + pat + `" here`, pat, `use "`},
		{"(" + pat + ")", pat, "("},
		{"Authorization:Bearer abcDEF123456 sent", "abcDEF123456", "sent"},
		{"Authorization: Bearer abcDEF123456 sent", "abcDEF123456", "sent"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIBsecret\n-----END RSA PRIVATE KEY----- tail", "MIIBsecret", "tail"},
	} {
		for name, f := range map[string]func(string) string{"Text": Text, "Credentials": Credentials} {
			got := f(tc.in)
			if strings.Contains(got, tc.leak) || !strings.Contains(got, tc.keep) {
				t.Errorf("%s(%q) = %q: must drop %q and keep %q", name, tc.in, got, tc.leak, tc.keep)
			}
		}
	}
	for _, in := range []string{"login failed, password: hunter2", `body {"password": 12345}`} {
		if got := Text(in); strings.Contains(got, "hunter2") || strings.Contains(got, "12345") {
			t.Errorf("Text(%q) = %q leaks the value", in, got)
		}
	}
	for _, plain := range []string{"the token authentication-failed here", "uses sk-learn-pipeline", "grant perm-admin-role",
		"Reset password: steps below"} {
		if got := Credentials(plain); got != plain {
			t.Errorf("Credentials(%q) = %q, want unchanged", plain, got)
		}
	}
}

// 64 KiB of adversarial text stays well within a request's budget.
func TestScrubLargeInput(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("a=b&token=x9 \"k\":\"v\" Bearer ", 64<<10/27)
	start := time.Now()
	_ = Text(in)
	_ = Credentials(in)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("scrubbing 64 KiB took %s", d)
	}
}
