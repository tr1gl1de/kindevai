// Package auth is the shared-kernel view of "who is calling": the identity context authenticates requests
// and stores a Principal in the context; other contexts read it without depending on identity's adapters.
package auth

import "context"

// Principal is the authenticated user of a request.
type Principal struct {
	UserID string
	Login  string
	Owner  bool // platform Owner
	// MustChangePassword: signed in with a temporary password; only the password change is allowed.
	MustChangePassword bool
}

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the request's principal, if authenticated.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
