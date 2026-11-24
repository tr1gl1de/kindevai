package password

import (
	"strings"
	"sync"
	"testing"
)

func testHasher() *Hasher {
	return New(Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 2})
}

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	h := testHasher()
	enc, err := h.Hash(t.Context(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=64,t=1,p=1$") || strings.Contains(enc, "correct horse") {
		t.Fatalf("encoded = %q", enc)
	}
	enc2, _ := h.Hash(t.Context(), "correct horse battery")
	if enc == enc2 {
		t.Fatal("same salt used twice")
	}
	tests := []struct {
		pw   string
		want bool
	}{{"correct horse battery", true}, {"correct horse batterY", false}, {"", false}}
	for _, tt := range tests {
		ok, err := h.Verify(t.Context(), enc, tt.pw)
		if err != nil || ok != tt.want {
			t.Fatalf("Verify(%q) = %v, %v", tt.pw, ok, err)
		}
	}
}

func TestVerifyUsesParamsFromHash(t *testing.T) {
	t.Parallel()
	old := New(Params{MemoryKiB: 32, Iterations: 2, Parallelism: 1, MaxConcurrent: 1})
	enc, _ := old.Hash(t.Context(), "password-password")
	if ok, err := testHasher().Verify(t.Context(), enc, "password-password"); err != nil || !ok {
		t.Fatalf("Verify with different current params = %v, %v", ok, err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, enc := range []string{"", "plain", "$argon2i$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=999999999,t=1,p=1$c2FsdA$aGFzaA"} {
		if _, err := testHasher().Verify(t.Context(), enc, "x"); err == nil {
			t.Fatalf("Verify(%q) accepted a malformed or oversized hash", enc)
		}
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()
	h := testHasher()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := h.Hash(t.Context(), "parallel-password"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if h.peak.Load() > 2 {
		t.Fatalf("peak concurrent hashes = %d, want <= 2", h.peak.Load())
	}
}

func TestHashHonoursCancelledContext(t *testing.T) {
	t.Parallel()
	h := New(Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 1})
	h.sem <- struct{}{} // occupy the only slot
	ctx, cancel := contextWithCancel(t)
	cancel()
	if _, err := h.Hash(ctx, "blocked-password"); err == nil {
		t.Fatal("want context error while waiting for a slot")
	}
}
