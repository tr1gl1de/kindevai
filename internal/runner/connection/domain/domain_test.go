package domain

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The fingerprint must match the Control Plane's (KIN-A-17) and `ssh-keygen -lf` for the same key.
func TestFingerprintMatchesOpenSSH(t *testing.T) {
	t.Parallel()
	// RFC 8032 test 1 seed; `ssh-keygen -lf` of its public key prints this fingerprint.
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, []byte{0x9d, 0x61, 0xb1, 0x9d, 0xef, 0xfd, 0x5a, 0x60, 0xba, 0x84, 0x4a, 0xf4, 0x92, 0xec, 0x2c, 0xc4,
		0x44, 0x49, 0xc5, 0x69, 0x7b, 0x32, 0x69, 0x19, 0x70, 0x3b, 0xac, 0x03, 0x1c, 0xae, 0x7f, 0x60})
	id, err := NewIdentity(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id.PublicKeyBase64(), "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="; got != want {
		t.Fatalf("public key = %s, want %s", got, want)
	}
	if got, want := id.Fingerprint(), "SHA256:bbXpuKG6zhzdmnxq256TlqzFBzRl2f6OOg722cYNbU8"; got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
}

func TestNewIdentityRejectsBadKeys(t *testing.T) {
	t.Parallel()
	if _, err := NewIdentity(nil); err == nil {
		t.Fatal("nil key accepted")
	}
	if _, err := NewIdentity(make(ed25519.PrivateKey, 10)); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestParseServerURL(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"https://cp.example.com":           "https://cp.example.com",
		"https://cp.example.com/":          "https://cp.example.com",
		"https://cp.example.com:8443/kin/": "https://cp.example.com:8443/kin",
		"HTTPS://CP.example.com":           "https://CP.example.com",
		"http://127.0.0.1:8080":            "http://127.0.0.1:8080",
		"http://localhost:8080":            "http://localhost:8080",
		"http://[::1]:8080":                "http://[::1]:8080",
		"http://127.0.0.2:8080":            "http://127.0.0.2:8080",
	}
	for in, want := range ok {
		got, err := ParseServerURL(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseServerURL(%q) = %q, %v; want %q", in, got.String(), err, want)
		}
	}
	bad := map[string]error{
		"":                               ErrServerURL,
		"cp.example.com":                 ErrServerURL,
		"ftp://cp.example.com":           ErrServerURL,
		"https://":                       ErrServerURL,
		"https://user:pw@cp.example.com": ErrServerURL,
		"https://cp.example.com/?a=b":    ErrServerURL,
		"https://cp.example.com/#x":      ErrServerURL,
		"http://cp.example.com":          ErrPlainHTTP,
		"http://10.0.0.5:8080":           ErrPlainHTTP,
		"http://localhost.evil.com":      ErrPlainHTTP,
		"ws://127.0.0.1":                 ErrServerURL,
	}
	for in, want := range bad {
		if _, err := ParseServerURL(in); !errors.Is(err, want) {
			t.Errorf("ParseServerURL(%q) err = %v, want %v", in, err, want)
		}
	}
}

func TestParseToken(t *testing.T) {
	t.Parallel()
	tok := strings.Repeat("a", 43)
	if got, err := ParseToken("  " + tok + "\n"); err != nil || got.Reveal() != tok {
		t.Fatalf("ParseToken = %q, %v", got.Reveal(), err)
	}
	for _, in := range []string{"", "   \n", "ab cd", "a\x00b", strings.Repeat("a", 513)} {
		if _, err := ParseToken(in); !errors.Is(err, ErrTokenFormat) {
			t.Errorf("ParseToken(%q) err = %v", in, err)
		}
	}
}

// A token never shows up in logs or error messages by accident.
func TestTokenDoesNotFormat(t *testing.T) {
	t.Parallel()
	tok, _ := ParseToken("secret-token-value")
	for _, s := range []string{tok.String(), fmt.Sprintf("%v %s %+v %#v", tok, tok, tok, tok)} {
		if strings.Contains(s, "secret") {
			t.Fatalf("token leaked: %q", s)
		}
	}
}

func TestEnrollmentEnrolled(t *testing.T) {
	t.Parallel()
	if (Enrollment{}).Enrolled() {
		t.Fatal("empty enrollment is enrolled")
	}
	srv, _ := ParseServerURL("https://cp.example.com")
	if !(Enrollment{Server: srv, RunnerID: "r1"}).Enrolled() {
		t.Fatal("enrollment with runner id not enrolled")
	}
}
