package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

// InvitationRepository stores invitations.
type InvitationRepository interface {
	Create(ctx context.Context, inv domain.Invitation) error
	List(ctx context.Context, limit int) ([]domain.Invitation, error) // newest first
	ByID(ctx context.Context, id string) (domain.Invitation, error)   // domain.ErrNotFound
	ByTokenHash(ctx context.Context, hash []byte) (domain.Invitation, error)
	// MarkConsumed sets consumed_at/by only if the invitation is still pending at now; false if it was not.
	MarkConsumed(ctx context.Context, id, userID string, now time.Time) (bool, error)
	// Revoke sets revoked_at only if the invitation is still pending at now; false if it was not.
	Revoke(ctx context.Context, id string, now time.Time) (bool, error)
}

// TxRunner runs fn in one database transaction; repositories called with fn's ctx join it.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// ProjectRef is how the identity context sees a project (owned by the projects context).
type ProjectRef struct {
	ID   string
	Name string
}

// Projects is the projects context's API as invitations need it.
type Projects interface {
	Lookup(ctx context.Context, projectID string) (ProjectRef, error) // domain.ErrNotFound if missing
	ValidateRole(role string) error
	// Grant adds the membership; called inside the acceptance transaction.
	Grant(ctx context.Context, projectID, userID, role string) error
}

// InvitationView is an invitation with its project's name and state, for the Owner.
type InvitationView struct {
	Invitation  domain.Invitation
	ProjectName string
	State       domain.InvitationState
}

// InvitationPreview is what a token holder may see before accepting.
type InvitationPreview struct {
	ProjectName string
	Role        string
	ExpiresAt   time.Time
}

// InvitationsService implements one-time invitations: the Owner issues and revokes them; the holder of the token
// creates their own account with a project membership.
type InvitationsService struct {
	Invitations InvitationRepository
	Accounts    AccountRepository
	Projects    Projects
	Tx          TxRunner
	Hasher      PasswordHasher
	Rand        io.Reader
	Now         func() time.Time
	NewID       func() string
	Logger      *slog.Logger
}

const invitationListLimit = 200

func (s *InvitationsService) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

var errProjectNotFound = &domain.ValidationError{Field: "projectId", Code: "project_not_found"}

// Create issues an invitation (Owner only) and returns the plaintext token once.
func (s *InvitationsService) Create(ctx context.Context, actor auth.Principal, projectID, role string, ttlHours int) (InvitationView, string, error) {
	if err := ownerOnly(actor); err != nil {
		return InvitationView{}, "", err
	}
	if err := s.Projects.ValidateRole(role); err != nil {
		return InvitationView{}, "", err
	}
	ttl, err := domain.InvitationTTL(ttlHours)
	if err != nil {
		return InvitationView{}, "", err
	}
	p, err := s.Projects.Lookup(ctx, projectID)
	if errors.Is(err, domain.ErrNotFound) {
		return InvitationView{}, "", errProjectNotFound
	}
	if err != nil {
		return InvitationView{}, "", fmt.Errorf("looking up project: %w", err)
	}
	plain, hash, err := domain.NewInvitationToken(s.Rand)
	if err != nil {
		return InvitationView{}, "", err
	}
	now := s.Now()
	inv := domain.Invitation{
		ID: s.NewID(), TokenHash: hash, ProjectID: p.ID, Role: role, CreatedBy: actor.UserID,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := s.Invitations.Create(ctx, inv); err != nil {
		return InvitationView{}, "", fmt.Errorf("saving invitation: %w", err)
	}
	s.logger().Info("invitation created", "invitation", inv.ID, "project", p.ID, "role", role, "by", actor.UserID)
	return InvitationView{Invitation: inv, ProjectName: p.Name, State: domain.InvitationPending}, plain, nil
}

// List returns recent invitations (Owner only), newest first.
func (s *InvitationsService) List(ctx context.Context, actor auth.Principal) ([]InvitationView, error) {
	if err := ownerOnly(actor); err != nil {
		return nil, err
	}
	invs, err := s.Invitations.List(ctx, invitationListLimit)
	if err != nil {
		return nil, fmt.Errorf("listing invitations: %w", err)
	}
	now := s.Now()
	names := map[string]string{}
	out := make([]InvitationView, 0, len(invs))
	for _, inv := range invs {
		name, ok := names[inv.ProjectID]
		if !ok {
			// Projects are few; a deleted project leaves its invitations listed without a name.
			if p, err := s.Projects.Lookup(ctx, inv.ProjectID); err == nil {
				name = p.Name
			} else if !errors.Is(err, domain.ErrNotFound) {
				return nil, fmt.Errorf("looking up project: %w", err)
			}
			names[inv.ProjectID] = name
		}
		out = append(out, InvitationView{Invitation: inv, ProjectName: name, State: inv.State(now)})
	}
	return out, nil
}

// Revoke withdraws a pending invitation (Owner only).
func (s *InvitationsService) Revoke(ctx context.Context, actor auth.Principal, id string) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	inv, err := s.Invitations.ByID(ctx, id)
	if err != nil {
		return err
	}
	now := s.Now()
	if err := inv.Usable(now); err != nil {
		return &domain.ValidationError{Field: "invitation", Code: "invitation_not_pending"}
	}
	ok, err := s.Invitations.Revoke(ctx, id, now)
	if err != nil {
		return fmt.Errorf("revoking invitation: %w", err)
	}
	if !ok { // accepted or expired between the read and the update
		return &domain.ValidationError{Field: "invitation", Code: "invitation_not_pending"}
	}
	s.logger().Info("invitation revoked", "invitation", id, "by", actor.UserID)
	return nil
}

func (s *InvitationsService) pending(ctx context.Context, token string) (domain.Invitation, error) {
	if token == "" {
		return domain.Invitation{}, domain.ErrInvitationInvalid
	}
	inv, err := s.Invitations.ByTokenHash(ctx, domain.HashInvitationToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Invitation{}, domain.ErrInvitationInvalid
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("looking up invitation: %w", err)
	}
	if err := inv.Usable(s.Now()); err != nil {
		return domain.Invitation{}, err
	}
	return inv, nil
}

// Preview shows the token holder what they are accepting.
func (s *InvitationsService) Preview(ctx context.Context, token string) (InvitationPreview, error) {
	inv, err := s.pending(ctx, token)
	if err != nil {
		return InvitationPreview{}, err
	}
	p, err := s.Projects.Lookup(ctx, inv.ProjectID)
	if errors.Is(err, domain.ErrNotFound) {
		return InvitationPreview{}, domain.ErrInvitationInvalid // the project was deleted
	}
	if err != nil {
		return InvitationPreview{}, fmt.Errorf("looking up project: %w", err)
	}
	return InvitationPreview{ProjectName: p.Name, Role: inv.Role, ExpiresAt: inv.ExpiresAt}, nil
}

// Accept creates the holder's account and project membership and consumes the invitation, atomically: either all
// three happen or none (a taken login leaves the invitation usable). The new user is not signed in.
func (s *InvitationsService) Accept(ctx context.Context, token, login, password string) (domain.User, error) {
	if _, err := s.pending(ctx, token); err != nil {
		return domain.User{}, err
	}
	l, err := domain.NewLogin(login)
	if err != nil {
		return domain.User{}, err
	}
	if err := domain.ValidatePassword(password, l); err != nil {
		return domain.User{}, err
	}
	// Hash before the transaction: Argon2id is slow and the database has a single connection.
	hash, err := s.Hasher.Hash(ctx, password)
	if err != nil {
		return domain.User{}, fmt.Errorf("hashing password: %w", err)
	}
	var u domain.User
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		inv, err := s.pending(ctx, token) // re-read inside the transaction
		if err != nil {
			return err
		}
		if _, err := s.Accounts.ByLogin(ctx, l); err == nil {
			return domain.ErrLoginTaken
		} else if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("checking login: %w", err)
		}
		now := s.Now()
		u = domain.User{ID: s.NewID(), Login: l, PasswordHash: hash, Role: domain.RoleUser, CreatedAt: now}
		if err := s.Accounts.Create(ctx, u); err != nil {
			return err
		}
		ok, err := s.Invitations.MarkConsumed(ctx, inv.ID, u.ID, now)
		if err != nil {
			return fmt.Errorf("consuming invitation: %w", err)
		}
		if !ok {
			return domain.ErrInvitationUsed
		}
		if err := s.Projects.Grant(ctx, inv.ProjectID, u.ID, inv.Role); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.ErrInvitationInvalid
			}
			return fmt.Errorf("granting membership: %w", err)
		}
		s.logger().Info("invitation accepted", "invitation", inv.ID, "project", inv.ProjectID, "user", u.ID)
		return nil
	})
	if err != nil {
		return domain.User{}, err
	}
	return u, nil
}
