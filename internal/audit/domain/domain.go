// Package domain is the audit bounded context: an append-only record of who did what, when, in which project.
// Events are created once and never changed or deleted. Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

var (
	// ErrInvalidEvent: a programming error in the recording context (bad kind, actor or details).
	ErrInvalidEvent = errors.New("invalid audit event")
	// ErrInvalidFilter: bad query parameters.
	ErrInvalidFilter = errors.New("invalid audit filter")
	// ErrForbidden: the caller may not read the audit log.
	ErrForbidden = errors.New("forbidden")
)

// ActorType says what kind of principal acted.
type ActorType string

const (
	ActorUser   ActorType = "user"   // a signed-in account
	ActorSystem ActorType = "system" // the Control Plane itself (schedulers, bootstrap)
	ActorRunner ActorType = "runner" // an enrolled runner
)

// Actor is who performed the action. Label is a display name captured at the time (the login), so the record
// stays readable after the account is renamed or deleted.
type Actor struct {
	Type  ActorType
	ID    string
	Label string
}

// Kind names what happened as "<namespace>.<action>", e.g. "invitation.accepted"; lower case, digits and '_'.
type Kind string

var (
	kindRe      = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	kindQueryRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	detailKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
)

// Limits keep one event small; details are a short projection, never a payload dump.
const (
	MaxKindLen    = 64
	MaxIDLen      = 128
	MaxDetails    = 16
	MaxDetailLen  = 512
	maxSubjectLen = 256
)

// Event is one audit record. ID is assigned by the store and orders events (newest = highest).
type Event struct {
	ID        int64
	At        time.Time
	Actor     Actor
	Kind      Kind
	ProjectID string // "" for platform-wide events
	Subject   string // what was acted on, as "<type>:<id>", e.g. "user:01J…"
	Details   map[string]string
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvent, fmt.Sprintf(format, a...))
}

// NewEvent validates an event. Overlong detail values are truncated rather than rejected, so an audit write never
// fails the action it records because of a long name. Callers redact secrets before calling.
func NewEvent(at time.Time, actor Actor, kind, projectID, subject string, details map[string]string) (Event, error) {
	if at.IsZero() {
		return Event{}, invalid("time is required")
	}
	switch actor.Type {
	case ActorUser, ActorRunner:
		if actor.ID == "" {
			return Event{}, invalid("%s actor needs an id", actor.Type)
		}
	case ActorSystem:
		if actor.Label == "" {
			actor.Label = "system"
		}
	default:
		return Event{}, invalid("unknown actor type %q", actor.Type)
	}
	if len(actor.ID) > MaxIDLen || len(actor.Label) > MaxIDLen {
		return Event{}, invalid("actor id or label too long")
	}
	if len(kind) > MaxKindLen || !kindRe.MatchString(kind) {
		return Event{}, invalid("kind %q must be <namespace>.<action>", kind)
	}
	if len(projectID) > MaxIDLen || len(subject) > maxSubjectLen {
		return Event{}, invalid("project or subject too long")
	}
	if len(details) > MaxDetails {
		return Event{}, invalid("%d details, at most %d", len(details), MaxDetails)
	}
	var d map[string]string
	if len(details) > 0 {
		d = make(map[string]string, len(details))
		for k, v := range details {
			if !detailKeyRe.MatchString(k) {
				return Event{}, invalid("detail key %q", k)
			}
			d[k] = truncate(v, MaxDetailLen)
		}
	}
	return Event{At: at.UTC(), Actor: actor, Kind: Kind(kind), ProjectID: projectID, Subject: subject, Details: d}, nil
}

// truncate shortens s to at most n bytes on a rune boundary, marking the cut with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "…"
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// Page sizes for queries.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Filter selects events, newest first. Empty fields do not filter. Kind is a full kind or a namespace
// ("invitation" matches every "invitation.*"). Actor matches the actor's id or label. From is inclusive, To
// exclusive. Before is a keyset cursor: only events with ID < Before.
type Filter struct {
	Actor     string
	ProjectID string
	Kind      string
	From, To  time.Time
	Before    int64
	Limit     int
}

// NewFilter validates f and applies the default and maximum page size.
func NewFilter(f Filter) (Filter, error) {
	bad := func(what string) (Filter, error) { return Filter{}, fmt.Errorf("%w: %s", ErrInvalidFilter, what) }
	switch {
	case f.Limit < 0:
		return bad("limit")
	case f.Before < 0:
		return bad("before")
	case !f.From.IsZero() && !f.To.IsZero() && f.To.Before(f.From):
		return bad("to is before from")
	case f.Kind != "" && (len(f.Kind) > MaxKindLen || !kindQueryRe.MatchString(f.Kind)):
		return bad("kind")
	case len(f.Actor) > MaxIDLen || len(f.ProjectID) > MaxIDLen:
		return bad("actor or project")
	}
	if f.Limit == 0 {
		f.Limit = DefaultPageSize
	}
	f.Limit = min(f.Limit, MaxPageSize)
	return f, nil
}
