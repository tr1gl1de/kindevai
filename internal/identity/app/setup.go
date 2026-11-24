// Package app holds the identity use cases. It orchestrates the domain through ports implemented by adapters.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

// SetupRepository runs fn inside one serialised write transaction (an error rolls everything back; fn's ctx
// carries the transaction, so the audit log joins it) and reads the state without taking the write lock.
type SetupRepository interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx SetupTx) error) error
	State(ctx context.Context) (domain.SetupState, error)
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
	Audit   AuditLog
	Now     func() time.Time
	Rand    io.Reader
	NewID   func() string
	Logger  *slog.Logger // optional
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
	err = s.Repo.InTx(ctx, func(ctx context.Context, tx SetupTx) error {
		st, err := tx.SetupState(ctx)
		if err != nil {
			return err
		}
		if st.Completed {
			return nil
		}
		setupRequired = true
		return s.issue(ctx, tx)
	})
	if err != nil {
		return false, fmt.Errorf("preparing setup: %w", err)
	}
	if !setupRequired {
		// Only a stale, already-invalid token can be left; failing to delete it must not block start-up.
		if err := s.clear(); err != nil {
			s.logger().Warn("could not remove leftover setup token file", "err", err)
		}
	}
	return setupRequired, nil
}

// Status reports whether setup is required, reissuing the token if the current one has expired. The common
// path only reads, so polling it cannot hold the database's single writer.
func (s *SetupService) Status(ctx context.Context) (setupRequired bool, err error) {
	st, err := s.Repo.State(ctx)
	if err != nil {
		return false, fmt.Errorf("reading setup status: %w", err)
	}
	if st.Completed {
		return false, nil
	}
	if st.Token != nil && !st.Token.Expired(s.Now()) {
		return true, nil
	}
	err = s.Repo.InTx(ctx, func(ctx context.Context, tx SetupTx) error {
		st, err := tx.SetupState(ctx) // re-check under the write lock
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
		return s.issue(ctx, tx)
	})
	if err != nil {
		return false, fmt.Errorf("reading setup status: %w", err)
	}
	return setupRequired, nil
}

// issue stores a new token and delivers it inside the same transaction: if delivery fails the new token is
// rolled back, so the next call issues another instead of trusting one the operator never received.
func (s *SetupService) issue(ctx context.Context, tx SetupTx) error {
	plain, tok, err := domain.NewSetupToken(s.Rand, s.Now(), domain.SetupTokenTTL)
	if err != nil {
		return err
	}
	if err := tx.SaveSetupToken(ctx, tok); err != nil {
		return err
	}
	if err := s.Channel.Publish(plain); err != nil {
		return fmt.Errorf("publishing setup token: %w", err)
	}
	return nil
}

func (s *SetupService) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
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
	in.Token = strings.TrimSpace(in.Token) // operators copy it from a file that ends with a newline
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
	err = s.Repo.InTx(ctx, func(ctx context.Context, tx SetupTx) error {
		st, err := tx.SetupState(ctx)
		if err != nil {
			return err
		}
		if err := st.Verify(in.Token, now); err != nil {
			return err
		}
		if err := tx.CompleteSetup(ctx, owner, name, now); err != nil {
			return err
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.User(owner.ID, string(owner.Login)), Kind: KindSetupCompleted,
			Subject: userSubject(owner.ID), Details: map[string]string{"platform_name": string(name)}})
	})
	if err != nil {
		return domain.User{}, err
	}
	// Setup is done; a leftover token file holds a token the database no longer accepts, and Prepare
	// removes it on the next start, so a failure here must not turn a completed setup into an error.
	if err := s.clear(); err != nil {
		s.logger().Warn("could not remove used setup token file", "err", err)
	}
	return owner, nil
}

func (s *SetupService) verify(ctx context.Context, token string) error {
	st, err := s.Repo.State(ctx)
	if err != nil {
		return err
	}
	return st.Verify(token, s.Now())
}
