package domain

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"time"
)

// Gateway errors (KIN-19). An unknown runner and a bad signature are the same error, so a guesser learns nothing.
var (
	ErrBadSignature     = errors.New("runner authentication failed")
	ErrChallengeExpired = errors.New("challenge expired")
	ErrChallengeUsed    = errors.New("challenge already used")
	ErrNotApproved      = errors.New("runner is not approved")
	ErrRunnerRevoked    = errors.New("runner is revoked")
)

// ChallengeTTL is how long a runner has to answer the challenge of its connection.
const ChallengeTTL = 30 * time.Second

// Challenge is the one-time nonce of one WebSocket connection. It is bound to the connection id and the audience
// (the Control Plane origin), so a signature is worthless on any other connection or server.
type Challenge struct {
	Nonce        []byte
	ConnectionID string
	Audience     string
	ExpiresAt    time.Time
	used         bool
}

// NewChallenge draws a 32-byte nonce valid for ChallengeTTL.
func NewChallenge(rnd io.Reader, connID, audience string, now time.Time) (*Challenge, error) {
	n := make([]byte, 32)
	if _, err := io.ReadFull(rnd, n); err != nil {
		return nil, fmt.Errorf("generating challenge nonce: %w", err)
	}
	return &Challenge{Nonce: n, ConnectionID: connID, Audience: audience, ExpiresAt: now.Add(ChallengeTTL)}, nil
}

// Use spends the challenge. Any second use fails, also after a failed first one, so it can never be replayed.
func (c *Challenge) Use(now time.Time) error {
	if c.used {
		return ErrChallengeUsed
	}
	c.used = true
	if !now.Before(c.ExpiresAt) {
		return ErrChallengeExpired
	}
	return nil
}

// Verify checks an Ed25519 signature made with the runner's private key.
func (r Runner) Verify(msg, sig []byte) bool {
	return len(r.PublicKey) == ed25519.PublicKeySize && ed25519.Verify(ed25519.PublicKey(r.PublicKey), msg, sig)
}

// CheckConnect: only an approved runner may hold a connection; revoked is final, pending/disabled may retry later.
func (r Runner) CheckConnect() error {
	switch {
	case r.MayConnect():
		return nil
	case r.Status == StatusRevoked:
		return ErrRunnerRevoked
	}
	return ErrNotApproved
}
