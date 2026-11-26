// Package sqlite implements the projects repository on the Control Plane SQLite database.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

// tsFormat is fixed-width UTC so timestamps sort correctly as text.
const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository stores projects and memberships.
type Repository struct{ db *sql.DB }

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

// Create inserts a project; a duplicate slug is domain.ErrSlugTaken.
func (r *Repository) Create(ctx context.Context, p domain.Project) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, "INSERT INTO projects(id, slug, name, created_at) VALUES (?, ?, ?, ?)",
		p.ID, string(p.Slug), string(p.Name), ts(p.CreatedAt))
	if db.IsUniqueViolation(err) {
		return domain.ErrSlugTaken
	}
	if err != nil {
		return fmt.Errorf("creating project: %w", err)
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanProject(s scanner) (domain.Project, error) {
	var p domain.Project
	var slug, name, created string
	if err := s.Scan(&p.ID, &slug, &name, &created); err != nil {
		return domain.Project{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return domain.Project{}, fmt.Errorf("reading project: %w", err)
	}
	p.Slug, p.Name, p.CreatedAt = domain.Slug(slug), domain.Name(name), t
	return p, nil
}

// ByID loads a project.
func (r *Repository) ByID(ctx context.Context, id string) (domain.Project, error) {
	p, err := scanProject(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT id, slug, name, created_at FROM projects WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("reading project: %w", err)
	}
	return p, nil
}

func (r *Repository) list(ctx context.Context, q string, args ...any) ([]domain.Project, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// All lists every project by name.
func (r *Repository) All(ctx context.Context) ([]domain.Project, error) {
	return r.list(ctx, "SELECT id, slug, name, created_at FROM projects ORDER BY name, id")
}

// ForUser lists projects where the user is a member.
func (r *Repository) ForUser(ctx context.Context, userID string) ([]domain.Project, error) {
	return r.list(ctx, `SELECT p.id, p.slug, p.name, p.created_at FROM projects p
		JOIN project_memberships m ON m.project_id = p.id WHERE m.user_id = ? ORDER BY p.name, p.id`, userID)
}

// Membership returns the user's membership in the project, if any.
func (r *Repository) Membership(ctx context.Context, projectID, userID string) (domain.Membership, bool, error) {
	m := domain.Membership{ProjectID: projectID, UserID: userID}
	var role, created string
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT role, created_at FROM project_memberships WHERE project_id = ? AND user_id = ?",
		projectID, userID).Scan(&role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Membership{}, false, nil
	}
	if err != nil {
		return domain.Membership{}, false, fmt.Errorf("reading membership: %w", err)
	}
	m.Role = domain.Role(role)
	if m.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return domain.Membership{}, false, fmt.Errorf("reading membership: %w", err)
	}
	return m, true, nil
}

// Members lists a project's memberships.
func (r *Repository) Members(ctx context.Context, projectID string) ([]domain.Membership, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx,
		"SELECT user_id, role, created_at FROM project_memberships WHERE project_id = ? ORDER BY created_at, user_id", projectID)
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	defer rows.Close()
	var out []domain.Membership
	for rows.Next() {
		m := domain.Membership{ProjectID: projectID}
		var role, created string
		if err := rows.Scan(&m.UserID, &role, &created); err != nil {
			return nil, fmt.Errorf("listing members: %w", err)
		}
		m.Role = domain.Role(role)
		t, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("listing members: %w", err)
		}
		m.CreatedAt = t
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMember inserts a membership or changes its role.
func (r *Repository) SetMember(ctx context.Context, m domain.Membership) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO project_memberships(project_id, user_id, role, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, user_id) DO UPDATE SET role = excluded.role`,
		m.ProjectID, m.UserID, string(m.Role), ts(m.CreatedAt))
	if db.IsForeignKeyViolation(err) {
		// The project or the user disappeared after the access check.
		if _, perr := r.ByID(ctx, m.ProjectID); errors.Is(perr, domain.ErrNotFound) {
			return domain.ErrNotFound
		}
		return domain.ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("saving membership: %w", err)
	}
	return nil
}

// RemoveMember deletes a membership and reports whether one existed.
func (r *Repository) RemoveMember(ctx context.Context, projectID, userID string) (bool, error) {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM project_memberships WHERE project_id = ? AND user_id = ?", projectID, userID)
	if err != nil {
		return false, fmt.Errorf("removing membership: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
