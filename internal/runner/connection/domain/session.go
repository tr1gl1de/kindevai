package domain

import (
	"crypto/ed25519"
	"errors"
	"time"
)

// Connection errors (KIN-19). Fatal ones stop the daemon: retrying cannot fix them.
var (
	ErrRunnerRevoked       = errors.New("the Control Plane revoked this runner; enroll again with a new key")
	ErrUnsupportedProtocol = errors.New("the Control Plane speaks another runner protocol version; upgrade ai-runner")
	ErrTLSVerification     = errors.New("TLS certificate verification failed; the connection is not trusted")
	ErrAudienceMismatch    = errors.New("the challenge was issued for another Control Plane address; refusing to sign it")
	ErrNotApproved         = errors.New("runner is not approved (pending or disabled); waiting for the Owner")
	ErrAuthRejected        = errors.New("the Control Plane rejected the runner's authentication")
)

// Fatal reports whether a connection error must stop the daemon instead of reconnecting.
func Fatal(err error) bool {
	for _, target := range []error{ErrRunnerRevoked, ErrUnsupportedProtocol, ErrTLSVerification, ErrPlainHTTP, ErrAudienceMismatch} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// Sign signs msg with the runner's private key (the challenge-response transcript).
func (i Identity) Sign(msg []byte) []byte { return ed25519.Sign(i.key, msg) }

// Host is the host[:port] of the Control Plane URL, as sent in the HTTP Host header.
func (s ServerURL) Host() string { return s.u.Host }

// Backoff is exponential reconnect backoff with "equal jitter": step n waits between half and all of
// min(Max, Min·2ⁿ), so many runners reconnecting after an outage spread out instead of arriving together.
type Backoff struct {
	Min, Max time.Duration
	attempt  int
}

// Next returns the next delay; r is a uniform random number in [0, 1).
func (b *Backoff) Next(r float64) time.Duration {
	d := b.Max
	if b.attempt < 32 && b.Min<<b.attempt < b.Max && b.Min<<b.attempt > 0 {
		d = b.Min << b.attempt
	}
	b.attempt++
	half := d / 2
	return half + time.Duration(float64(d-half)*r)
}

// Reset starts over after a connection that stayed up.
func (b *Backoff) Reset() { b.attempt = 0 }
