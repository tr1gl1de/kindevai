package app

import (
	"context"
	"slices"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// MaxEventPage bounds one page of an attempt's events in the task API (output events carry ≤8 KiB each).
const MaxEventPage = 32

// AttemptEvent is one recorded runner event of an attempt as the task UI shows it: lifecycle, an output chunk
// (Stream, Offset, Data), dropped output (Bytes) or a failure (Reason). Omitted: the body was not stored (output
// over the attempt's or the platform's limit, or removed by retention).
type AttemptEvent struct {
	Seq     int64
	Kind    string
	At      time.Time // the runner's clock
	Stream  string
	Offset  int64
	Data    string
	Bytes   int64
	Reason  string
	Omitted bool
}

// AttemptLog is the runners context's record of attempt events (KIN-22/52).
type AttemptLog interface {
	// AttemptEvents returns at most limit events of the attempt (step, fence) after the seq cursor, in order.
	AttemptEvents(ctx context.Context, stepID string, fence, after int64, limit int) ([]AttemptEvent, error)
}

// EventPage is a page of an attempt's events; Next is the cursor for the following page, More whether the page
// was full (ask again right away).
type EventPage struct {
	Events []AttemptEvent
	Next   int64
	More   bool
}

// AttemptEvents returns a page of the events of the task's step attempt with fence after the cursor, for anyone
// who may see the task (domain.ErrNotFound otherwise, also when the step or attempt is not the task's).
func (s *Service) AttemptEvents(ctx context.Context, p auth.Principal, taskID, stepID string, fence, after int64) (EventPage, error) {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return EventPage{}, err
	}
	v, err := s.visible(ctx, p)
	if err != nil {
		return EventPage{}, err
	}
	if _, ok := v[t.ProjectID]; !ok {
		return EventPage{}, domain.ErrNotFound
	}
	e, err := s.Repo.ExecutionByStep(ctx, stepID)
	if err != nil {
		return EventPage{}, err
	}
	st := e.Step(stepID)
	if e.TaskID != t.ID || st == nil || !slices.ContainsFunc(st.Attempts, func(a domain.Attempt) bool { return a.Fence == fence }) {
		return EventPage{}, domain.ErrNotFound
	}
	page := EventPage{Events: []AttemptEvent{}, Next: max(after, 0)}
	if s.Log == nil {
		return page, nil
	}
	evs, err := s.Log.AttemptEvents(ctx, stepID, fence, page.Next, MaxEventPage)
	if err != nil {
		return EventPage{}, err
	}
	if len(evs) > MaxEventPage {
		evs = evs[:MaxEventPage]
	}
	page.Events = append(page.Events, evs...)
	if n := len(evs); n > 0 {
		page.Next = evs[n-1].Seq
	}
	page.More = len(evs) == MaxEventPage
	return page, nil
}
