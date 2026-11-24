package app

import (
	"context"
	"log/slog"
	"time"
)

// Retention of closed alerts (KIN-60): DISMISSED and RESOLVED alerts are deleted AlertRetention after they
// closed; active alerts are never deleted.
const (
	AlertRetention    = 90 * 24 * time.Hour
	RetentionBatch    = 256 // most alerts one statement deletes, so a pass never holds SQLite's writer for long
	RetentionInterval = time.Hour
)

// ClosedAlerts deletes closed alerts; each call is one short statement of its own.
type ClosedAlerts interface {
	// DeleteClosed deletes up to limit alerts closed before `before` and returns how many it deleted.
	DeleteClosed(ctx context.Context, before time.Time, limit int) (int64, error)
}

// Retention purges closed alerts older than AlertRetention.
type Retention struct {
	Repo   ClosedAlerts
	Now    func() time.Time
	Logger *slog.Logger
	Pause  time.Duration // between batches, lets other writers in (zero: none)
}

func (r *Retention) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

// Purge runs one pass in batches of RetentionBatch until no expired closed alert is left.
func (r *Retention) Purge(ctx context.Context) (int64, error) {
	cutoff := r.Now().Add(-AlertRetention)
	var total int64
	for {
		n, err := r.Repo.DeleteClosed(ctx, cutoff, RetentionBatch)
		total += n
		if err != nil || n < RetentionBatch {
			if total > 0 {
				r.logger().Info("closed alerts purged", "alerts", total)
			}
			return total, err
		}
		if err := pause(ctx, r.Pause); err != nil {
			return total, err
		}
	}
}

// Run purges at start and then every RetentionInterval until ctx ends; a failed pass is retried at the next tick.
func (r *Retention) Run(ctx context.Context) {
	tick := time.NewTicker(RetentionInterval)
	defer tick.Stop()
	for {
		if _, err := r.Purge(ctx); err != nil && ctx.Err() == nil {
			r.logger().Warn("alert retention", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
