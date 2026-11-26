package protocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

// TestAuthTranscriptVector pins the canonical signed payload of runner.authenticate. Changing it breaks every
// deployed runner: bump AuthContext's version instead.
func TestAuthTranscriptVector(t *testing.T) {
	t.Parallel()
	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(0xa0 + i%16)
	}
	got, err := AuthTranscript(AuthFields{
		RunnerID: "runner-1", Audience: "https://cp.example.com", Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		ExpiresAt: 1795000000, ConnectionID: "conn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "kindevai-runner-auth/1\nrunner-1\nhttps://cp.example.com\noKGio6SlpqeoqaqrrK2ur6ChoqOkpaanqKmqq6ytrq8\n1795000000\nconn-1"
	if string(got) != want {
		t.Fatalf("transcript:\n%q\nwant\n%q", got, want)
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), got)
	const wantSigHex = "191fd23ba444342bbaf72faf98b8004a84cac871187cd715b7907ff87bbdea0ed4a0418e07c0fd6745145de3961ccb9d4d69bd5cd0df97ac3d1053bb7c847f0f"
	if hex.EncodeToString(sig) != wantSigHex {
		t.Fatalf("signature %s", hex.EncodeToString(sig))
	}
}

func TestAuthTranscriptRejectsAmbiguousFields(t *testing.T) {
	t.Parallel()
	ok := AuthFields{RunnerID: "r", Audience: "https://a", Nonce: "n", ExpiresAt: 1, ConnectionID: "c"}
	for name, mut := range map[string]func(*AuthFields){
		"newline in runner":   func(f *AuthFields) { f.RunnerID = "r\nx" },
		"newline in audience": func(f *AuthFields) { f.Audience = "https://a\n" },
		"empty nonce":         func(f *AuthFields) { f.Nonce = "" },
		"empty connection":    func(f *AuthFields) { f.ConnectionID = "" },
		"zero expiry":         func(f *AuthFields) { f.ExpiresAt = 0 },
		"control char":        func(f *AuthFields) { f.ConnectionID = "c\x00" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := ok
			mut(&f)
			if _, err := AuthTranscript(f); !errors.Is(err, ErrAuthField) {
				t.Fatalf("err = %v, want ErrAuthField", err)
			}
		})
	}
	if _, err := AuthTranscript(ok); err != nil {
		t.Fatal(err)
	}
}

func TestAudience(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		secure     bool
		host, want string
	}{
		{true, "CP.Example.com", "https://cp.example.com"},
		{true, "cp.example.com:443", "https://cp.example.com"},
		{true, "cp.example.com:8443", "https://cp.example.com:8443"},
		{false, "127.0.0.1:8080", "http://127.0.0.1:8080"},
		{false, "localhost:80", "http://localhost"},
		{false, "[::1]:8080", "http://[::1]:8080"},
	} {
		if got := Audience(c.secure, c.host); got != c.want {
			t.Errorf("Audience(%v, %q) = %q, want %q", c.secure, c.host, got, c.want)
		}
	}
}
