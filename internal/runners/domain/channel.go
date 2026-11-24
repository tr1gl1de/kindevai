package domain

import (
	"errors"
	"time"
)

// Command/event channel (KIN-22). Commands for a runner are numbered in a per-runner log (Seq, monotonic within
// the log's epoch) and kept until acknowledged; events from the runner are recorded once per attempt event.

// Event errors reported by the workflow port.
var (
	// ErrStaleAttempt: the attempt's fencing token is no longer current (or its lease ran out before it started):
	// the event is rejected and the runner is told to stop the process.
	ErrStaleAttempt = errors.New("stale attempt")
	// ErrEventRejected: the event does not fit the step's state (e.g. a result for a step that already finished).
	ErrEventRejected = errors.New("event rejected")
)

// MaxAttemptOutput bounds the output stored for one attempt; output beyond it is acknowledged and dropped.
const MaxAttemptOutput = 16 << 20

// AckedCommandRetention: acknowledged commands are kept this long, so a re-delivered outbox message (at least
// once) finds its command id and is not appended a second time.
const AckedCommandRetention = 7 * 24 * time.Hour

// QueuedCommand is a command waiting in a runner's log.
type QueuedCommand struct {
	Seq  int64
	ID   string
	Kind string
	Body []byte
}

// Channel is a runner's channel position: the command log (epoch, last assigned seq, last acknowledged seq) and
// the event stream cursor (the runner spool's epoch and the last seq applied).
type Channel struct {
	CommandEpoch string
	CommandSeq   int64
	CommandAcked int64
	EventEpoch   string
	EventLast    int64
}

// WireEpoch is the command epoch a runner sees: the log's stored epoch qualified by the Control Plane instance (a
// random id per process start, KIN-41). A database restored from a backup or imported keeps the stored epoch but
// is necessarily opened by a new process, so the runner sees a new epoch, resets its cursor to Base and never
// skips new commands as duplicates of seq numbers it applied before the restore (the KIN-22 limit). Unacknowledged
// commands are re-sent after every restart and re-applied: runner handlers are idempotent by command id.
func (c Channel) WireEpoch(instance string) string {
	if instance == "" {
		return c.CommandEpoch
	}
	return c.CommandEpoch + "." + instance
}

// Event outcomes.
const (
	OutcomeApplied  = "applied"
	OutcomeRejected = "rejected"
)

// AttemptEvent is one runner event as recorded: (Step, Fence, AttemptSeq) is unique.
type AttemptEvent struct {
	RunnerID   string
	Step       string
	Fence      int64
	AttemptSeq int64
	Kind       string
	Body       []byte // nil when dropped (over MaxAttemptOutput)
	Outcome    string
	Reason     string
	RunnerAt   time.Time
	ReceivedAt time.Time
}
