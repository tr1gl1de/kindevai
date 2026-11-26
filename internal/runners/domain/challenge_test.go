package domain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

func TestChallengeIsRandomSingleUseAndExpires(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	a, err := NewChallenge(rand.Reader, "c1", "https://cp", now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewChallenge(rand.Reader, "c2", "https://cp", now)
	if len(a.Nonce) != 32 || bytes.Equal(a.Nonce, b.Nonce) {
		t.Fatalf("nonces must be 32 random bytes: %x %x", a.Nonce, b.Nonce)
	}
	if !a.ExpiresAt.Equal(now.Add(ChallengeTTL)) {
		t.Fatalf("expiresAt = %v", a.ExpiresAt)
	}
	if err := a.Use(now.Add(ChallengeTTL - time.Second)); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := a.Use(now); !errors.Is(err, ErrChallengeUsed) {
		t.Fatalf("second use = %v, want ErrChallengeUsed", err)
	}
	if err := b.Use(now.Add(ChallengeTTL)); !errors.Is(err, ErrChallengeExpired) {
		t.Fatalf("expired use = %v, want ErrChallengeExpired", err)
	}
	if err := b.Use(now); !errors.Is(err, ErrChallengeUsed) {
		t.Fatalf("a challenge is spent even by a failed use, got %v", err)
	}
}

func TestChallengeNeedsRandomness(t *testing.T) {
	t.Parallel()
	if _, err := NewChallenge(bytes.NewReader(nil), "c", "a", time.Now()); err == nil {
		t.Fatal("want an error without randomness")
	}
}

func TestRunnerVerifiesItsSignature(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	r := Runner{PublicKey: PublicKey(pub)}
	msg := []byte("transcript")
	if !r.Verify(msg, ed25519.Sign(priv, msg)) {
		t.Fatal("own signature rejected")
	}
	if r.Verify(msg, ed25519.Sign(other, msg)) {
		t.Fatal("impostor signature accepted")
	}
	if r.Verify([]byte("other"), ed25519.Sign(priv, msg)) {
		t.Fatal("signature over another message accepted")
	}
	if (Runner{PublicKey: PublicKey{1, 2}}).Verify(msg, ed25519.Sign(priv, msg)) {
		t.Fatal("malformed key accepted")
	}
}

func TestConnectDecision(t *testing.T) {
	t.Parallel()
	for status, want := range map[Status]error{
		StatusApproved: nil, StatusPending: ErrNotApproved, StatusDisabled: ErrNotApproved, StatusRevoked: ErrRunnerRevoked,
	} {
		if err := (Runner{Status: status}).CheckConnect(); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("%s: CheckConnect = %v, want %v", status, err, want)
		}
	}
}
