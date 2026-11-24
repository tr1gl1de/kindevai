// Package app holds the identity use cases. It orchestrates the domain through ports implemented by adapters.
package app

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

// SetupRepository runs fn inside one serialised write transaction; an error rolls everything back.
type SetupRepository interface {
	InTx(ctx context.Context, fn func(SetupTx) error) error
}

// SetupTx is the transactional view of the setup state.
type SetupTx interface {
	SetupState(ctx context.Context) (domain.SetupState, error)
	SaveSetupToken(ctx context.Context, tok domain.SetupToken) error
	// CompleteSetup stores the owner and platform name, marks setup completed and deletes the token.
	CompleteSetup(ctx context.Context, owner domain.User, name domain.PlatformName, at time.Time) error
}

// PasswordHasher turns a password into an encoded slow hash.
type PasswordHasher interface {
	Hash(ctx context.Context, password string) (string, error)
}

// TokenChannel hands the setup token to the operator out of band (never over the HTTP API or logs).
type TokenChannel interface {
	Publish(plain string) error
	Clear() error
}

// SetupService implements the first-run setup: one-time token and atomic creation of the first Owner.
type SetupService struct {
	Repo    SetupRepository
	Hasher  PasswordHasher
	Channel TokenChannel
	Now     func() time.Time
	Rand    io.Reader
	NewID   func() string
}

// CompleteSetupInput is the First-run Wizard form.
type CompleteSetupInput struct {
	Token        string
	Login        string
	Password     string
	PlatformName string
}

// Prepare runs at start-up: in setup mode it issues a fresh token; otherwise it removes any leftover token.
func (s *SetupService) Prepare(ctx context.Context) (setupRequired bool, err error) {
	var plain string
	err = s.Repo.InTx(ctx, func(tx SetupTx) error {
		st, err := tx.SetupState(ctx)
		if err != nil {
			return err
		}
		if st.Completed {
			return nil
		}
		setupRequired = true
		plain, err = s.issue(ctx, tx)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("preparing setup: %w", err)
	}
	if !setupRequired {
		return false, s.clear()
	}
	return true, s.publish(plain)
}

// Status reports whether setup is required, reissuing the token if the current one has expired.
func (s *SetupService) Status(ctx context.Context) (setupRequired bool, err error) {
	var plain string
	err = s.Repo.InTx(ctx, func(tx SetupTx) error {
		st, err := tx.SetupState(ctx)
		if err != nil {
			return err
		}
		if st.Completed {
			return nil
		}
		setupRequired = true
		if st.Token != nil && !st.Token.Expired(s.Now()) {
			return nil
		}
		plain, err = s.issue(ctx, tx)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("reading setup status: %w", err)
	}
	if plain != "" {
		return true, s.publish(plain)
	}
	return setupRequired, nil
}

func (s *SetupService) issue(ctx context.Context, tx SetupTx) (string, error) {
	plain, tok, err := domain.NewSetupToken(s.Rand, s.Now(), domain.SetupTokenTTL)
	if err != nil {
		return "", err
	}
	return plain, tx.SaveSetupToken(ctx, tok)
}

func (s *SetupService) publish(plain string) error {
	if err := s.Channel.Publish(plain); err != nil {
		return fmt.Errorf("publishing setup token: %w", err)
	}
	return nil
}

func (s *SetupService) clear() error {
	if err := s.Channel.Clear(); err != nil {
		return fmt.Errorf("removing setup token: %w", err)
	}
	return nil
}

// Complete creates the first Owner. The token is checked before the (expensive) password hash and again,
// together with the write, inside one transaction, so concurrent or repeated requests cannot both succeed.
func (s *SetupService) Complete(ctx context.Context, in CompleteSetupInput) (domain.User, error) {
	if err := s.verify(ctx, in.Token); err != nil {
		return domain.User{}, err
	}
	login, err := domain.NewLogin(in.Login)
	if err != nil {
		return domain.User{}, err
	}
	if err := domain.ValidatePassword(in.Password, login); err != nil {
		return domain.User{}, err
	}
	name, err := domain.NewPlatformName(in.PlatformName)
	if err != nil {
		return domain.User{}, err
	}
	hash, err := s.Hasher.Hash(ctx, in.Password)
	if err != nil {
		return domain.User{}, fmt.Errorf("hashing password: %w", err)
	}
	now := s.Now()
	owner := domain.User{ID: s.NewID(), Login: login, PasswordHash: hash, Role: domain.RoleOwner, CreatedAt: now}
	err = s.Repo.InTx(ctx, func(tx SetupTx) error {
		st, err := tx.SetupState(ctx)
		if err != nil {
			return err
		}
		if err := st.Verify(in.Token, now); err != nil {
			return err
		}
		return tx.CompleteSetup(ctx, owner, name, now)
	})
	if err != nil {
		return domain.User{}, err
	}
	// Setup is done; a leftover token file holds a token the database no longer accepts, and Prepare
	// removes it on the next start, so a failure here must not turn a completed setup into an error.
	_ = s.clear()
	return owner, nil
}

func (s *SetupService) verify(ctx context.Context, token string) error {
	return s.Repo.InTx(ctx, func(tx SetupTx) error {
		st, err := tx.SetupState(ctx)
		if err != nil {
			return err
		}
		return st.Verify(token, s.Now())
	})
}
