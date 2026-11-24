// Package password hashes user passwords with Argon2id (PHC string format) under a concurrency limit,
// so a burst of logins cannot exhaust the 1 GB Control Plane's memory.
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters.
type Params struct {
	MemoryKiB     uint32
	Iterations    uint32
	Parallelism   uint8
	MaxConcurrent int // simultaneous hash computations
}

// DefaultParams follow the OWASP minimum for Argon2id (19 MiB, t=2, p=1). With two concurrent hashes the
// peak is ~40 MiB.
var DefaultParams = Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, MaxConcurrent: 2}

// maxMemoryKiB bounds parameters read from stored hashes (defends against a tampered row exhausting memory).
const maxMemoryKiB = 256 * 1024

const (
	saltLen = 16
	keyLen  = 32
)

// ErrMalformedHash is returned for strings that are not Argon2id PHC hashes this package accepts.
var ErrMalformedHash = errors.New("malformed password hash")

// Hasher hashes and verifies passwords.
type Hasher struct {
	p    Params
	sem  chan struct{}
	cur  atomic.Int32
	peak atomic.Int32
}

// New returns a Hasher with params p.
func New(p Params) *Hasher {
	if p.MaxConcurrent < 1 {
		p.MaxConcurrent = 1
	}
	return &Hasher{p: p, sem: make(chan struct{}, p.MaxConcurrent)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("waiting for password hashing slot: %w", ctx.Err())
	}
	n := h.cur.Add(1)
	for {
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return nil
}

func (h *Hasher) release() {
	h.cur.Add(-1)
	<-h.sem
}

// Hash returns the PHC-encoded Argon2id hash of pw with a random salt.
func (h *Hasher) Hash(ctx context.Context, pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, h.p.Iterations, h.p.MemoryKiB, h.p.Parallelism, keyLen)
	h.release()
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, h.p.MemoryKiB, h.p.Iterations,
		h.p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether pw matches the encoded hash, using the parameters stored in the hash.
func (h *Hasher) Verify(ctx context.Context, encoded, pw string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, ErrMalformedHash
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrMalformedHash
	}
	if m == 0 || m > maxMemoryKiB || t == 0 || t > 16 || p == 0 {
		return false, ErrMalformedHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrMalformedHash
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	h.release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// DummyVerify burns the same work as a real verification; use it when the user does not exist so response
// time does not reveal which logins are valid.
func (h *Hasher) DummyVerify(ctx context.Context, pw string) {
	salt := make([]byte, saltLen)
	if h.acquire(ctx) != nil {
		return
	}
	_ = argon2.IDKey([]byte(pw), salt, h.p.Iterations, h.p.MemoryKiB, h.p.Parallelism, keyLen)
	h.release()
}
