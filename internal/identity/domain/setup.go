package domain

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrSetupCompleted: the first Owner exists; setup can never run again.
	ErrSetupCompleted = errors.New("setup is already completed")
	// ErrInvalidSetupToken covers wrong, used, expired and missing tokens alike, so callers learn nothing.
	ErrInvalidSetupToken = errors.New("invalid or expired setup token")
)

// SetupTokenTTL is how long a setup token stays valid.
const SetupTokenTTL = time.Hour

// SetupToken is the stored form of a one-time setup token: only its SHA-256 hash is kept.
// The token is 256 random bits, so a fast hash is sufficient.
type SetupToken struct {
	Hash      []byte
	ExpiresAt time.Time
}

// NewSetupToken draws a token from rnd. It returns the plaintext (to hand to the operator once) and the stored form.
func NewSetupToken(rnd io.Reader, now time.Time, ttl time.Duration) (string, SetupToken, error) {
	var b [32]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", SetupToken{}, fmt.Errorf("generating setup token: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b[:])
	return plain, SetupToken{Hash: hashToken(plain), ExpiresAt: now.Add(ttl)}, nil
}

func hashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// Expired reports whether the token is no longer usable at now.
func (t SetupToken) Expired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// Verify checks plain against the stored hash in constant time and rejects expired tokens.
func (t SetupToken) Verify(plain string, now time.Time) error {
	if plain == "" || t.Expired(now) || subtle.ConstantTimeCompare(hashToken(plain), t.Hash) != 1 {
		return ErrInvalidSetupToken
	}
	return nil
}

// SetupState is the installation's first-run state.
type SetupState struct {
	Completed bool
	Token     *SetupToken // current token, nil if none issued
}

// Verify decides whether plain may complete setup now.
func (s SetupState) Verify(plain string, now time.Time) error {
	if s.Completed {
		return ErrSetupCompleted
	}
	if s.Token == nil {
		return ErrInvalidSetupToken
	}
	return s.Token.Verify(plain, now)
}
