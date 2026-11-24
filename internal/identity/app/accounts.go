package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

// AccountRepository changes accounts.
type AccountRepository interface {
	UserRepository
	Create(ctx context.Context, u domain.User) error // domain.ErrLoginTaken on conflict
	List(ctx context.Context) ([]domain.User, error)
	SetDisabled(ctx context.Context, id string, disabled bool) error
	SetPassword(ctx context.Context, id, hash string, mustChange bool) error
}

// SessionRevoker ends a user's sessions, optionally keeping the one with token hash keep.
type SessionRevoker interface {
	DeleteByUser(ctx context.Context, userID string, keep []byte) error
}

// PasswordHashVerifier hashes and verifies passwords.
type PasswordHashVerifier interface {
	Hash(ctx context.Context, password string) (string, error)
	Verify(ctx context.Context, encodedHash, password string) (bool, error)
}

// AccountsService implements account management (Owner) and self-service password changes.
type AccountsService struct {
	Accounts AccountRepository
	Sessions SessionRevoker
	Hasher   PasswordHashVerifier
	Rand     io.Reader
	Now      func() time.Time
	NewID    func() string
}

func ownerOnly(p auth.Principal) error {
	if !p.Owner {
		return domain.ErrForbidden
	}
	return nil
}

// CreateUser adds an account with a server-generated temporary password, returned once for the Owner to hand over.
func (s *AccountsService) CreateUser(ctx context.Context, actor auth.Principal, login string) (domain.User, string, error) {
	if err := ownerOnly(actor); err != nil {
		return domain.User{}, "", err
	}
	l, err := domain.NewLogin(login)
	if err != nil {
		return domain.User{}, "", err
	}
	if _, err := s.Accounts.ByLogin(ctx, l); err == nil {
		return domain.User{}, "", domain.ErrLoginTaken
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, "", fmt.Errorf("checking login: %w", err)
	}
	temp, hash, err := s.temporary(ctx)
	if err != nil {
		return domain.User{}, "", err
	}
	u := domain.User{ID: s.NewID(), Login: l, PasswordHash: hash, Role: domain.RoleUser, CreatedAt: s.Now(), MustChangePassword: true}
	if err := s.Accounts.Create(ctx, u); err != nil {
		return domain.User{}, "", err
	}
	return u, temp, nil
}

func (s *AccountsService) temporary(ctx context.Context) (plain, hash string, err error) {
	plain, err = domain.NewTemporaryPassword(s.Rand)
	if err != nil {
		return "", "", err
	}
	hash, err = s.Hasher.Hash(ctx, plain)
	if err != nil {
		return "", "", fmt.Errorf("hashing password: %w", err)
	}
	return plain, hash, nil
}

// ListUsers returns all accounts (Owner only).
func (s *AccountsService) ListUsers(ctx context.Context, actor auth.Principal) ([]domain.User, error) {
	if err := ownerOnly(actor); err != nil {
		return nil, err
	}
	return s.Accounts.List(ctx)
}

// SetDisabled disables or re-enables an account; disabling ends all its sessions. The Owner cannot be disabled.
func (s *AccountsService) SetDisabled(ctx context.Context, actor auth.Principal, userID string, disabled bool) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	u, err := s.Accounts.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Role == domain.RoleOwner {
		return &domain.ValidationError{Field: "user", Code: "owner_cannot_be_disabled"}
	}
	if err := s.Accounts.SetDisabled(ctx, userID, disabled); err != nil {
		return fmt.Errorf("updating account: %w", err)
	}
	if disabled {
		if err := s.Sessions.DeleteByUser(ctx, userID, nil); err != nil {
			return fmt.Errorf("revoking sessions: %w", err)
		}
	}
	return nil
}

// ResetPassword gives an account a new temporary password and ends its sessions.
func (s *AccountsService) ResetPassword(ctx context.Context, actor auth.Principal, userID string) (string, error) {
	if err := ownerOnly(actor); err != nil {
		return "", err
	}
	if _, err := s.Accounts.ByID(ctx, userID); err != nil {
		return "", err
	}
	temp, hash, err := s.temporary(ctx)
	if err != nil {
		return "", err
	}
	if err := s.Accounts.SetPassword(ctx, userID, hash, true); err != nil {
		return "", fmt.Errorf("updating password: %w", err)
	}
	if err := s.Sessions.DeleteByUser(ctx, userID, nil); err != nil {
		return "", fmt.Errorf("revoking sessions: %w", err)
	}
	return temp, nil
}

// ChangePassword lets a user set their own password after proving the current one. Other sessions end; the
// session identified by keepSession (the caller's) stays signed in.
func (s *AccountsService) ChangePassword(ctx context.Context, actor auth.Principal, current, next string, keepSession []byte) error {
	u, err := s.Accounts.ByID(ctx, actor.UserID)
	if err != nil {
		return err
	}
	ok, err := s.Hasher.Verify(ctx, u.PasswordHash, current)
	if err != nil {
		return fmt.Errorf("verifying password: %w", err)
	}
	if !ok {
		return domain.ErrInvalidCredentials
	}
	if err := domain.ValidatePassword(next, u.Login); err != nil {
		return err
	}
	if next == current {
		return &domain.ValidationError{Field: "newPassword", Code: "password_reused"}
	}
	hash, err := s.Hasher.Hash(ctx, next)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	if err := s.Accounts.SetPassword(ctx, u.ID, hash, false); err != nil {
		return fmt.Errorf("updating password: %w", err)
	}
	if err := s.Sessions.DeleteByUser(ctx, u.ID, keepSession); err != nil {
		return fmt.Errorf("revoking other sessions: %w", err)
	}
	return nil
}
