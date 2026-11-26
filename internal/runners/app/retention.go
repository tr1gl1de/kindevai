package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// RetentionRepo finds and deletes recorded runner events. Each call is one short statement or transaction of its
// own, so a pass never holds SQLite's single writer for long.
type RetentionRepo interface {
	// OutputAttempts lists attempts with counted output whose last output arrived before `before`, ordered by
	// (LastAt, Step, Fence) and starting after the cursor.
	OutputAttempts(ctx context.Context, before time.Time, after domain.AttemptOutput, limit int) ([]domain.AttemptOutput, error)
	// DeleteAttemptOutput deletes up to limit output events of the attempt and takes their bytes off the counters;
	// done when no output event is left (the attempt's counter is then removed).
	DeleteAttemptOutput(ctx context.Context, step string, fence int64, limit int) (rows int, done bool, err error)
	// OldEvents lists events received before `before` that hold no counted output, ordered by (ReceivedAt, ID)
	// and starting after the cursor.
	OldEvents(ctx context.Context, before time.Time, after domain.EventRef, limit int) ([]domain.EventRef, error)
	DeleteEvents(ctx context.Context, refs []domain.EventRef) (int64, error)
	// OutputUsage is the total of counted output bytes.
	OutputUsage(ctx context.Context) (int64, error)
	// DeleteRevokedCommands deletes up to limit commands, acknowledged or not, from the logs of revoked runners
	// (KIN-72) and returns how many it deleted.
	DeleteRevokedCommands(ctx context.Context, limit int) (int64, error)
}

// FinishedSteps is the workflow as the retention needs it: of the given steps, those whose execution finished
// (with the time it finished; the zero time for a step that no longer exists). Steps of a running execution are
// absent, and their records are never deleted.
type FinishedSteps interface {
	FinishedSteps(ctx context.Context, stepIDs []string) (map[string]time.Time, error)
}

// RetentionBatch is the most rows one retention statement reads or deletes; output (up to 56 KiB a row) is
// deleted half as many rows at a time.
const (
	RetentionBatch = 256
	outputBatch    = RetentionBatch / 2
	attemptBatch   = 64
)

// RetentionReport counts what one pass deleted.
type RetentionReport struct {
	OutputAttempts int   // attempts whose output was deleted
	OutputBytes    int64 // their output bytes
	Events         int64 // other events deleted
	// RevokedCommands: commands deleted from the logs of revoked runners (KIN-72).
	RevokedCommands int64
}

// Retention deletes recorded runner events by the policy (KIN-52). Records of a running execution are kept.
type Retention struct {
	Repo   RetentionRepo
	Steps  FinishedSteps
	Policy domain.RetentionPolicy
	Now    func() time.Time
	Logger *slog.Logger
	// Pause between batches lets other writers in (zero: none).
	Pause time.Duration
}

func (r *Retention) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

func (r *Retention) pause(ctx context.Context) error {
	if r.Pause <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(r.Pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Purge runs one pass: the command logs of revoked runners, expired output, then the output limit, then expired
// events.
func (r *Retention) Purge(ctx context.Context) (RetentionReport, error) {
	var rep RetentionReport
	now := r.Now()
	outputCutoff, eventCutoff := now.Add(-r.Policy.Output), now.Add(-r.Policy.Events)
	err := r.purgeRevokedCommands(ctx, &rep)
	if err == nil {
		err = r.purgeOutput(ctx, &rep, outputCutoff, func(finished time.Time) bool { return domain.Expired(finished, outputCutoff) }, nil)
	}
	if err == nil {
		err = r.enforceLimit(ctx, &rep, now)
	}
	if err == nil {
		err = r.purgeEvents(ctx, &rep, eventCutoff)
	}
	if rep != (RetentionReport{}) {
		r.logger().Info("runner events purged", "output_attempts", rep.OutputAttempts, "output_bytes", rep.OutputBytes,
			"events", rep.Events, "revoked_commands", rep.RevokedCommands)
	}
	return rep, err
}

// RetentionInterval is how often Run purges (it also purges at start).
const RetentionInterval = time.Hour

// Run purges at start and then every RetentionInterval until ctx ends; a failed pass is logged and retried at the
// next tick (each batch it committed stays deleted).
func (r *Retention) Run(ctx context.Context) {
	tick := time.NewTicker(RetentionInterval)
	defer tick.Stop()
	for {
		if _, err := r.Purge(ctx); err != nil && ctx.Err() == nil {
			r.logger().Warn("runner event retention", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// purgeRevokedCommands empties the command logs of revoked runners (KIN-72). Revocation is final (KIN-17): the
// runner never connects again, so its commands, acknowledged or not, can never be delivered, and their bodies may
// hold sealed proxy passwords (runner.config.update, KIN-30). Nothing else reads the log, and KIN-63 already ended
// the runner's attempts in the revocation's transaction, so deleting it settles nothing.
func (r *Retention) purgeRevokedCommands(ctx context.Context, rep *RetentionReport) error {
	for {
		n, err := r.Repo.DeleteRevokedCommands(ctx, outputBatch)
		if err != nil {
			return err
		}
		rep.RevokedCommands += n
		if n < outputBatch {
			return nil
		}
		if err := r.pause(ctx); err != nil {
			return err
		}
	}
}

// purgeOutput deletes the output of attempts last written before `before` whose execution finished at a
// time deletable accepts; stop ends the pass early (checked after each attempt).
func (r *Retention) purgeOutput(ctx context.Context, rep *RetentionReport, before time.Time, deletable func(time.Time) bool,
	stop func() (bool, error)) error {
	var cur domain.AttemptOutput
	for {
		page, err := r.Repo.OutputAttempts(ctx, before, cur, attemptBatch)
		if err != nil || len(page) == 0 {
			return err
		}
		cur = page[len(page)-1]
		finished, err := finishedOf(ctx, r.Steps, page, func(a domain.AttemptOutput) string { return a.Step })
		if err != nil {
			return err
		}
		for _, a := range page {
			at, ok := finished[a.Step]
			if !ok || !deletable(at) {
				continue
			}
			if err := r.deleteOutput(ctx, a); err != nil {
				return err
			}
			rep.OutputAttempts++
			rep.OutputBytes += a.Bytes
			if err := r.pause(ctx); err != nil {
				return err
			}
			if stop != nil {
				if done, err := stop(); done || err != nil {
					return err
				}
			}
		}
		if len(page) < attemptBatch {
			return nil
		}
	}
}

func (r *Retention) deleteOutput(ctx context.Context, a domain.AttemptOutput) error {
	for {
		_, done, err := r.Repo.DeleteAttemptOutput(ctx, a.Step, a.Fence, outputBatch)
		if err != nil || done {
			return err
		}
		if err := r.pause(ctx); err != nil {
			return err
		}
	}
}

// enforceLimit evicts the oldest output of finished executions while more than the limit is stored, down to 90%
// of it, so the limit is not hit again right away.
func (r *Retention) enforceLimit(ctx context.Context, rep *RetentionReport, now time.Time) error {
	used, err := r.Repo.OutputUsage(ctx)
	if err != nil || used <= r.Policy.MaxOutputBytes {
		return err
	}
	target := r.Policy.MaxOutputBytes / 10 * 9
	before := rep.OutputBytes
	err = r.purgeOutput(ctx, rep, now.Add(time.Millisecond), func(time.Time) bool { return true }, func() (bool, error) {
		used, err = r.Repo.OutputUsage(ctx)
		return used <= target, err
	})
	if err != nil {
		return err
	}
	r.logger().Warn("runner output over the limit: evicted the oldest output of finished executions",
		"limit_bytes", r.Policy.MaxOutputBytes, "evicted_bytes", rep.OutputBytes-before, "stored_bytes", used)
	if used > r.Policy.MaxOutputBytes {
		r.logger().Warn("runner output storage full: only running executions hold output; new output is dropped until they finish",
			"limit_bytes", r.Policy.MaxOutputBytes, "stored_bytes", used)
	}
	return nil
}

// purgeEvents deletes events without counted output received before cutoff whose execution finished before it.
func (r *Retention) purgeEvents(ctx context.Context, rep *RetentionReport, cutoff time.Time) error {
	var cur domain.EventRef
	for {
		page, err := r.Repo.OldEvents(ctx, cutoff, cur, RetentionBatch)
		if err != nil || len(page) == 0 {
			return err
		}
		cur = page[len(page)-1]
		finished, err := finishedOf(ctx, r.Steps, page, func(e domain.EventRef) string { return e.Step })
		if err != nil {
			return err
		}
		var del []domain.EventRef
		for _, e := range page {
			if at, ok := finished[e.Step]; ok && domain.Expired(at, cutoff) {
				del = append(del, e)
			}
		}
		if len(del) > 0 {
			n, err := r.Repo.DeleteEvents(ctx, del)
			if err != nil {
				return err
			}
			rep.Events += n
		}
		if len(page) < RetentionBatch {
			return nil
		}
		if err := r.pause(ctx); err != nil {
			return err
		}
	}
}

// finishedOf asks the workflow about the distinct steps of a page.
func finishedOf[T any](ctx context.Context, steps FinishedSteps, page []T, step func(T) string) (map[string]time.Time, error) {
	seen := map[string]bool{}
	var ids []string
	for _, x := range page {
		if id := step(x); !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return steps.FinishedSteps(ctx, ids)
}
