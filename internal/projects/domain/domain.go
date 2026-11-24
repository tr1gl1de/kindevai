// Package domain is the projects bounded context: projects, memberships and who may do what with them.
// Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotFound: the project does not exist or the actor may not see it (indistinguishable on purpose).
	ErrNotFound = errors.New("project not found")
	// ErrForbidden: the actor sees the project but may not perform the action.
	ErrForbidden = errors.New("forbidden")
	// ErrSlugTaken: another project already uses the slug.
	ErrSlugTaken = errors.New("project slug is already taken")
	// ErrUserNotFound: no user with the given login.
	ErrUserNotFound = errors.New("user not found")
	// ErrNotMember: the user exists but is not a member of the project.
	ErrNotMember = errors.New("user is not a member of the project")
)

// ValidationError reports invalid input for one field with a stable code.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

// Name is a project's display name.
type Name string

// NewName trims and validates 1–80 printable characters.
func NewName(s string) (Name, error) {
	n := strings.TrimSpace(s)
	if n == "" || utf8.RuneCountInString(n) > 80 || strings.IndexFunc(n, unicode.IsControl) >= 0 {
		return "", &ValidationError{Field: "name", Code: "name_format"}
	}
	return Name(n), nil
}

// Slug is a project's unique URL-safe identifier.
type Slug string

var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])$`)

// NewSlug lower-cases and validates 2–40 of a-z 0-9 '-', not starting or ending with '-'.
func NewSlug(s string) (Slug, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if !slugRe.MatchString(v) {
		return "", &ValidationError{Field: "slug", Code: "slug_format"}
	}
	return Slug(v), nil
}

// Project is a managed project.
type Project struct {
	ID        string
	Slug      Slug
	Name      Name
	CreatedAt time.Time
}

// Role is a user's role inside one project. The platform Owner is not a project role: Owner sees everything.
type Role string

const (
	RoleProjectAdmin Role = "project_admin"
	RoleMember       Role = "member"
)

// ParseRole validates a project role.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleProjectAdmin, RoleMember:
		return r, nil
	}
	return "", &ValidationError{Field: "role", Code: "role_unknown"}
}

// Membership grants a user a role in a project.
type Membership struct {
	ProjectID string
	UserID    string
	Role      Role
	CreatedAt time.Time
}

// Actor is who performs an action.
type Actor struct {
	UserID string
	Login  string // for the audit log's actor label
	Owner  bool
}

// Access is an actor's standing in one project.
type Access struct {
	actor    Actor
	role     Role
	isMember bool
}

// NewAccess combines an actor with their membership in a project (isMember false if none).
func NewAccess(a Actor, r Role, isMember bool) Access {
	return Access{actor: a, role: r, isMember: isMember}
}

// CanView: Owner and members see the project; nobody else learns it exists.
func (a Access) CanView() bool { return a.actor.Owner || a.isMember }

// CanManageMembers: only the Owner grants or changes memberships in the MVP (KIN-A-21).
func (a Access) CanManageMembers() bool { return a.actor.Owner }

// Role is the actor's project role, if a member.
func (a Access) Role() (Role, bool) { return a.role, a.isMember }

// CanCreateProject: only the Owner creates projects.
func CanCreateProject(a Actor) bool { return a.Owner }
