// Package sqlite stores the integrations context in SQLite. Every statement runs on db.Conn, so use cases make
// their writes, the audit record and outbox messages atomic. Credentials arrive here already encrypted.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository implements app.Store and app.SecretStore.
type Repository struct{ db *sql.DB }

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func parseNullTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTS(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *Repository) conn(ctx context.Context) db.Executor { return db.Conn(ctx, r.db) }

// PutSecret inserts or replaces a credential's ciphertext.
func (r *Repository) PutSecret(ctx context.Context, name domain.SecretName, ciphertext []byte, by string, at time.Time) error {
	_, err := r.conn(ctx).ExecContext(ctx, `INSERT INTO integration_secrets(name, ciphertext, updated_at, updated_by)
		VALUES (?, ?, ?, ?) ON CONFLICT(name) DO UPDATE SET ciphertext = excluded.ciphertext,
		updated_at = excluded.updated_at, updated_by = excluded.updated_by`, string(name), ciphertext, ts(at), nullStr(by))
	if err != nil {
		return fmt.Errorf("storing credential: %w", err)
	}
	return nil
}

// SecretCiphertext returns a credential's ciphertext; domain.ErrSecretNotSet when missing.
func (r *Repository) SecretCiphertext(ctx context.Context, name domain.SecretName) ([]byte, error) {
	var ct []byte
	err := r.conn(ctx).QueryRowContext(ctx, "SELECT ciphertext FROM integration_secrets WHERE name = ?", string(name)).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrSecretNotSet
	}
	if err != nil {
		return nil, fmt.Errorf("reading credential: %w", err)
	}
	return ct, nil
}

// SecretMeta reports whether a credential is set and when it changed.
func (r *Repository) SecretMeta(ctx context.Context, name domain.SecretName) (domain.SecretMeta, error) {
	m := domain.SecretMeta{Name: name}
	var at string
	var by sql.NullString
	err := r.conn(ctx).QueryRowContext(ctx, "SELECT updated_at, updated_by FROM integration_secrets WHERE name = ?",
		string(name)).Scan(&at, &by)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return m, fmt.Errorf("reading credential metadata: %w", err)
	}
	if m.UpdatedAt, err = parseTS(at); err != nil {
		return m, fmt.Errorf("reading credential metadata: %w", err)
	}
	m.Set, m.UpdatedBy = true, by.String
	return m, nil
}

// DeleteSecret removes a credential.
func (r *Repository) DeleteSecret(ctx context.Context, name domain.SecretName) error {
	if _, err := r.conn(ctx).ExecContext(ctx, "DELETE FROM integration_secrets WHERE name = ?", string(name)); err != nil {
		return fmt.Errorf("deleting credential: %w", err)
	}
	return nil
}

// YouTrack loads the connection; domain.ErrNotConfigured when there is none.
func (r *Repository) YouTrack(ctx context.Context) (domain.YouTrackConnection, error) {
	var c domain.YouTrackConnection
	var updated, status string
	var requested, checked sql.NullString
	err := r.conn(ctx).QueryRowContext(ctx, `SELECT base_url, updated_at, check_status, check_id, check_code, check_detail,
		check_requested_at, checked_at FROM youtrack_connection WHERE id = 1`).Scan(&c.BaseURL, &updated, &status,
		&c.Check.ID, &c.Check.Code, &c.Check.Detail, &requested, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return c, domain.ErrNotConfigured
	}
	if err != nil {
		return c, fmt.Errorf("reading youtrack connection: %w", err)
	}
	c.Check.Status = domain.CheckStatus(status)
	if c.UpdatedAt, err = parseTS(updated); err == nil {
		if c.Check.RequestedAt, err = parseNullTS(requested); err == nil {
			c.Check.CheckedAt, err = parseNullTS(checked)
		}
	}
	if err != nil {
		return c, fmt.Errorf("reading youtrack connection: %w", err)
	}
	return c, nil
}

// SaveYouTrack inserts or replaces the connection.
func (r *Repository) SaveYouTrack(ctx context.Context, c domain.YouTrackConnection) error {
	status := c.Check.Status
	if status == "" {
		status = domain.CheckNone
	}
	_, err := r.conn(ctx).ExecContext(ctx, `INSERT INTO youtrack_connection(id, base_url, updated_at, check_status, check_id,
		check_code, check_detail, check_requested_at, checked_at) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET base_url = excluded.base_url, updated_at = excluded.updated_at,
		check_status = excluded.check_status, check_id = excluded.check_id, check_code = excluded.check_code,
		check_detail = excluded.check_detail, check_requested_at = excluded.check_requested_at, checked_at = excluded.checked_at`,
		c.BaseURL, ts(c.UpdatedAt), string(status), c.Check.ID, c.Check.Code, c.Check.Detail,
		nullTS(c.Check.RequestedAt), nullTS(c.Check.CheckedAt))
	if err != nil {
		return fmt.Errorf("saving youtrack connection: %w", err)
	}
	return nil
}

// DeleteYouTrack removes the connection.
func (r *Repository) DeleteYouTrack(ctx context.Context) error {
	if _, err := r.conn(ctx).ExecContext(ctx, "DELETE FROM youtrack_connection"); err != nil {
		return fmt.Errorf("deleting youtrack connection: %w", err)
	}
	return nil
}

// ProjectYouTrack returns the short name bound to the project, "" when unbound.
func (r *Repository) ProjectYouTrack(ctx context.Context, projectID string) (string, error) {
	var sn string
	err := r.conn(ctx).QueryRowContext(ctx, "SELECT short_name FROM project_youtrack WHERE project_id = ?", projectID).Scan(&sn)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading youtrack binding: %w", err)
	}
	return sn, nil
}

// SetProjectYouTrack binds or (with "") unbinds a project.
func (r *Repository) SetProjectYouTrack(ctx context.Context, projectID, shortName string, at time.Time) error {
	var err error
	if shortName == "" {
		_, err = r.conn(ctx).ExecContext(ctx, "DELETE FROM project_youtrack WHERE project_id = ?", projectID)
	} else {
		_, err = r.conn(ctx).ExecContext(ctx, `INSERT INTO project_youtrack(project_id, short_name, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(project_id) DO UPDATE SET short_name = excluded.short_name, updated_at = excluded.updated_at,
			poll_cursor = CASE WHEN short_name = excluded.short_name THEN poll_cursor ELSE 0 END,
			last_polled_at = CASE WHEN short_name = excluded.short_name THEN last_polled_at ELSE NULL END`,
			projectID, shortName, ts(at))
	}
	switch {
	case db.IsUniqueViolation(err):
		return domain.ErrShortNameTaken
	case db.IsForeignKeyViolation(err):
		return domain.ErrNotFound
	case err != nil:
		return fmt.Errorf("saving youtrack binding: %w", err)
	}
	return nil
}

const repoCols = `id, project_id, owner, name, integration_branch, created_at, updated_at, gh_installation_id,
	gh_default_branch, gh_protection, gh_unprotected, gh_sync_status, gh_sync_id, gh_sync_code, gh_sync_detail,
	gh_sync_requested_at, gh_synced_at, test_command`

type scanner interface{ Scan(dest ...any) error }

func scanRepo(row scanner) (domain.Repository, error) {
	var r domain.Repository
	var created, updated, protection, unprotected, syncStatus string
	var requested, synced sql.NullString
	g := &r.GitHub
	err := row.Scan(&r.ID, &r.ProjectID, &r.Owner, &r.Name, &r.IntegrationBranch, &created, &updated, &g.InstallationID,
		&g.DefaultBranch, &protection, &unprotected, &syncStatus, &g.Sync.ID, &g.Sync.Code, &g.Sync.Detail, &requested, &synced,
		&r.TestCommand)
	if errors.Is(err, sql.ErrNoRows) {
		return r, domain.ErrNotFound
	}
	if err == nil {
		if r.CreatedAt, err = parseTS(created); err == nil {
			if r.UpdatedAt, err = parseTS(updated); err == nil {
				if g.Sync.RequestedAt, err = parseNullTS(requested); err == nil {
					g.Sync.CheckedAt, err = parseNullTS(synced)
				}
			}
		}
	}
	if err != nil {
		return r, fmt.Errorf("reading repository: %w", err)
	}
	g.Protection, g.Sync.Status = domain.ProtectionStatus(protection), domain.CheckStatus(syncStatus)
	if unprotected != "" {
		g.Unprotected = strings.Split(unprotected, ",")
	}
	return r, nil
}

func repoArgs(repo domain.Repository) []any {
	g := repo.GitHub
	protection, status := g.Protection, g.Sync.Status
	if protection == "" {
		protection = domain.ProtectionUnknown
	}
	if status == "" {
		status = domain.CheckNone
	}
	return []any{repo.ID, repo.ProjectID, repo.Owner, repo.Name, repo.IntegrationBranch, ts(repo.CreatedAt), ts(repo.UpdatedAt),
		g.InstallationID, g.DefaultBranch, string(protection), strings.Join(g.Unprotected, ","), string(status), g.Sync.ID,
		g.Sync.Code, g.Sync.Detail, nullTS(g.Sync.RequestedAt), nullTS(g.Sync.CheckedAt), repo.TestCommand}
}

func (r *Repository) listRepos(ctx context.Context, where string, args ...any) ([]domain.Repository, error) {
	rows, err := r.conn(ctx).QueryContext(ctx, "SELECT "+repoCols+" FROM project_repositories "+where+" ORDER BY owner, name", args...)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	defer rows.Close()
	out := []domain.Repository{}
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	return out, nil
}

// Repositories lists a project's repositories by full name.
func (r *Repository) Repositories(ctx context.Context, projectID string) ([]domain.Repository, error) {
	return r.listRepos(ctx, "WHERE project_id = ?", projectID)
}

// AllRepositories lists every registered repository.
func (r *Repository) AllRepositories(ctx context.Context) ([]domain.Repository, error) {
	return r.listRepos(ctx, "")
}

// RepositoryByID loads a repository of any project; domain.ErrNotFound otherwise.
func (r *Repository) RepositoryByID(ctx context.Context, id string) (domain.Repository, error) {
	return scanRepo(r.conn(ctx).QueryRowContext(ctx, "SELECT "+repoCols+" FROM project_repositories WHERE id = ?", id))
}

// RepositoryByFullName finds a repository by "owner/name" (case-insensitive, like GitHub); domain.ErrNotFound otherwise.
func (r *Repository) RepositoryByFullName(ctx context.Context, fullName string) (domain.Repository, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return domain.Repository{}, domain.ErrNotFound
	}
	return scanRepo(r.conn(ctx).QueryRowContext(ctx, "SELECT "+repoCols+" FROM project_repositories WHERE owner = ? AND name = ?", owner, name))
}

// Repository loads one of the project's repositories; domain.ErrNotFound otherwise.
func (r *Repository) Repository(ctx context.Context, projectID, id string) (domain.Repository, error) {
	return scanRepo(r.conn(ctx).QueryRowContext(ctx, "SELECT "+repoCols+" FROM project_repositories WHERE id = ? AND project_id = ?", id, projectID))
}

// CreateRepository inserts a repository.
func (r *Repository) CreateRepository(ctx context.Context, repo domain.Repository) error {
	_, err := r.conn(ctx).ExecContext(ctx, "INSERT INTO project_repositories("+repoCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, repoArgs(repo)...)
	switch {
	case db.IsUniqueViolation(err):
		return domain.ErrRepositoryExists
	case db.IsForeignKeyViolation(err):
		return domain.ErrNotFound
	case err != nil:
		return fmt.Errorf("saving repository: %w", err)
	}
	return nil
}

// SaveRepository updates a repository's integration branch and GitHub state.
func (r *Repository) SaveRepository(ctx context.Context, repo domain.Repository) error {
	a := repoArgs(repo)
	res, err := r.conn(ctx).ExecContext(ctx, `UPDATE project_repositories SET integration_branch = ?, updated_at = ?,
		gh_installation_id = ?, gh_default_branch = ?, gh_protection = ?, gh_unprotected = ?, gh_sync_status = ?, gh_sync_id = ?,
		gh_sync_code = ?, gh_sync_detail = ?, gh_sync_requested_at = ?, gh_synced_at = ?, test_command = ?
		WHERE id = ? AND project_id = ?`,
		append(append([]any{a[4], a[6]}, a[7:]...), repo.ID, repo.ProjectID)...)
	if err != nil {
		return fmt.Errorf("saving repository: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteRepository removes one of the project's repositories.
func (r *Repository) DeleteRepository(ctx context.Context, projectID, id string) error {
	res, err := r.conn(ctx).ExecContext(ctx, "DELETE FROM project_repositories WHERE id = ? AND project_id = ?", id, projectID)
	if err != nil {
		return fmt.Errorf("deleting repository: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// GitHubApp loads the App connection; domain.ErrNotConfigured when there is none.
func (r *Repository) GitHubApp(ctx context.Context) (domain.GitHubApp, error) {
	var a domain.GitHubApp
	var updated, status string
	var requested, checked, delivered sql.NullString
	err := r.conn(ctx).QueryRowContext(ctx, `SELECT app_id, updated_at, check_status, check_id, check_code, check_detail,
		check_requested_at, checked_at, last_delivery_at, last_event FROM github_app WHERE id = 1`).Scan(&a.AppID, &updated, &status,
		&a.Check.ID, &a.Check.Code, &a.Check.Detail, &requested, &checked, &delivered, &a.LastEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return a, domain.ErrNotConfigured
	}
	if err != nil {
		return a, fmt.Errorf("reading github app: %w", err)
	}
	a.Check.Status = domain.CheckStatus(status)
	if a.UpdatedAt, err = parseTS(updated); err == nil {
		if a.Check.RequestedAt, err = parseNullTS(requested); err == nil {
			if a.Check.CheckedAt, err = parseNullTS(checked); err == nil {
				a.LastDeliveryAt, err = parseNullTS(delivered)
			}
		}
	}
	if err != nil {
		return a, fmt.Errorf("reading github app: %w", err)
	}
	return a, nil
}

// SaveGitHubApp inserts or replaces the App connection.
func (r *Repository) SaveGitHubApp(ctx context.Context, a domain.GitHubApp) error {
	status := a.Check.Status
	if status == "" {
		status = domain.CheckNone
	}
	_, err := r.conn(ctx).ExecContext(ctx, `INSERT INTO github_app(id, app_id, updated_at, check_status, check_id, check_code,
		check_detail, check_requested_at, checked_at, last_delivery_at, last_event) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET app_id = excluded.app_id, updated_at = excluded.updated_at,
		check_status = excluded.check_status, check_id = excluded.check_id, check_code = excluded.check_code,
		check_detail = excluded.check_detail, check_requested_at = excluded.check_requested_at, checked_at = excluded.checked_at,
		last_delivery_at = excluded.last_delivery_at, last_event = excluded.last_event`,
		a.AppID, ts(a.UpdatedAt), string(status), a.Check.ID, a.Check.Code, a.Check.Detail,
		nullTS(a.Check.RequestedAt), nullTS(a.Check.CheckedAt), nullTS(a.LastDeliveryAt), a.LastEvent)
	if err != nil {
		return fmt.Errorf("saving github app: %w", err)
	}
	return nil
}

// DeleteGitHubApp removes the App connection.
func (r *Repository) DeleteGitHubApp(ctx context.Context) error {
	if _, err := r.conn(ctx).ExecContext(ctx, "DELETE FROM github_app"); err != nil {
		return fmt.Errorf("deleting github app: %w", err)
	}
	return nil
}
