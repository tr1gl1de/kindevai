package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
)

// Invitation errors are specific on purpose: whoever presents the token already holds it, so telling them it was
// used, revoked or expired leaks nothing. An unknown token is ErrInvitationInvalid.
var (
	ErrInvitationInvalid = errors.New("invalid invitation")
	ErrInvitationUsed    = errors.New("invitation already used")
	ErrInvitationRevoked = errors.New("invitation revoked")
	ErrInvitationExpired = errors.New("invitation expired")
)

// Invitation TTL bounds: short-lived by design (KIN-A-21).
const (
	DefaultInvitationTTL = 72 * time.Hour
	MaxInvitationTTL     = 7 * 24 * time.Hour
)

// InvitationState is derived from the timestamps; nothing stores it.
type InvitationState string

const (
	InvitationPending InvitationState = "pending"
	InvitationUsed    InvitationState = "used"
	InvitationRevoked InvitationState = "revoked"
	InvitationExpired InvitationState = "expired"
)

// Invitation is a one-time offer of a project role to someone who does not have an account yet. Only the SHA-256
// of the 256-bit token is stored. ProjectID and Role belong to the projects context and are opaque here.
type Invitation struct {
	ID         string
	TokenHash  []byte
	ProjectID  string
	Role       string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	ConsumedBy string
	RevokedAt  *time.Time
}

// NewInvitationToken returns a random 256-bit token (plaintext, shown once) and its stored hash.
func NewInvitationToken(rnd io.Reader) (string, []byte, error) {
	var b [32]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", nil, fmt.Errorf("generating invitation token: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b[:])
	return plain, HashInvitationToken(plain), nil
}

// HashInvitationToken is the lookup key for a presented token; a fast hash suffices for 256 random bits.
func HashInvitationToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// InvitationTTL turns the requested lifetime in hours into a duration; 0 means the default.
func InvitationTTL(hours int) (time.Duration, error) {
	if hours == 0 {
		return DefaultInvitationTTL, nil
	}
	d := time.Duration(hours) * time.Hour
	if hours < 0 || d > MaxInvitationTTL {
		return 0, &ValidationError{Field: "ttlHours", Code: "ttl_out_of_range"}
	}
	return d, nil
}

// State reports the invitation's state at now. Used and revoked win over expired.
func (i Invitation) State(now time.Time) InvitationState {
	switch {
	case i.ConsumedAt != nil:
		return InvitationUsed
	case i.RevokedAt != nil:
		return InvitationRevoked
	case !now.Before(i.ExpiresAt):
		return InvitationExpired
	}
	return InvitationPending
}

// Usable returns nil for a pending invitation and the matching error otherwise.
func (i Invitation) Usable(now time.Time) error {
	switch i.State(now) {
	case InvitationUsed:
		return ErrInvitationUsed
	case InvitationRevoked:
		return ErrInvitationRevoked
	case InvitationExpired:
		return ErrInvitationExpired
	}
	return nil
}
