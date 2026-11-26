package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// This file implements app.IntakeStore (KIN-27).

const bindingCols = "project_id, short_name, trigger_tag, poll_cursor, last_polled_at, auto_plan"

func scanBinding(row scanner) (domain.Binding, error) {
	var b domain.Binding
	var polled sql.NullString
	if err := row.Scan(&b.ProjectID, &b.ShortName, &b.TriggerTag, &b.Cursor, &polled, &b.AutoPlan); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b, domain.ErrNotFound
		}
		return b, fmt.Errorf("reading youtrack binding: %w", err)
	}
	var err error
	if b.LastPolledAt, err = parseNullTS(polled); err != nil {
		return b, fmt.Errorf("reading youtrack binding: %w", err)
	}
	return b, nil
}

// Bindings lists every project bound to a YouTrack project.
func (r *Repository) Bindings(ctx context.Context) ([]domain.Binding, error) {
	rows, err := r.conn(ctx).QueryContext(ctx, "SELECT "+bindingCols+" FROM project_youtrack ORDER BY short_name")
	if err != nil {
		return nil, fmt.Errorf("listing youtrack bindings: %w", err)
	}
	defer rows.Close()
	var out []domain.Binding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing youtrack bindings: %w", err)
	}
	return out, nil
}

// Binding returns a project's binding; domain.ErrNotFound when unbound.
func (r *Repository) Binding(ctx context.Context, projectID string) (domain.Binding, error) {
	return scanBinding(r.conn(ctx).QueryRowContext(ctx, "SELECT "+bindingCols+" FROM project_youtrack WHERE project_id = ?", projectID))
}

// BindingByShortName returns the binding of a YouTrack project; domain.ErrNotFound when none.
func (r *Repository) BindingByShortName(ctx context.Context, shortName string) (domain.Binding, error) {
	return scanBinding(r.conn(ctx).QueryRowContext(ctx, "SELECT "+bindingCols+" FROM project_youtrack WHERE short_name = ?", shortName))
}

// SetTriggerTag changes a binding's trigger tag; domain.ErrNotFound when unbound.
func (r *Repository) SetTriggerTag(ctx context.Context, projectID, tag string, at time.Time) error {
	res, err := r.conn(ctx).ExecContext(ctx, "UPDATE project_youtrack SET trigger_tag = ?, updated_at = ? WHERE project_id = ?",
		tag, ts(at), projectID)
	return affected(res, err, "saving trigger tag")
}

// SetAutoPlan turns a binding's automatic planning on or off; domain.ErrNotFound when unbound.
func (r *Repository) SetAutoPlan(ctx context.Context, projectID string, on bool, at time.Time) error {
	res, err := r.conn(ctx).ExecContext(ctx, "UPDATE project_youtrack SET auto_plan = ?, updated_at = ? WHERE project_id = ?",
		on, ts(at), projectID)
	return affected(res, err, "saving auto-planning")
}

// AdvanceCursor records a successful poll; the cursor never moves back, and a poll of a binding that was replaced
// meanwhile (another short name) changes nothing.
func (r *Repository) AdvanceCursor(ctx context.Context, projectID, shortName string, cursor int64, at time.Time) error {
	_, err := r.conn(ctx).ExecContext(ctx, `UPDATE project_youtrack SET poll_cursor = max(poll_cursor, ?), last_polled_at = ?
		WHERE project_id = ? AND short_name = ?`, cursor, ts(at), projectID, shortName)
	if err != nil {
		return fmt.Errorf("saving poll cursor: %w", err)
	}
	return nil
}

func affected(res sql.Result, err error, op string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// IntakeSettings returns the intake settings, or the defaults when never saved.
func (r *Repository) IntakeSettings(ctx context.Context) (domain.IntakeSettings, error) {
	var secs int
	var at string
	err := r.conn(ctx).QueryRowContext(ctx, "SELECT poll_interval_s, updated_at FROM youtrack_intake_settings WHERE id = 1").Scan(&secs, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DefaultIntakeSettings(), nil
	}
	if err != nil {
		return domain.IntakeSettings{}, fmt.Errorf("reading intake settings: %w", err)
	}
	s := domain.IntakeSettings{PollInterval: time.Duration(secs) * time.Second}
	if s.UpdatedAt, err = parseTS(at); err != nil {
		return s, fmt.Errorf("reading intake settings: %w", err)
	}
	return s, nil
}

// SaveIntakeSettings stores the intake settings.
func (r *Repository) SaveIntakeSettings(ctx context.Context, s domain.IntakeSettings) error {
	_, err := r.conn(ctx).ExecContext(ctx, `INSERT INTO youtrack_intake_settings(id, poll_interval_s, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET poll_interval_s = excluded.poll_interval_s, updated_at = excluded.updated_at`,
		int(s.PollInterval/time.Second), ts(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("saving intake settings: %w", err)
	}
	return nil
}

// UserLinks lists the YouTrack logins linked to accounts.
func (r *Repository) UserLinks(ctx context.Context) ([]domain.UserLink, error) {
	rows, err := r.conn(ctx).QueryContext(ctx, "SELECT youtrack_login, user_id, updated_at FROM youtrack_user_links ORDER BY youtrack_login")
	if err != nil {
		return nil, fmt.Errorf("listing youtrack logins: %w", err)
	}
	defer rows.Close()
	out := []domain.UserLink{}
	for rows.Next() {
		var l domain.UserLink
		var at string
		if err := rows.Scan(&l.YouTrackLogin, &l.UserID, &at); err != nil {
			return nil, fmt.Errorf("listing youtrack logins: %w", err)
		}
		if l.UpdatedAt, err = parseTS(at); err != nil {
			return nil, fmt.Errorf("listing youtrack logins: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing youtrack logins: %w", err)
	}
	return out, nil
}

// LinkedUser returns the account linked to a YouTrack login (case-insensitive); domain.ErrNotFound when none.
func (r *Repository) LinkedUser(ctx context.Context, youtrackLogin string) (string, error) {
	var id string
	err := r.conn(ctx).QueryRowContext(ctx, "SELECT user_id FROM youtrack_user_links WHERE youtrack_login = ?", youtrackLogin).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading youtrack login: %w", err)
	}
	return id, nil
}

// LinkUser links a YouTrack login to an account, replacing the account's previous link.
func (r *Repository) LinkUser(ctx context.Context, l domain.UserLink) error {
	return db.Transactor{DB: r.db}.InTx(ctx, func(ctx context.Context) error {
		if _, err := r.conn(ctx).ExecContext(ctx, "DELETE FROM youtrack_user_links WHERE user_id = ?", l.UserID); err != nil {
			return fmt.Errorf("linking youtrack login: %w", err)
		}
		_, err := r.conn(ctx).ExecContext(ctx, "INSERT INTO youtrack_user_links(youtrack_login, user_id, updated_at) VALUES (?, ?, ?)",
			l.YouTrackLogin, l.UserID, ts(l.UpdatedAt))
		switch {
		case db.IsUniqueViolation(err):
			return domain.ErrYouTrackLoginTaken
		case db.IsForeignKeyViolation(err):
			return domain.ErrNotFound
		case err != nil:
			return fmt.Errorf("linking youtrack login: %w", err)
		}
		return nil
	})
}

// UnlinkUser removes an account's YouTrack login.
func (r *Repository) UnlinkUser(ctx context.Context, userID string) error {
	if _, err := r.conn(ctx).ExecContext(ctx, "DELETE FROM youtrack_user_links WHERE user_id = ?", userID); err != nil {
		return fmt.Errorf("unlinking youtrack login: %w", err)
	}
	return nil
}

const intakeCols = "project_id, issue_id, issue_updated, outcome, reason, initiator, reporter, task_id, channel, processed_at"

func scanIntake(row scanner) (domain.IntakeRecord, error) {
	var rec domain.IntakeRecord
	var at string
	if err := row.Scan(&rec.ProjectID, &rec.IssueID, &rec.IssueUpdated, &rec.Outcome, &rec.Reason, &rec.Initiator,
		&rec.Reporter, &rec.TaskID, &rec.Channel, &at); err != nil {
		return rec, err
	}
	var err error
	if rec.ProcessedAt, err = parseTS(at); err != nil {
		return rec, fmt.Errorf("reading intake record: %w", err)
	}
	return rec, nil
}

// IntakeRecord returns the last intake decision for an issue in a project.
func (r *Repository) IntakeRecord(ctx context.Context, projectID, issueID string) (domain.IntakeRecord, bool, error) {
	rec, err := scanIntake(r.conn(ctx).QueryRowContext(ctx, "SELECT "+intakeCols+
		" FROM youtrack_intake WHERE project_id = ? AND issue_id = ?", projectID, issueID))
	if errors.Is(err, sql.ErrNoRows) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, fmt.Errorf("reading intake record: %w", err)
	}
	return rec, true, nil
}

// SaveIntakeRecord inserts or replaces the decision for an issue.
func (r *Repository) SaveIntakeRecord(ctx context.Context, rec domain.IntakeRecord) error {
	_, err := r.conn(ctx).ExecContext(ctx, `INSERT INTO youtrack_intake(`+intakeCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, issue_id) DO UPDATE SET issue_updated = excluded.issue_updated, outcome = excluded.outcome,
		reason = excluded.reason, initiator = excluded.initiator, reporter = excluded.reporter, task_id = excluded.task_id,
		channel = excluded.channel, processed_at = excluded.processed_at`,
		rec.ProjectID, rec.IssueID, rec.IssueUpdated, string(rec.Outcome), rec.Reason, rec.Initiator, rec.Reporter,
		rec.TaskID, rec.Channel, ts(rec.ProcessedAt))
	if err != nil {
		return fmt.Errorf("saving intake record: %w", err)
	}
	return nil
}

// RecentIntake lists a project's latest intake decisions, newest first.
func (r *Repository) RecentIntake(ctx context.Context, projectID string, limit int) ([]domain.IntakeRecord, error) {
	rows, err := r.conn(ctx).QueryContext(ctx, "SELECT "+intakeCols+
		" FROM youtrack_intake WHERE project_id = ? ORDER BY processed_at DESC, issue_id LIMIT ?", projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing intake records: %w", err)
	}
	defer rows.Close()
	out := []domain.IntakeRecord{}
	for rows.Next() {
		rec, err := scanIntake(rows)
		if err != nil {
			return nil, fmt.Errorf("listing intake records: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing intake records: %w", err)
	}
	return out, nil
}
