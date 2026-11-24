package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEnableRecordsWhoWhenWhy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	m, err := Enable(now, "owner", "  moving to a new VPS \n")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Enabled || m.By != "owner" || !m.Since.Equal(now) || m.Reason != "moving to a new VPS" {
		t.Fatalf("mode %+v", m)
	}
	if off := Disabled(); off.Enabled || off.Reason != "" {
		t.Fatalf("disabled %+v", off)
	}
}

func TestEnableRejectsLongOrControlReason(t *testing.T) {
	t.Parallel()
	for _, r := range []string{strings.Repeat("x", MaxReasonLen+1), "a\x00b"} {
		if _, err := Enable(time.Now(), "o", r); !errors.Is(err, ErrInvalidReason) {
			t.Fatalf("reason %q: %v", r, err)
		}
	}
}

// Read-only: user changes are refused in maintenance mode, except signing in/out, the mode itself, inbound webhooks
// (durable inbox), runner enrollment, a read-only invitation preview and safety actions: cancelling a task (speeds
// up the drain), revoking or disabling a runner, disabling a user, rejecting an approval.
func TestAllowsDuringMaintenance(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/api/auth/login": true, "/api/auth/logout": true, "/api/auth/password": true, "/api/setup": true,
		"/api/maintenance": true, "/api/webhooks/github": true, "/api/webhooks/youtrack": true,
		"/api/telegram/login": true, "/api/runners/enroll": true, "/api/tasks/t1/cancel": true,
		"/api/tasks": false, "/api/projects": false, "/api/tasks/t1/mock-run": false, "/api/users": false,
		"/api/tasks/a/b/cancel":  false,
		"/api/runners/r1/revoke": true, "/api/runners/r1/disable": true, "/api/runners/r1/enable": false,
		"/api/users/u1/disable": true, "/api/users/u1/enable": false, "/api/approvals/a1/reject": true,
		"/api/approvals/a1/approve": false, "/api/invitations/preview": true, "/api/runners//revoke": false,
	} {
		if got := WriteAllowed(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}
