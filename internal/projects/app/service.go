// Package app holds the projects use cases.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

// Audit event kinds recorded by the projects context.
const (
	KindProjectCreated       = "project.created"
	KindProjectMemberSet     = "project.member_set"
	KindProjectMemberRemoved = "project.member_removed"
)

// AuditLog is the audit context's API as projects needs it. Record must be called with the ctx of the
// transaction that performs the change, so the event commits or rolls back with it.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// TxRunner runs fn in one database transaction; repositories called with fn's ctx join it.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Repository persists projects and memberships.
type Repository interface {
	Create(ctx context.Context, p domain.Project) error // domain.ErrSlugTaken on conflict
	ByID(ctx context.Context, id string) (domain.Project, error)
	All(ctx context.Context) ([]domain.Project, error)
	ForUser(ctx context.Context, userID string) ([]domain.Project, error)
	Membership(ctx context.Context, projectID, userID string) (domain.Membership, bool, error)
	Members(ctx context.Context, projectID string) ([]domain.Membership, error)
	SetMember(ctx context.Context, m domain.Membership) error // insert or change role
	RemoveMember(ctx context.Context, projectID, userID string) (bool, error)
}

// UserRef is how the projects context sees an account (owned by the identity context).
type UserRef struct {
	ID       string
	Login    string
	Owner    bool
	Disabled bool
}

// UserDirectory resolves accounts, including disabled ones; implemented on top of the identity context.
type UserDirectory interface {
	ByLogin(ctx context.Context, login string) (UserRef, error) // domain.ErrUserNotFound
	ByIDs(ctx context.Context, ids []string) (map[string]UserRef, error)
}

// Service implements project and membership use cases with access checks.
type Service struct {
	Repo  Repository
	Users UserDirectory
	Tx    TxRunner
	Audit AuditLog
	Now   func() time.Time
	NewID func() string
}

var errNoAuditLog = errors.New("projects: audit log or transactions not configured")

// change runs write and records its audit event in one transaction.
func (s *Service) change(ctx context.Context, a domain.Actor, e auditapp.Entry, write func(ctx context.Context) error) error {
	if s.Audit == nil || s.Tx == nil { // a wiring mistake must not silently drop the record
		return errNoAuditLog
	}
	e.Actor = auditapp.User(a.UserID, a.Login)
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := write(ctx); err != nil {
			return err
		}
		return s.Audit.Record(ctx, e)
	})
}

// ProjectView is a project as seen by an actor; MyRole is "owner" for the platform Owner.
type ProjectView struct {
	Project domain.Project
	MyRole  string
}

// MemberView is a membership with the member's login.
type MemberView struct {
	UserID string
	Login  string
	Role   string
}

// access loads the project and the actor's standing; invisible projects are ErrNotFound.
func (s *Service) access(ctx context.Context, a domain.Actor, projectID string) (domain.Project, domain.Access, error) {
	p, err := s.Repo.ByID(ctx, projectID)
	if err != nil {
		return domain.Project{}, domain.Access{}, err
	}
	m, ok, err := s.Repo.Membership(ctx, projectID, a.UserID)
	if err != nil {
		return domain.Project{}, domain.Access{}, fmt.Errorf("loading membership: %w", err)
	}
	acc := domain.NewAccess(a, m.Role, ok)
	if !acc.CanView() {
		return domain.Project{}, domain.Access{}, domain.ErrNotFound
	}
	return p, acc, nil
}

func myRole(a domain.Actor, acc domain.Access) string {
	if a.Owner {
		return "owner"
	}
	r, _ := acc.Role()
	return string(r)
}

// List returns the projects the actor may see.
func (s *Service) List(ctx context.Context, a domain.Actor) ([]ProjectView, error) {
	var ps []domain.Project
	var err error
	if a.Owner {
		ps, err = s.Repo.All(ctx)
	} else {
		ps, err = s.Repo.ForUser(ctx, a.UserID)
	}
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	out := make([]ProjectView, 0, len(ps))
	for _, p := range ps {
		role := "owner"
		if !a.Owner {
			m, _, err := s.Repo.Membership(ctx, p.ID, a.UserID)
			if err != nil {
				return nil, fmt.Errorf("loading membership: %w", err)
			}
			role = string(m.Role)
		}
		out = append(out, ProjectView{Project: p, MyRole: role})
	}
	return out, nil
}

// Create adds a project (Owner only).
func (s *Service) Create(ctx context.Context, a domain.Actor, name, slug string) (domain.Project, error) {
	if !domain.CanCreateProject(a) {
		return domain.Project{}, domain.ErrForbidden
	}
	n, err := domain.NewName(name)
	if err != nil {
		return domain.Project{}, err
	}
	sl, err := domain.NewSlug(slug)
	if err != nil {
		return domain.Project{}, err
	}
	p := domain.Project{ID: s.NewID(), Slug: sl, Name: n, CreatedAt: s.Now()}
	err = s.change(ctx, a, auditapp.Entry{Kind: KindProjectCreated, ProjectID: p.ID, Subject: "project:" + p.ID,
		Details: map[string]string{"slug": string(p.Slug), "name": string(p.Name)}},
		func(ctx context.Context) error { return s.Repo.Create(ctx, p) })
	if err != nil {
		return domain.Project{}, err
	}
	return p, nil
}

// Get returns one visible project.
func (s *Service) Get(ctx context.Context, a domain.Actor, projectID string) (ProjectView, error) {
	p, acc, err := s.access(ctx, a, projectID)
	if err != nil {
		return ProjectView{}, err
	}
	return ProjectView{Project: p, MyRole: myRole(a, acc)}, nil
}

// Members lists a visible project's members.
func (s *Service) Members(ctx context.Context, a domain.Actor, projectID string) ([]MemberView, error) {
	if _, _, err := s.access(ctx, a, projectID); err != nil {
		return nil, err
	}
	ms, err := s.Repo.Members(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.UserID
	}
	users, err := s.Users.ByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("resolving members: %w", err)
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		out = append(out, MemberView{UserID: m.UserID, Login: users[m.UserID].Login, Role: string(m.Role)})
	}
	return out, nil
}

// SetMember grants or changes a user's role in a project (Owner only).
func (s *Service) SetMember(ctx context.Context, a domain.Actor, projectID, login, role string) (MemberView, error) {
	_, acc, err := s.access(ctx, a, projectID)
	if err != nil {
		return MemberView{}, err
	}
	if !acc.CanManageMembers() {
		return MemberView{}, domain.ErrForbidden
	}
	r, err := domain.ParseRole(role)
	if err != nil {
		return MemberView{}, err
	}
	u, err := s.Users.ByLogin(ctx, strings.ToLower(strings.TrimSpace(login)))
	if err != nil {
		return MemberView{}, err
	}
	if u.Owner {
		return MemberView{}, &domain.ValidationError{Field: "login", Code: "owner_has_full_access"}
	}
	if u.Disabled {
		return MemberView{}, &domain.ValidationError{Field: "login", Code: "user_disabled"}
	}
	m := domain.Membership{ProjectID: projectID, UserID: u.ID, Role: r, CreatedAt: s.Now()}
	err = s.change(ctx, a, auditapp.Entry{Kind: KindProjectMemberSet, ProjectID: projectID, Subject: "user:" + u.ID,
		Details: map[string]string{"login": u.Login, "role": string(r)}},
		func(ctx context.Context) error { return s.Repo.SetMember(ctx, m) })
	if err != nil {
		return MemberView{}, err
	}
	return MemberView{UserID: u.ID, Login: u.Login, Role: string(r)}, nil
}

// RemoveMember revokes a membership (Owner only), also for disabled users. Removing a non-member is ErrNotMember.
func (s *Service) RemoveMember(ctx context.Context, a domain.Actor, projectID, login string) error {
	_, acc, err := s.access(ctx, a, projectID)
	if err != nil {
		return err
	}
	if !acc.CanManageMembers() {
		return domain.ErrForbidden
	}
	u, err := s.Users.ByLogin(ctx, strings.ToLower(strings.TrimSpace(login)))
	if err != nil {
		return err
	}
	return s.change(ctx, a, auditapp.Entry{Kind: KindProjectMemberRemoved, ProjectID: projectID, Subject: "user:" + u.ID,
		Details: map[string]string{"login": u.Login}},
		func(ctx context.Context) error {
			removed, err := s.Repo.RemoveMember(ctx, projectID, u.ID)
			if err != nil {
				return fmt.Errorf("removing membership: %w", err)
			}
			if !removed {
				return domain.ErrNotMember
			}
			return nil
		})
}

// Lookup loads a project without an access check. Trusted: for other contexts' use cases that authorise the caller
// themselves (identity's invitations are issued by the Owner only).
func (s *Service) Lookup(ctx context.Context, projectID string) (domain.Project, error) {
	return s.Repo.ByID(ctx, projectID)
}

// ValidateRole checks that role is a project role.
func (s *Service) ValidateRole(role string) error {
	_, err := domain.ParseRole(role)
	return err
}

// Grant adds a membership without an access check. Trusted: used when an Owner-issued invitation is accepted,
// inside the acceptance transaction carried by ctx.
func (s *Service) Grant(ctx context.Context, projectID, userID, role string) error {
	r, err := domain.ParseRole(role)
	if err != nil {
		return err
	}
	return s.Repo.SetMember(ctx, domain.Membership{ProjectID: projectID, UserID: userID, Role: r, CreatedAt: s.Now()})
}
