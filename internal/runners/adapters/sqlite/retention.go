package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

var _ app.RetentionRepo = (*Repository)(nil)

// outputKinds are the event kinds whose bodies count towards the output limits.
var outputKinds = []any{protocol.EventOutput, protocol.EventOutputDropped}

// OutputAttempts lists attempts with counted output last written before `before`, after the cursor in
// (last_at, step_id, fence) order.
func (r *Repository) OutputAttempts(ctx context.Context, before time.Time, after domain.AttemptOutput, limit int) ([]domain.AttemptOutput, error) {
	last := after.LastAt.UnixMilli()
	if after.LastAt.IsZero() {
		last = -1
	}
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT step_id, fence, bytes, last_at FROM runner_attempt_output
		WHERE last_at < ? AND (last_at, step_id, fence) > (?, ?, ?) ORDER BY last_at, step_id, fence LIMIT ?`,
		before.UnixMilli(), last, after.Step, after.Fence, limit)
	if err != nil {
		return nil, fmt.Errorf("listing attempt output: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.AttemptOutput
	for rows.Next() {
		var a domain.AttemptOutput
		var at int64
		if err := rows.Scan(&a.Step, &a.Fence, &a.Bytes, &at); err != nil {
			return nil, fmt.Errorf("reading attempt output: %w", err)
		}
		a.LastAt = time.UnixMilli(at).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAttemptOutput deletes up to limit output events of the attempt in one transaction and takes their bytes
// off the attempt's counter and the total; when the last ones are gone it removes the counter.
func (r *Repository) DeleteAttemptOutput(ctx context.Context, step string, fence int64, limit int) (int, bool, error) {
	var n int
	var done bool
	err := db.Transactor{DB: r.db}.InTx(ctx, func(ctx context.Context) error {
		conn := db.Conn(ctx, r.db)
		freed, rows, err := deleteOutputRows(ctx, conn, step, fence, limit)
		if err != nil {
			return err
		}
		n = rows
		done = n < limit
		if done {
			// The counter still holds every counted byte of the attempt, this batch included: all of it leaves the total.
			err := conn.QueryRowContext(ctx, `DELETE FROM runner_attempt_output WHERE step_id = ? AND fence = ?
				RETURNING bytes`, step, fence).Scan(&freed)
			if errors.Is(err, sql.ErrNoRows) {
				freed = 0 // no counter: nothing of it was counted
			} else if err != nil {
				return err
			}
		} else if _, err := conn.ExecContext(ctx, `UPDATE runner_attempt_output SET bytes = max(bytes - ?, 0)
				WHERE step_id = ? AND fence = ?`, freed, step, fence); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE runner_output_usage SET bytes = max(bytes - ?, 0) WHERE id = 1`, freed)
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("deleting attempt output: %w", err)
	}
	return n, done, nil
}

// deleteOutputRows deletes up to limit output events of the attempt and returns their body bytes and count.
func deleteOutputRows(ctx context.Context, conn db.Executor, step string, fence int64, limit int) (int64, int, error) {
	rows, err := conn.QueryContext(ctx, `DELETE FROM runner_attempt_events WHERE rowid IN (SELECT rowid
		FROM runner_attempt_events WHERE step_id = ? AND fence = ? AND kind IN (?, ?) LIMIT ?) RETURNING length(body)`,
		step, fence, outputKinds[0], outputKinds[1], limit)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	var freed int64
	n := 0
	for rows.Next() {
		var b *int64
		if err := rows.Scan(&b); err != nil {
			return 0, 0, err
		}
		n++
		if b != nil {
			freed += *b
		}
	}
	return freed, n, rows.Err()
}

// OldEvents lists events received before `before` that hold no counted output, after the cursor in
// (received_at, rowid) order.
func (r *Repository) OldEvents(ctx context.Context, before time.Time, after domain.EventRef, limit int) ([]domain.EventRef, error) {
	last := after.ReceivedAt.UnixMilli()
	if after.ReceivedAt.IsZero() {
		last = -1
	}
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT rowid, step_id, received_at FROM runner_attempt_events
		WHERE received_at < ? AND (received_at, rowid) > (?, ?) AND (body IS NULL OR kind NOT IN (?, ?))
		ORDER BY received_at, rowid LIMIT ?`, before.UnixMilli(), last, after.ID, outputKinds[0], outputKinds[1], limit)
	if err != nil {
		return nil, fmt.Errorf("listing old runner events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.EventRef
	for rows.Next() {
		var e domain.EventRef
		var at int64
		if err := rows.Scan(&e.ID, &e.Step, &at); err != nil {
			return nil, fmt.Errorf("reading old runner event: %w", err)
		}
		e.ReceivedAt = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEvents deletes the events (never counted output, so the counters stay) in one statement.
func (r *Repository) DeleteEvents(ctx context.Context, refs []domain.EventRef) (int64, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(refs)+2)
	for _, e := range refs {
		args = append(args, e.ID)
	}
	args = append(args, outputKinds...)
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `DELETE FROM runner_attempt_events WHERE rowid IN (?`+
		strings.Repeat(", ?", len(refs)-1)+`) AND (body IS NULL OR kind NOT IN (?, ?))`, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting runner events: %w", err)
	}
	return res.RowsAffected()
}

// OutputUsage is the total of counted output bytes.
func (r *Repository) OutputUsage(ctx context.Context) (int64, error) {
	var n int64
	if err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT bytes FROM runner_output_usage WHERE id = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("reading output usage: %w", err)
	}
	return n, nil
}
