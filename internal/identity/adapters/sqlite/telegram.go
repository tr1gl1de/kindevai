package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// TelegramRepository stores Telegram identities, link requests and the Mini App settings.
type TelegramRepository struct{ db *sql.DB }

// NewTelegramRepository returns a repository on d.
func NewTelegramRepository(d *sql.DB) *TelegramRepository { return &TelegramRepository{db: d} }

func scanIdentity(row rowScanner) (domain.TelegramIdentity, error) {
	var id domain.TelegramIdentity
	var linked string
	err := row.Scan(&id.UserID, &id.TelegramID, &id.Username, &linked)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TelegramIdentity{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.TelegramIdentity{}, fmt.Errorf("reading Telegram identity: %w", err)
	}
	if id.LinkedAt, err = parseTS(linked); err != nil {
		return domain.TelegramIdentity{}, fmt.Errorf("reading Telegram identity: %w", err)
	}
	return id, nil
}

const identityCols = "user_id, telegram_id, username, linked_at"

// IdentityByTelegramID finds the link of a Telegram user.
func (r *TelegramRepository) IdentityByTelegramID(ctx context.Context, telegramID int64) (domain.TelegramIdentity, error) {
	return scanIdentity(db.Conn(ctx, r.db).QueryRowContext(ctx,
		"SELECT "+identityCols+" FROM telegram_identities WHERE telegram_id = ?", telegramID))
}

// IdentityByUser finds the link of an account.
func (r *TelegramRepository) IdentityByUser(ctx context.Context, userID string) (domain.TelegramIdentity, error) {
	return scanIdentity(db.Conn(ctx, r.db).QueryRowContext(ctx,
		"SELECT "+identityCols+" FROM telegram_identities WHERE user_id = ?", userID))
}

// CreateIdentity inserts a link; the unique constraints are the last line against duplicates.
func (r *TelegramRepository) CreateIdentity(ctx context.Context, id domain.TelegramIdentity) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx,
		"INSERT INTO telegram_identities(user_id, telegram_id, username, linked_at) VALUES (?, ?, ?, ?)",
		id.UserID, id.TelegramID, id.Username, ts(id.LinkedAt))
	switch {
	case err == nil:
		return nil
	case db.IsUniqueViolation(err) && strings.Contains(err.Error(), "telegram_identities.telegram_id"):
		return domain.ErrTelegramTaken
	case db.IsUniqueViolation(err):
		return domain.ErrUserHasTelegram
	case db.IsForeignKeyViolation(err):
		return domain.ErrNotFound
	}
	return fmt.Errorf("creating Telegram identity: %w", err)
}

// DeleteIdentity removes an account's link.
func (r *TelegramRepository) DeleteIdentity(ctx context.Context, userID string) (bool, error) {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM telegram_identities WHERE user_id = ?", userID)
	if err != nil {
		return false, fmt.Errorf("deleting Telegram identity: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const linkCols = "id, token_hash, user_id, session_hash, created_at, expires_at, telegram_id, telegram_username, " +
	"telegram_name, code_hash, attempts, consumed_at, cancelled_at"

func scanLink(row rowScanner) (domain.LinkRequest, error) {
	var l domain.LinkRequest
	var created, expires, tgName string
	var tgID sql.NullInt64
	var consumed, cancelled sql.NullString
	err := row.Scan(&l.ID, &l.TokenHash, &l.UserID, &l.SessionHash, &created, &expires, &tgID, &l.Telegram.Username,
		&tgName, &l.CodeHash, &l.Attempts, &consumed, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.LinkRequest{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.LinkRequest{}, fmt.Errorf("reading link request: %w", err)
	}
	l.Telegram.ID, l.Telegram.FirstName = tgID.Int64, tgName
	if l.CreatedAt, err = parseTS(created); err != nil {
		return domain.LinkRequest{}, fmt.Errorf("reading link request: %w", err)
	}
	if l.ExpiresAt, err = parseTS(expires); err != nil {
		return domain.LinkRequest{}, fmt.Errorf("reading link request: %w", err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{{consumed, &l.ConsumedAt}, {cancelled, &l.CancelledAt}} {
		if f.src.Valid {
			t, err := parseTS(f.src.String)
			if err != nil {
				return domain.LinkRequest{}, fmt.Errorf("reading link request: %w", err)
			}
			*f.dst = &t
		}
	}
	return l, nil
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// tgName keeps the non-username part of the Telegram display name (FirstName holds it after a reload).
func tgName(u domain.TelegramUser) string { return strings.TrimSpace(u.FirstName + " " + u.LastName) }

// CreateLinkRequest inserts a new request.
func (r *TelegramRepository) CreateLinkRequest(ctx context.Context, l domain.LinkRequest) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO telegram_link_requests(id, token_hash, user_id,
		session_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		l.ID, l.TokenHash, l.UserID, l.SessionHash, ts(l.CreatedAt), ts(l.ExpiresAt))
	if err != nil {
		return fmt.Errorf("creating link request: %w", err)
	}
	return nil
}

// LinkRequestByTokenHash loads a request by its token hash.
func (r *TelegramRepository) LinkRequestByTokenHash(ctx context.Context, hash []byte) (domain.LinkRequest, error) {
	return scanLink(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+linkCols+" FROM telegram_link_requests WHERE token_hash = ?", hash))
}

// LatestLinkRequest loads the user's newest request.
func (r *TelegramRepository) LatestLinkRequest(ctx context.Context, userID string) (domain.LinkRequest, error) {
	return scanLink(db.Conn(ctx, r.db).QueryRowContext(ctx,
		"SELECT "+linkCols+" FROM telegram_link_requests WHERE user_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1", userID))
}

// SaveLinkRequest stores the mutable state of a request.
func (r *TelegramRepository) SaveLinkRequest(ctx context.Context, l domain.LinkRequest) error {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE telegram_link_requests SET telegram_id = ?,
		telegram_username = ?, telegram_name = ?, code_hash = ?, attempts = ?, consumed_at = ?, cancelled_at = ?
		WHERE id = ?`, nullID(l.Telegram.ID), l.Telegram.Username, tgName(l.Telegram), l.CodeHash, l.Attempts,
		nullTS(l.ConsumedAt), nullTS(l.CancelledAt), l.ID)
	if err != nil {
		return fmt.Errorf("saving link request: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// CancelOpenLinkRequests cancels the user's requests that are neither used nor cancelled.
func (r *TelegramRepository) CancelOpenLinkRequests(ctx context.Context, userID string, now time.Time) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE telegram_link_requests SET cancelled_at = ?
		WHERE user_id = ? AND consumed_at IS NULL AND cancelled_at IS NULL`, ts(now), userID); err != nil {
		return fmt.Errorf("cancelling link requests: %w", err)
	}
	return nil
}

// DeleteLinkRequestsBefore removes requests that expired before cutoff (they keep Telegram names).
func (r *TelegramRepository) DeleteLinkRequestsBefore(ctx context.Context, cutoff time.Time) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM telegram_link_requests WHERE expires_at < ?", ts(cutoff)); err != nil {
		return fmt.Errorf("purging link requests: %w", err)
	}
	return nil
}

// Settings loads the Mini App settings.
func (r *TelegramRepository) Settings(ctx context.Context) (domain.BotSettings, time.Time, error) {
	var s domain.BotSettings
	var updated string
	err := db.Conn(ctx, r.db).QueryRowContext(ctx,
		"SELECT bot_username, app_short_name, updated_at FROM telegram_bot_settings WHERE id = 1").
		Scan(&s.BotUsername, &s.AppShortName, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.BotSettings{}, time.Time{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.BotSettings{}, time.Time{}, fmt.Errorf("reading Telegram settings: %w", err)
	}
	at, err := parseTS(updated)
	if err != nil {
		return domain.BotSettings{}, time.Time{}, fmt.Errorf("reading Telegram settings: %w", err)
	}
	return s, at, nil
}

// SaveSettings upserts the Mini App settings.
func (r *TelegramRepository) SaveSettings(ctx context.Context, s domain.BotSettings, now time.Time) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO telegram_bot_settings(id, bot_username, app_short_name, updated_at)
		VALUES (1, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET bot_username = excluded.bot_username,
		app_short_name = excluded.app_short_name, updated_at = excluded.updated_at`, s.BotUsername, s.AppShortName, ts(now)); err != nil {
		return fmt.Errorf("saving Telegram settings: %w", err)
	}
	return nil
}
