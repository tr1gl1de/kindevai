package domain

import (
	"errors"
	"fmt"
	"time"
)

// Retention of recorded runner events (KIN-52). Records of an attempt are kept while its execution runs (fencing
// and reconciliation need them) and removed some time after the execution finished: output first, the small
// lifecycle events later. Stored output is also bounded Control Plane-wide.

// Day is 24 hours (retention is configured in days).
const Day = 24 * time.Hour

// ErrInvalidRetention: a retention setting is out of range.
var ErrInvalidRetention = errors.New("invalid runner retention")

// RetentionPolicy says how long recorded runner events are kept after their execution finished and how much
// output may be stored in total.
type RetentionPolicy struct {
	Output         time.Duration // output events of an attempt
	Events         time.Duration // every other event (lifecycle, rejected and dropped ones)
	MaxOutputBytes int64         // all stored output; beyond it the oldest finished output goes, then new output is dropped
}

// DefaultRetention: output 14 days, events 30 days (as long as dead messages, KIN-15), 1 GiB of output.
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{Output: 14 * Day, Events: 30 * Day, MaxOutputBytes: 1 << 30}
}

// Validate checks the policy: periods of at least a day, events kept at least as long as output, and room for at
// least one attempt's full output.
func (p RetentionPolicy) Validate() error {
	switch {
	case p.Output < Day:
		return fmt.Errorf("%w: output retention %s is under a day", ErrInvalidRetention, p.Output)
	case p.Events < p.Output:
		return fmt.Errorf("%w: event retention %s is shorter than output retention %s", ErrInvalidRetention, p.Events, p.Output)
	case p.MaxOutputBytes < MaxAttemptOutput:
		return fmt.Errorf("%w: output limit %d B is below one attempt's %d B", ErrInvalidRetention, p.MaxOutputBytes, MaxAttemptOutput)
	}
	return nil
}

// Expired reports whether an execution that finished at finishedAt (zero: it no longer exists) is past cutoff.
func Expired(finishedAt, cutoff time.Time) bool { return finishedAt.Before(cutoff) }

// OutputLimits bound stored output per attempt and in total.
type OutputLimits struct {
	Attempt int64
	Total   int64
}

// OutputVerdict says whether an output event's body is stored.
type OutputVerdict int

// Output verdicts.
const (
	OutputStored      OutputVerdict = iota
	OutputOverAttempt               // the attempt reached MaxAttemptOutput
	OutputOverTotal                 // the Control Plane's output storage is full
)

// Reason is recorded on an event whose body was dropped.
func (v OutputVerdict) Reason() string {
	switch v {
	case OutputOverAttempt:
		return "output_limit"
	case OutputOverTotal:
		return "output_storage_limit"
	}
	return ""
}

// Verdict decides on n more bytes for an attempt that holds attempt bytes when total bytes are stored.
func (l OutputLimits) Verdict(attempt, total, n int64) OutputVerdict {
	switch {
	case attempt+n > l.Attempt:
		return OutputOverAttempt
	case total+n > l.Total:
		return OutputOverTotal
	}
	return OutputStored
}

// AttemptOutput is an attempt's counted output: Bytes stored, the last counted at LastAt.
type AttemptOutput struct {
	Step   string
	Fence  int64
	Bytes  int64
	LastAt time.Time
}

// EventRef points at one recorded event for retention.
type EventRef struct {
	ID         int64
	Step       string
	ReceivedAt time.Time
}
