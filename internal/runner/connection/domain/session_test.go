package domain

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

func TestBackoffGrowsWithJitterAndCaps(t *testing.T) {
	t.Parallel()
	b := Backoff{Min: time.Second, Max: 8 * time.Second}
	// r=0 gives the lower half, r→1 the upper bound of each step.
	for i, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if got := b.Next(0); got != want {
			t.Fatalf("step %d (r=0) = %v, want %v", i, got, want)
		}
	}
	b.Reset()
	if got := b.Next(0.999999); got < 999*time.Millisecond || got > time.Second {
		t.Fatalf("after reset (r≈1) = %v", got)
	}
	for range 100 { // no overflow after many attempts
		if got := b.Next(0.5); got <= 0 || got > 8*time.Second {
			t.Fatalf("out of range: %v", got)
		}
	}
}

func TestIdentitySigns(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	id, _ := NewIdentity(priv)
	if !ed25519.Verify(id.PublicKey(), []byte("m"), id.Sign([]byte("m"))) {
		t.Fatal("signature does not verify")
	}
}

func TestFatalConnectionErrors(t *testing.T) {
	t.Parallel()
	for err, fatal := range map[error]bool{
		ErrRunnerRevoked: true, ErrUnsupportedProtocol: true, ErrTLSVerification: true, ErrPlainHTTP: true,
		ErrAudienceMismatch: true, ErrNotApproved: false, ErrAuthRejected: false, errors.New("io"): false,
	} {
		if got := Fatal(err); got != fatal {
			t.Errorf("Fatal(%v) = %v, want %v", err, got, fatal)
		}
	}
}
