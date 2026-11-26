package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

// UserRepository reads accounts.
type UserRepository interface {
	ByLogin(ctx context.Context, login domain.Login) (domain.User, error)
	ByID(ctx context.Context, id string) (domain.User, error)
}

// SessionRepository stores sessions by token hash.
type SessionRepository interface {
	Create(ctx context.Context, s domain.Session) error
	ByTokenHash(ctx context.Context, hash []byte) (domain.Session, error)
	// Touch records activity at at; ErrNotFound if the session no longer exists.
	Touch(ctx context.Context, s domain.Session, at time.Time) error
	Delete(ctx context.Context, hash []byte) error
	DeleteExpired(ctx context.Context, now time.Time) error
}

// PasswordVerifier checks passwords against stored hashes.
type PasswordVerifier interface {
	Verify(ctx context.Context, encodedHash, password string) (bool, error)
	// DummyVerify spends the cost of a verification without a user, to equalise timing.
	DummyVerify(ctx context.Context, password string)
}

// AuthService implements login, session authentication and logout. Sign-ins, failed sign-ins and logouts are
// audited; a session is created or deleted in one transaction with its event.
type AuthService struct {
	Users     UserRepository
	Sessions  SessionRepository
	Passwords PasswordVerifier
	Tx        TxRunner
	Audit     AuditLog
	Now       func() time.Time
	Rand      io.Reader
	Logger    *slog.Logger // optional
}

// Login reasons recorded with auth.login_failed.
const (
	failUnknownLogin  = "unknown_login"
	failWrongPassword = "wrong_password"
	failDisabled      = "disabled"
)

// Login checks credentials and opens a session; client is the caller's address for the audit log. Every failure
// is domain.ErrInvalidCredentials and costs one password hash, so neither the answer nor its timing tells which
// part was wrong. Failures are audited (best-effort) without the password, and with the login only when that account exists:
// an unknown "login" may be a password typed into the wrong field.
func (s *AuthService) Login(ctx context.Context, login, password, client string) (string, domain.User, error) {
	l, err := domain.NewLogin(login)
	if err != nil {
		s.Passwords.DummyVerify(ctx, password)
		return "", domain.User{}, s.loginFailed(ctx, domain.User{}, failUnknownLogin, client)
	}
	u, err := s.Users.ByLogin(ctx, l)
	if errors.Is(err, domain.ErrNotFound) {
		s.Passwords.DummyVerify(ctx, password)
		return "", domain.User{}, s.loginFailed(ctx, domain.User{}, failUnknownLogin, client)
	}
	if err != nil {
		return "", domain.User{}, fmt.Errorf("loading user: %w", err)
	}
	ok, err := s.Passwords.Verify(ctx, u.PasswordHash, password)
	if err != nil {
		return "", domain.User{}, fmt.Errorf("verifying password: %w", err)
	}
	if !ok {
		return "", domain.User{}, s.loginFailed(ctx, u, failWrongPassword, client)
	}
	if u.Disabled {
		return "", domain.User{}, s.loginFailed(ctx, u, failDisabled, client)
	}
	now := s.Now()
	// Housekeeping must not block a valid login.
	if err := s.Sessions.DeleteExpired(ctx, now); err != nil {
		s.logger().Warn("purging expired sessions", "err", err)
	}
	plain, sess, err := domain.NewSession(s.Rand, u.ID, now)
	if err != nil {
		return "", domain.User{}, err
	}
	err = s.inTx(ctx, func(ctx context.Context) error {
		if err := s.Sessions.Create(ctx, sess); err != nil {
			return fmt.Errorf("creating session: %w", err)
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.User(u.ID, string(u.Login)), Kind: KindLoginSucceeded,
			Subject: userSubject(u.ID), Details: map[string]string{"ip": client}})
	})
	if err != nil {
		return "", domain.User{}, err
	}
	return plain, u, nil
}

// loginFailed records a failed attempt and returns domain.ErrInvalidCredentials. Recording is best-effort: a
// failure is logged, never turned into another answer, so the reply stays a uniform 401 and the attempt still
// counts against the rate limits. u is the zero User for an unknown login.
func (s *AuthService) loginFailed(ctx context.Context, u domain.User, reason, client string) error {
	e := auditapp.Entry{Actor: auditapp.Anonymous(), Kind: KindLoginFailed, Details: map[string]string{"ip": client, "reason": reason}}
	if u.ID != "" {
		e.Subject, e.Details["login"] = userSubject(u.ID), string(u.Login)
	}
	if err := record(ctx, s.Audit, e); err != nil {
		s.logger().Error("recording failed login", "err", err)
	}
	return domain.ErrInvalidCredentials
}

func (s *AuthService) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.Tx == nil {
		return errNoAuditLog
	}
	return s.Tx.InTx(ctx, fn)
}

// Authenticate resolves a session token to its active user.
func (s *AuthService) Authenticate(ctx context.Context, token string) (domain.User, error) {
	if token == "" {
		return domain.User{}, domain.ErrUnauthenticated
	}
	hash := domain.HashSessionToken(token)
	sess, err := s.Sessions.ByTokenHash(ctx, hash)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("loading session: %w", err)
	}
	now := s.Now()
	if !sess.Valid(now) {
		if err := s.Sessions.Delete(ctx, hash); err != nil {
			return domain.User{}, fmt.Errorf("deleting expired session: %w", err)
		}
		return domain.User{}, domain.ErrUnauthenticated
	}
	u, err := s.Users.ByID(ctx, sess.UserID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, domain.ErrUnauthenticated
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("loading user: %w", err)
	}
	if u.Disabled {
		return domain.User{}, domain.ErrUnauthenticated
	}
	if sess.NeedsTouch(now) {
		err := s.Sessions.Touch(ctx, sess, now)
		if errors.Is(err, domain.ErrNotFound) { // logged out concurrently
			return domain.User{}, domain.ErrUnauthenticated
		}
		if err != nil {
			return domain.User{}, fmt.Errorf("touching session: %w", err)
		}
	}
	return u, nil
}

func (s *AuthService) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// Logout revokes the session and records auth.logout in the same transaction; an unknown token is not an error
// and records nothing.
func (s *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	hash := domain.HashSessionToken(token)
	return s.inTx(ctx, func(ctx context.Context) error {
		sess, err := s.Sessions.ByTokenHash(ctx, hash)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loading session: %w", err)
		}
		u, err := s.Users.ByID(ctx, sess.UserID)
		if err != nil {
			return fmt.Errorf("loading user: %w", err)
		}
		if err := s.Sessions.Delete(ctx, hash); err != nil {
			return fmt.Errorf("deleting session: %w", err)
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.User(u.ID, string(u.Login)), Kind: KindLogout,
			Subject: userSubject(u.ID)})
	})
}

// Revoke deletes a session without an audit event: used when a new sign-in replaces the browser's previous
// session, which the auth.login_succeeded event already accounts for. An unknown token is not an error.
func (s *AuthService) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.Sessions.Delete(ctx, domain.HashSessionToken(token)); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}
