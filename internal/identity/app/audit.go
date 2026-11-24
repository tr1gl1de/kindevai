package app

import (
	"context"
	"errors"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
)

// AuditLog is the audit context's API as identity needs it. Record must be called with the ctx of the
// transaction that performs the action, so the event commits or rolls back with it.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Audit event kinds recorded by the identity context.
const (
	KindUserCreated         = "user.created"
	KindUserDisabled        = "user.disabled"
	KindUserEnabled         = "user.enabled"
	KindUserPasswordReset   = "user.password_reset"
	KindUserPasswordChanged = "user.password_changed"
	KindInvitationCreated   = "invitation.created"
	KindInvitationRevoked   = "invitation.revoked"
	KindInvitationAccepted  = "invitation.accepted"
)

var errNoAuditLog = errors.New("identity: audit log not configured")

func record(ctx context.Context, log AuditLog, e auditapp.Entry) error {
	if log == nil { // a wiring mistake must not silently drop the record
		return errNoAuditLog
	}
	return log.Record(ctx, e)
}

func userSubject(id string) string       { return "user:" + id }
func invitationSubject(id string) string { return "invitation:" + id }
