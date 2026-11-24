package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

var _ app.ChannelRepo = (*Repository)(nil)

// EnsureChannel creates the runner's channel row with epoch unless it exists.
func (r *Repository) EnsureChannel(ctx context.Context, runnerID, epoch string) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx,
		`INSERT INTO runner_channels(runner_id, command_epoch) VALUES (?, ?) ON CONFLICT(runner_id) DO NOTHING`,
		runnerID, epoch); err != nil {
		if db.IsForeignKeyViolation(err) {
			return fmt.Errorf("creating runner channel: %w", domain.ErrNotFound)
		}
		return fmt.Errorf("creating runner channel: %w", err)
	}
	return nil
}

// Channel returns the runner's channel position; false when it has none yet.
func (r *Repository) Channel(ctx context.Context, runnerID string) (domain.Channel, bool, error) {
	var c domain.Channel
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT command_epoch, command_seq, command_acked, event_epoch, event_last
		FROM runner_channels WHERE runner_id = ?`, runnerID).
		Scan(&c.CommandEpoch, &c.CommandSeq, &c.CommandAcked, &c.EventEpoch, &c.EventLast)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Channel{}, false, nil
	}
	if err != nil {
		return domain.Channel{}, false, fmt.Errorf("loading runner channel: %w", err)
	}
	return c, true, nil
}

// AppendCommand gives the command the next seq unless its id is already in the log. Run it in a transaction.
func (r *Repository) AppendCommand(ctx context.Context, runnerID string, q domain.QueuedCommand, now time.Time) (int64, error) {
	conn := db.Conn(ctx, r.db)
	var seq int64
	err := conn.QueryRowContext(ctx, `SELECT seq FROM runner_commands WHERE runner_id = ? AND command_id = ?`,
		runnerID, q.ID).Scan(&seq)
	if err == nil {
		return seq, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("finding runner command: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `UPDATE runner_channels SET command_seq = command_seq + 1 WHERE runner_id = ?
		RETURNING command_seq`, runnerID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("numbering runner command: %w", err)
	}
	body := q.Body
	if body == nil {
		body = []byte{}
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO runner_commands(runner_id, seq, command_id, kind, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, runnerID, seq, q.ID, q.Kind, body, now.UnixMilli()); err != nil {
		return 0, fmt.Errorf("storing runner command: %w", err)
	}
	return seq, nil
}

// CommandsAfter lists unacknowledged commands after seq in order.
func (r *Repository) CommandsAfter(ctx context.Context, runnerID string, after int64, limit int) ([]domain.QueuedCommand, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT seq, command_id, kind, body FROM runner_commands
		WHERE runner_id = ? AND seq > ? ORDER BY seq LIMIT ?`, runnerID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("listing runner commands: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.QueuedCommand
	for rows.Next() {
		var q domain.QueuedCommand
		if err := rows.Scan(&q.Seq, &q.ID, &q.Kind, &q.Body); err != nil {
			return nil, fmt.Errorf("reading runner command: %w", err)
		}
		if len(q.Body) == 0 {
			q.Body = nil
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// AckCommands moves the cumulative ACK forward (never back, never past the log) and purges commands acknowledged
// more than domain.AckedCommandRetention ago.
func (r *Repository) AckCommands(ctx context.Context, runnerID string, seq int64, now time.Time) error {
	return db.Transactor{DB: r.db}.InTx(ctx, func(ctx context.Context) error {
		conn := db.Conn(ctx, r.db)
		if _, err := conn.ExecContext(ctx, `UPDATE runner_channels SET command_acked = ?
			WHERE runner_id = ? AND command_acked < ? AND ? <= command_seq`, seq, runnerID, seq, seq); err != nil {
			return fmt.Errorf("acknowledging runner commands: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE runner_commands SET acked_at = ?
			WHERE runner_id = ? AND seq <= ? AND acked_at IS NULL`, now.UnixMilli(), runnerID, seq); err != nil {
			return fmt.Errorf("acknowledging runner commands: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM runner_commands WHERE acked_at IS NOT NULL AND acked_at < ?`,
			now.Add(-domain.AckedCommandRetention).UnixMilli()); err != nil {
			return fmt.Errorf("purging acknowledged runner commands: %w", err)
		}
		return nil
	})
}

// SaveEventCursor stores the position in the runner's event stream.
func (r *Repository) SaveEventCursor(ctx context.Context, runnerID, epoch string, last int64) error {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE runner_channels SET event_epoch = ?, event_last = ?
		WHERE runner_id = ?`, epoch, last, runnerID)
	if err != nil {
		return fmt.Errorf("saving runner event cursor: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("saving runner event cursor: %w", domain.ErrNotFound)
	}
	return nil
}

// EventSeen reports whether the runner's attempt event is recorded.
func (r *Repository) EventSeen(ctx context.Context, runnerID, step string, fence, attemptSeq int64) (bool, error) {
	var one int
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT 1 FROM runner_attempt_events
		WHERE runner_id = ? AND step_id = ? AND fence = ? AND attempt_seq = ?`, runnerID, step, fence, attemptSeq).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("finding runner event: %w", err)
	}
	return true, nil
}

// RecordEvent stores an attempt event.
func (r *Repository) RecordEvent(ctx context.Context, e domain.AttemptEvent) error {
	var body any
	if e.Body != nil {
		body = e.Body
	}
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO runner_attempt_events(step_id, fence, attempt_seq,
		runner_id, kind, body, outcome, reason, runner_at, received_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Step, e.Fence, e.AttemptSeq, e.RunnerID, e.Kind, body, e.Outcome, e.Reason, e.RunnerAt.UnixMilli(),
		e.ReceivedAt.UnixMilli()); err != nil {
		return fmt.Errorf("recording runner event: %w", err)
	}
	return nil
}

// AddOutput counts n bytes of the attempt's output if the total stays within max.
func (r *Repository) AddOutput(ctx context.Context, step string, fence, n, max int64) (bool, error) {
	conn := db.Conn(ctx, r.db)
	if n > max {
		return false, nil
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO runner_attempt_output(step_id, fence, bytes) VALUES (?, ?, 0)
		ON CONFLICT(step_id, fence) DO NOTHING`, step, fence); err != nil {
		return false, fmt.Errorf("counting attempt output: %w", err)
	}
	res, err := conn.ExecContext(ctx, `UPDATE runner_attempt_output SET bytes = bytes + ?
		WHERE step_id = ? AND fence = ? AND bytes + ? <= ?`, n, step, fence, n, max)
	if err != nil {
		return false, fmt.Errorf("counting attempt output: %w", err)
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}
