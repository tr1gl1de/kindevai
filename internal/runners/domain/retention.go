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

// Stored runner output in the Storage Overview (KIN-59).

// OutputDropsWindow is how far back the overview reports output dropped for the total limit;
// OutputDroppingWindow is how recent a drop must be for the storage to count as full (output is being dropped).
const (
	OutputDropsWindow    = 24 * time.Hour
	OutputDroppingWindow = 15 * time.Minute
)

// OutputStorageState summarizes stored runner output: ok, high (≥ 90% of the limit: the retention evicts the
// oldest finished output) or full (new output was dropped within OutputDroppingWindow).
type OutputStorageState string

// Output storage states.
const (
	OutputStorageOK   OutputStorageState = "ok"
	OutputStorageHigh OutputStorageState = "high"
	OutputStorageFull OutputStorageState = "full"
)

// OutputDrop is one attempt whose output bodies were dropped because the output storage was full.
type OutputDrop struct {
	RunnerID string
	Step     string
	Fence    int64
	Events   int64
	LastAt   time.Time
}

// OutputStorage is the Control Plane's stored runner output against its limit, with the recent drops.
type OutputStorage struct {
	UsedBytes     int64
	LimitBytes    int64
	Policy        RetentionPolicy
	DroppedEvents int64        // output events stored without body for the total limit within OutputDropsWindow
	LastDropAt    *time.Time   // the latest of them; nil when none
	Drops         []OutputDrop // per attempt, newest first (bounded)
}

// UsedPercent is the share of the limit in use (0 without a limit).
func (s OutputStorage) UsedPercent() float64 {
	if s.LimitBytes <= 0 {
		return 0
	}
	return float64(s.UsedBytes) * 100 / float64(s.LimitBytes)
}

// State summarizes the output storage at now.
func (s OutputStorage) State(now time.Time) OutputStorageState {
	switch {
	case s.LastDropAt != nil && now.Sub(*s.LastDropAt) < OutputDroppingWindow:
		return OutputStorageFull
	case s.LimitBytes > 0 && s.UsedBytes*10 >= s.LimitBytes*9:
		return OutputStorageHigh
	}
	return OutputStorageOK
}
