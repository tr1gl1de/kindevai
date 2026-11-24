package domain

import (
	"errors"
	"fmt"
	"io"
)

var (
	// ErrLoginTaken: another account already uses the login.
	ErrLoginTaken = errors.New("login is already taken")
	// ErrForbidden: the actor may not manage accounts.
	ErrForbidden = errors.New("forbidden")
	// ErrPasswordChangeRequired: the account must set its own password before doing anything else.
	ErrPasswordChangeRequired = errors.New("password change required")
)

// temporaryAlphabet avoids look-alike characters (0/O, 1/l/I) because the Owner passes the password on by hand.
const temporaryAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// NewTemporaryPassword returns a random 16-character password (~93 bits) for a new or reset account.
func NewTemporaryPassword(rnd io.Reader) (string, error) {
	const n = 16
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	limit := byte(256 - 256%len(temporaryAlphabet)) // rejection sampling: no modulo bias
	for len(out) < n {
		if _, err := io.ReadFull(rnd, buf); err != nil {
			return "", fmt.Errorf("generating temporary password: %w", err)
		}
		if buf[0] >= limit {
			continue
		}
		out = append(out, temporaryAlphabet[int(buf[0])%len(temporaryAlphabet)])
	}
	return string(out), nil
}
