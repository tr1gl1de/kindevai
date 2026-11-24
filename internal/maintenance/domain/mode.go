// Package domain is the maintenance context (KIN-41): the platform-wide maintenance mode the Owner switches on
// before moving or backing up an installation. While it is on the scheduler hands out no new steps, running
// attempts drain, runners keep reconciling and user changes are refused (read-only UI).
package domain

import (
	"errors"
	"strings"
	"time"
	"unicode"
)

// MaxReasonLen bounds the reason shown in the banner.
const MaxReasonLen = 500

// ErrInvalidReason: the reason is too long or has control characters.
var ErrInvalidReason = errors.New("maintenance reason must be at most 500 characters without control characters")

// ErrForbidden: only the platform Owner switches maintenance mode.
var ErrForbidden = errors.New("only the Owner can switch maintenance mode")

// Mode is the maintenance state.
type Mode struct {
	Enabled bool
	Reason  string
	Since   time.Time
	By      string // login of the Owner who switched it on
}

// Enable returns the mode switched on at now by login with reason (trimmed).
func Enable(now time.Time, login, reason string) (Mode, error) {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > MaxReasonLen || strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		return Mode{}, ErrInvalidReason
	}
	return Mode{Enabled: true, Reason: reason, Since: now.UTC(), By: login}, nil
}

// Disabled is the normal operating mode.
func Disabled() Mode { return Mode{} }

// writeAllowed are API paths (or prefixes ending in "/") that still accept changes in maintenance mode.
var writeAllowed = []string{"/api/auth/", "/api/setup", "/api/maintenance", "/api/webhooks/", "/api/telegram/login",
	"/api/runners/enroll", "/api/invitations/preview"}

// safetyActions are "/api/<collection>/{id}/<action>" changes kept in maintenance mode: they stop or reduce work
// (and answer a security incident), never start new work.
var safetyActions = map[string][]string{
	"tasks":     {"cancel"},
	"runners":   {"revoke", "disable"},
	"users":     {"disable"},
	"approvals": {"reject"},
}

// WriteAllowed reports whether a change request to path is accepted while maintenance mode is on: signing in and
// out, the mode itself, inbound webhooks (they only fill the durable inbox), runner enrollment, the read-only
// invitation preview and the safety actions (cancel a task, revoke or disable a runner, disable a user, reject an
// approval).
func WriteAllowed(path string) bool {
	for _, p := range writeAllowed {
		if path == p || strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/"), "/")
	if !strings.HasPrefix(path, "/api/") || len(parts) != 3 || parts[1] == "" {
		return false
	}
	for _, a := range safetyActions[parts[0]] {
		if parts[2] == a {
			return true
		}
	}
	return false
}
