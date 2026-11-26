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
	// AttemptWorkspace returns the attempt's latest workspace reports (KIN-29); nil where the runner sent none.
	AttemptWorkspace(ctx context.Context, stepID string, fence int64) (AttemptWorkspace, error)
}

// InstructionFile is a repository-native instruction file the runner found in the attempt's worktree: its path,
// SHA-256 and size only (the content is untrusted and never read by the Control Plane, KB KIN-A-15).
type InstructionFile struct {
	Path    string
	SHA256  string
	Size    int64
	Symlink bool
}

// ChangedFile is a path the attempt changed against the base: A, M, D or T (type changed). The runner diffs with
// --no-renames, so a rename shows as a deletion and an addition.
type ChangedFile struct {
	Path   string
	Status string
}

// InstructionChange is an instruction file whose hash changed during the attempt (empty Before/After: absent).
type InstructionChange struct {
	Path   string
	Before string
	After  string
}

// WorkspacePrepared is the worktree the runner prepared for the attempt (workspace.prepared).
type WorkspacePrepared struct {
	At                    time.Time // the runner's clock
	Repository            string
	Branch                string
	Base                  string
	BaseSHA               string
	Instructions          []InstructionFile
	InstructionsTruncated bool
}

// WorkspaceChanges is what the attempt changed against the base (workspace.changes): Files lists at most what fit
// the runner's message, FilesTotal counts all of them.
type WorkspaceChanges struct {
	At                  time.Time // the runner's clock
	Branch              string
	BaseSHA             string
	HeadSHA             string
	Files               []ChangedFile
	FilesTotal          int
	Truncated           bool
	InstructionsChanged []InstructionChange
}

// AttemptWorkspace is an attempt's latest workspace reports; nil where none was recorded (an attempt without a
// repository, one that has not got that far, or a report removed by retention).
type AttemptWorkspace struct {
	Prepared *WorkspacePrepared
	Changes  *WorkspaceChanges
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
	if err := s.attemptVisible(ctx, p, taskID, stepID, fence); err != nil {
		return EventPage{}, err
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

// AttemptWorkspace returns the latest workspace reports of the task's step attempt with fence (KIN-70), for anyone
// who may see the task (domain.ErrNotFound otherwise, also when the step or attempt is not the task's).
func (s *Service) AttemptWorkspace(ctx context.Context, p auth.Principal, taskID, stepID string, fence int64) (AttemptWorkspace, error) {
	if err := s.attemptVisible(ctx, p, taskID, stepID, fence); err != nil {
		return AttemptWorkspace{}, err
	}
	if s.Log == nil {
		return AttemptWorkspace{}, nil
	}
	return s.Log.AttemptWorkspace(ctx, stepID, fence)
}

// attemptVisible checks that p may see the task and that the step attempt with fence is the task's.
func (s *Service) attemptVisible(ctx context.Context, p auth.Principal, taskID, stepID string, fence int64) error {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return err
	}
	v, err := s.visible(ctx, p)
	if err != nil {
		return err
	}
	if _, ok := v[t.ProjectID]; !ok {
		return domain.ErrNotFound
	}
	if _, ok := domain.PlanningRunID(stepID); ok {
		return s.planningVisible(ctx, t.ID, stepID, fence)
	}
	e, err := s.Repo.ExecutionByStep(ctx, stepID)
	if err != nil {
		return err
	}
	st := e.Step(stepID)
	if e.TaskID != t.ID || st == nil || !slices.ContainsFunc(st.Attempts, func(a domain.Attempt) bool { return a.Fence == fence }) {
		return domain.ErrNotFound
	}
	return nil
}
