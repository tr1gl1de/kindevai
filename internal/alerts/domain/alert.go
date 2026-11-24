// Package domain is the Alert Center (KIN-40): alerts raised by the platform's monitors, their life cycle and the
// users' notification preferences. Alert First: an alert is shown and delivered to people; nothing acts on it
// automatically.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Domain errors.
var (
	ErrNotFound          = errors.New("not found")
	ErrForbidden         = errors.New("forbidden")
	ErrInvalidTransition = errors.New("invalid alert state transition")
)

// ValidationError rejects an input field with a machine-readable code.
type ValidationError struct{ Field, Code string }

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Code) }

// State is where an alert is in its life cycle. OPEN and ACKNOWLEDGED are active; the others are closed for good.
type State string

const (
	StateOpen         State = "OPEN"
	StateAcknowledged State = "ACKNOWLEDGED"
	StateDismissed    State = "DISMISSED"
	StateResolved     State = "RESOLVED"
)

// Severity orders alerts.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func (s Severity) rank() int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityCritical:
		return 3
	}
	return 0
}

// Kind is the monitor that raised an alert.
type Kind string

const (
	KindRunnerUnreachable Kind = "runner_unreachable"
	KindStorageLevel      Kind = "storage_level"
	KindRunnerOutputFull  Kind = "runner_output_full" // KIN-59: runner output is dropped, the Control Plane's output storage is full
)

// Limits of stored alert fields.
const (
	MaxDedupKeyBytes = 200
	MaxDetails       = 16
	maxDetailBytes   = 200
)

// Occurrence is one observation of an anomaly by a monitor. ProjectID "" is a platform alert (runners,
// Control Plane storage), visible to the Owner only.
type Occurrence struct {
	ProjectID string
	Kind      Kind
	Severity  Severity
	DedupKey  string // one active alert per key: repeats are grouped into it
	Subject   string // what the alert is about, for display (runner id, machine/volume)
	Details   map[string]string
}

func (o Occurrence) validate() error {
	switch {
	case o.Kind == "" || !printable(string(o.Kind), 64):
		return &ValidationError{Field: "kind", Code: "invalid"}
	case o.Severity.rank() == 0:
		return &ValidationError{Field: "severity", Code: "invalid"}
	case o.DedupKey == "" || !printable(o.DedupKey, MaxDedupKeyBytes):
		return &ValidationError{Field: "dedupKey", Code: "invalid"}
	case !printable(o.Subject, maxDetailBytes):
		return &ValidationError{Field: "subject", Code: "invalid"}
	case len(o.Details) > MaxDetails:
		return &ValidationError{Field: "details", Code: "too_many"}
	}
	for k, v := range o.Details {
		if k == "" || !printable(k, 64) || !printable(v, maxDetailBytes) {
			return &ValidationError{Field: "details", Code: "invalid"}
		}
	}
	return nil
}

func printable(s string, maxBytes int) bool {
	return len(s) <= maxBytes && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Alert is the aggregate: one anomaly, grouped over its repeats while it is active.
type Alert struct {
	ID          string
	ProjectID   string
	Kind        Kind
	Severity    Severity
	DedupKey    string
	Subject     string
	Details     map[string]string
	State       State
	Occurrences int
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	AckedBy     string // user id; "" when not acknowledged
	AckedAt     *time.Time
	ClosedBy    string // user id, or "" when the system resolved it
	ClosedAt    *time.Time
	// LastEvent is the id of the monitor event last applied (its outbox key), so a redelivered event is not
	// counted twice.
	LastEvent string
}

// NewAlert opens an alert for its first occurrence.
func NewAlert(id string, o Occurrence, now time.Time) (Alert, error) {
	if err := o.validate(); err != nil {
		return Alert{}, err
	}
	return Alert{ID: id, ProjectID: o.ProjectID, Kind: o.Kind, Severity: o.Severity, DedupKey: o.DedupKey,
		Subject: o.Subject, Details: o.Details, State: StateOpen, Occurrences: 1, FirstSeenAt: now, LastSeenAt: now}, nil
}

// Active reports whether the alert still groups repeats (OPEN or ACKNOWLEDGED).
func (a Alert) Active() bool { return a.State == StateOpen || a.State == StateAcknowledged }

// Recur records a repeat of an active alert: the latest subject and details replace the old ones. A higher
// severity escalates it: the alert takes the new severity and an acknowledged alert opens again. It reports whether
// it escalated (people are notified again).
func (a *Alert) Recur(o Occurrence, now time.Time) (bool, error) {
	if err := o.validate(); err != nil {
		return false, err
	}
	a.Occurrences++
	a.LastSeenAt = now
	a.Subject, a.Details = o.Subject, o.Details
	if o.Severity.rank() <= a.Severity.rank() {
		return false, nil
	}
	a.Severity = o.Severity
	a.State, a.AckedBy, a.AckedAt = StateOpen, "", nil
	return true, nil
}

// Acknowledge marks an open alert as seen by userID; it stays active.
func (a *Alert) Acknowledge(userID string, now time.Time) error {
	if a.State != StateOpen {
		return fmt.Errorf("alert %s is %s: %w", a.ID, a.State, ErrInvalidTransition)
	}
	a.State, a.AckedBy, a.AckedAt = StateAcknowledged, userID, &now
	return nil
}

// Dismiss closes an active alert as not needing action.
func (a *Alert) Dismiss(userID string, now time.Time) error {
	return a.close(StateDismissed, userID, now)
}

// Resolve closes an active alert as fixed; userID "" means the system saw the anomaly end.
func (a *Alert) Resolve(userID string, now time.Time) error {
	return a.close(StateResolved, userID, now)
}

func (a *Alert) close(to State, userID string, now time.Time) error {
	if !a.Active() {
		return fmt.Errorf("alert %s is %s: %w", a.ID, a.State, ErrInvalidTransition)
	}
	a.State, a.ClosedBy, a.ClosedAt = to, userID, &now
	return nil
}
