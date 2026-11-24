package app

import (
	"context"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// MaxAttemptLogPage bounds one page of an attempt's events (output events carry ≤8 KiB, so a page is ≤256 KiB).
const MaxAttemptLogPage = 32

// AttemptLogRepo reads recorded attempt events.
type AttemptLogRepo interface {
	// AttemptEvents returns the applied events of the attempt (step, fence) with attempt_seq > after, in order.
	AttemptEvents(ctx context.Context, step string, fence, after int64, limit int) ([]domain.AttemptEvent, error)
}

// AttemptLog reads an attempt's recorded runner events for the task UI (KIN-25): events, output and failures in
// the order the runner sent them, in bounded pages after a cursor. Only applied events are returned; access is
// checked by the caller (the workflow's task API).
type AttemptLog struct{ Repo AttemptLogRepo }

// Events returns at most min(limit, MaxAttemptLogPage) events of the attempt after the cursor. Output and failure
// reasons are redacted again (KIN-77): ingest already masked them, this covers events stored before that.
func (l AttemptLog) Events(ctx context.Context, step string, fence, after int64, limit int) ([]domain.AttemptEvent, error) {
	limit = min(max(limit, 1), MaxAttemptLogPage)
	evs, err := l.Repo.AttemptEvents(ctx, step, fence, max(after, 0), limit)
	for i := range evs {
		if evs[i].Body != nil {
			evs[i].Body = redactBody(evs[i].Kind, evs[i].Body)
		}
	}
	return evs, err
}
