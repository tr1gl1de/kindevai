package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

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

// AuthService implements login, session authentication and logout.
type AuthService struct {
	Users     UserRepository
	Sessions  SessionRepository
	Passwords PasswordVerifier
	Now       func() time.Time
	Rand      io.Reader
	Logger    *slog.Logger // optional
}

// Login checks credentials and opens a session. Every failure is domain.ErrInvalidCredentials and costs one
// password hash, so neither the answer nor its timing tells which part was wrong.
func (s *AuthService) Login(ctx context.Context, login, password string) (string, domain.User, error) {
	l, err := domain.NewLogin(login)
	if err != nil {
		s.Passwords.DummyVerify(ctx, password)
		return "", domain.User{}, domain.ErrInvalidCredentials
	}
	u, err := s.Users.ByLogin(ctx, l)
	if errors.Is(err, domain.ErrNotFound) {
		s.Passwords.DummyVerify(ctx, password)
		return "", domain.User{}, domain.ErrInvalidCredentials
	}
	if err != nil {
		return "", domain.User{}, fmt.Errorf("loading user: %w", err)
	}
	ok, err := s.Passwords.Verify(ctx, u.PasswordHash, password)
	if err != nil {
		return "", domain.User{}, fmt.Errorf("verifying password: %w", err)
	}
	if !ok || u.Disabled {
		return "", domain.User{}, domain.ErrInvalidCredentials
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
	if err := s.Sessions.Create(ctx, sess); err != nil {
		return "", domain.User{}, fmt.Errorf("creating session: %w", err)
	}
	return plain, u, nil
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

// Logout revokes the session; an unknown token is not an error.
func (s *AuthService) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.Sessions.Delete(ctx, domain.HashSessionToken(token)); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}
