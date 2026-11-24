package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// SessionIdleTTL ends a session after this long without requests.
	SessionIdleTTL = 12 * time.Hour
	// SessionMaxAge ends a session this long after login, however active.
	SessionMaxAge = 7 * 24 * time.Hour
	// sessionTouchEvery throttles last-seen writes.
	sessionTouchEvery = time.Minute
)

// ErrInvalidCredentials covers unknown login, wrong password and disabled account alike.
var ErrInvalidCredentials = errors.New("invalid login or password")

// ErrUnauthenticated means the request has no valid session.
var ErrUnauthenticated = errors.New("not authenticated")

// Session is a server-side login session. Only the hash of its bearer token is stored.
type Session struct {
	TokenHash  []byte
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// NewSession creates a session for userID and returns its plaintext token (for the cookie) and stored form.
func NewSession(rnd io.Reader, userID string, now time.Time) (string, Session, error) {
	var b [32]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", Session{}, fmt.Errorf("generating session token: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b[:])
	return plain, Session{TokenHash: HashSessionToken(plain), UserID: userID, CreatedAt: now, LastSeenAt: now}, nil
}

// HashSessionToken is the lookup key for a presented token.
func HashSessionToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// Valid reports whether the session may be used at now (idle and absolute limits).
func (s Session) Valid(now time.Time) bool {
	return now.Before(s.LastSeenAt.Add(SessionIdleTTL)) && now.Before(s.CreatedAt.Add(SessionMaxAge))
}

// NeedsTouch reports whether LastSeenAt is stale enough to be worth a write.
func (s Session) NeedsTouch(now time.Time) bool { return now.Sub(s.LastSeenAt) >= sessionTouchEvery }

// ExpiresAt is when the session ends if it is not used after lastSeen.
func (s Session) ExpiresAt(lastSeen time.Time) time.Time {
	idle := lastSeen.Add(SessionIdleTTL)
	if abs := s.CreatedAt.Add(SessionMaxAge); abs.Before(idle) {
		return abs
	}
	return idle
}
