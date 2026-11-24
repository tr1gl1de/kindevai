package app

import (
	"encoding/json"
	"errors"
)

// ErrSpoolFull: the event spool reached its size limit. Output is dropped and counted (execution.output.dropped);
// lifecycle events may use a small reserve above the limit, so a terminal result is not lost to chatty output.
var ErrSpoolFull = errors.New("event spool is full")

// SpooledEvent is one event in the runner's on-disk spool. Seq is assigned by the spool, monotonic within its epoch.
type SpooledEvent struct {
	Seq        int64           `json:"seq"`
	Step       string          `json:"step"`
	Fence      int64           `json:"fence"`
	AttemptSeq int64           `json:"attemptSeq"`
	Kind       string          `json:"kind"`
	At         int64           `json:"at"`
	Body       json.RawMessage `json:"body,omitempty"`
}

// SpoolState is the spool's position: the epoch (a random id of this spool; a new spool restarts numbering), the
// last Seq the Control Plane acknowledged and the last Seq written.
type SpoolState struct {
	Epoch      string
	Acked      int64
	Last       int64
	Bytes      int64
	LimitBytes int64
}

// PendingResult is an unacknowledged terminal event: the attempt finished, the Control Plane has not confirmed it.
type PendingResult struct {
	Step  string
	Fence int64
	Kind  string
	Seq   int64
}

// Spool is the runner's bounded, durable event log (adapters/spool). Append is durable (fsync) when it returns.
type Spool interface {
	Append(e SpooledEvent) (int64, error) // ErrSpoolFull
	// Read returns events after seq in order, at most n of them and about maxBytes of bodies (at least one).
	Read(after int64, n, maxBytes int) ([]SpooledEvent, error)
	Ack(seq int64) error // deletes everything up to seq
	State() SpoolState
	Pending() []PendingResult
}
