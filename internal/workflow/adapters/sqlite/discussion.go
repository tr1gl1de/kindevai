package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

var _ app.DiscussionRepository = (*Repository)(nil)

const messageCols = `id, task_id, role, author, plan_revision, text, status, reason, notice, revision, reply_to, run_id,
	agent, model, usage_json, work_ms, delivered_at, sent_at, created_at, updated_at, version`

func scanMessage(row scanner) (domain.DiscussionMessage, error) {
	var m domain.DiscussionMessage
	var usage, created, updated string
	var workMs int64
	var delivered, sent sql.NullString
	err := row.Scan(&m.ID, &m.TaskID, &m.Role, &m.Author, &m.PlanRevision, &m.Text, &m.Status, &m.Reason, &m.Notice,
		&m.Revision, &m.ReplyTo, &m.RunID, &m.Agent, &m.Model, &usage, &workMs, &delivered, &sent, &created, &updated, &m.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return m, domain.ErrNotFound
	}
	if err != nil {
		return m, fmt.Errorf("reading discussion message: %w", err)
	}
	m.Work = time.Duration(workMs) * time.Millisecond
	if err := json.Unmarshal([]byte(usage), &m.Usage); err != nil {
		return m, fmt.Errorf("discussion message %s usage: %w", m.ID, err)
	}
	var sentAt *time.Time
	if sentAt, err = parseNullTS(sent); err == nil && sentAt != nil {
		m.SentAt = *sentAt
	}
	if m.DeliveredAt, err = parseNullTS(delivered); err == nil {
		if m.CreatedAt, err = parseTS(created); err == nil {
			m.UpdatedAt, err = parseTS(updated)
		}
	}
	if err != nil {
		return m, fmt.Errorf("reading discussion message %s: %w", m.ID, err)
	}
	return m, nil
}

// CreateMessage inserts a message at version 1; domain.ErrConflict when its turn already has an answer.
func (r *Repository) CreateMessage(ctx context.Context, m *domain.DiscussionMessage) error {
	usage, err := json.Marshal(m.Usage)
	if err != nil {
		return fmt.Errorf("encoding the message usage: %w", err)
	}
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO plan_discussion_messages(`+messageCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		m.ID, m.TaskID, m.Role, m.Author, m.PlanRevision, m.Text, m.Status, m.Reason, m.Notice, m.Revision, m.ReplyTo,
		m.RunID, m.Agent, m.Model, string(usage), m.Work.Milliseconds(), nullTS(m.DeliveredAt), sentTS(m), ts(m.CreatedAt), ts(m.UpdatedAt))
	switch {
	case db.IsUniqueViolation(err):
		return domain.ErrConflict
	case db.IsForeignKeyViolation(err):
		return fmt.Errorf("task %s: %w", m.TaskID, domain.ErrNotFound)
	case err != nil:
		return fmt.Errorf("creating discussion message: %w", err)
	}
	if m.Role == domain.RoleAssistant { // an answer's CLI time is the task's agent time (KIN-75 aggregate)
		if err := refreshAgentTime(ctx, db.Conn(ctx, r.db), m.TaskID); err != nil {
			return err
		}
	}
	m.Version = 1
	return nil
}

// sentTS is a user message's last send time (NULL for answers and notices).
func sentTS(m *domain.DiscussionMessage) any {
	if m.SentAt.IsZero() {
		return nil
	}
	return ts(m.SentAt)
}

// Message loads a message.
func (r *Repository) Message(ctx context.Context, id string) (domain.DiscussionMessage, error) {
	return scanMessage(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+messageCols+" FROM plan_discussion_messages WHERE id = ?", id))
}

// SaveMessage writes a message's mutable fields if its version is unchanged and increments the version.
func (r *Repository) SaveMessage(ctx context.Context, m *domain.DiscussionMessage) error {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE plan_discussion_messages SET status = ?, reason = ?, run_id = ?,
		delivered_at = ?, sent_at = ?, updated_at = ?, version = version + 1 WHERE id = ? AND version = ?`,
		m.Status, m.Reason, m.RunID, nullTS(m.DeliveredAt), sentTS(m), ts(m.UpdatedAt), m.ID, m.Version)
	if err := oneRow(res, err, "saving discussion message"); err != nil {
		return err
	}
	m.Version++
	return nil
}

// Messages returns the task's newest limit messages, oldest first.
func (r *Repository) Messages(ctx context.Context, taskID string, limit int) ([]domain.DiscussionMessage, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT `+messageCols+` FROM (SELECT rowid AS rid, * FROM
		plan_discussion_messages WHERE task_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?) ORDER BY created_at, rid`,
		taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing discussion messages: %w", err)
	}
	defer rows.Close()
	out := []domain.DiscussionMessage{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountUserMessages counts the task's user messages.
func (r *Repository) CountUserMessages(ctx context.Context, taskID string) (int, error) {
	var n int
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT COUNT(*) FROM plan_discussion_messages WHERE task_id = ?
		AND role = 'user'`, taskID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting discussion messages: %w", err)
	}
	return n, nil
}

// UserMessageTimes returns when the user wrote messages since `since`.
func (r *Repository) UserMessageTimes(ctx context.Context, userID string, since time.Time) ([]time.Time, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT sent_at FROM plan_discussion_messages WHERE role = 'user'
		AND author = ? AND sent_at >= ? ORDER BY sent_at`, userID, ts(since))
	if err != nil {
		return nil, fmt.Errorf("listing the user's discussion messages: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("reading a message time: %w", err)
		}
		at, err := parseTS(s)
		if err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, rows.Err()
}

// DiscussionLimits returns the limits the Owner set, or the defaults.
func (r *Repository) DiscussionLimits(ctx context.Context) (domain.DiscussionLimits, error) {
	var l domain.DiscussionLimits
	var secs int64
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT max_message_chars, max_messages_per_plan, max_messages_per_hour,
		answer_timeout_seconds FROM plan_discussion_settings WHERE id = 1`).Scan(&l.MaxMessageChars, &l.MaxMessagesPerPlan,
		&l.MaxMessagesPerHour, &secs)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DefaultDiscussionLimits(), nil
	}
	if err != nil {
		return l, fmt.Errorf("reading discussion limits: %w", err)
	}
	l.AnswerTimeout = time.Duration(secs) * time.Second
	return l, nil
}

// SetDiscussionLimits stores the limits.
func (r *Repository) SetDiscussionLimits(ctx context.Context, l domain.DiscussionLimits, by string, now time.Time) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO plan_discussion_settings(id, max_message_chars,
		max_messages_per_plan, max_messages_per_hour, answer_timeout_seconds, updated_by, updated_at) VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET max_message_chars = excluded.max_message_chars,
		max_messages_per_plan = excluded.max_messages_per_plan, max_messages_per_hour = excluded.max_messages_per_hour,
		answer_timeout_seconds = excluded.answer_timeout_seconds, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		l.MaxMessageChars, l.MaxMessagesPerPlan, l.MaxMessagesPerHour, int64(l.AnswerTimeout/time.Second), by, ts(now))
	if err != nil {
		return fmt.Errorf("saving discussion limits: %w", err)
	}
	return nil
}
