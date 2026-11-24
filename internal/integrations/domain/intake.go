package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// YouTrack intake (KIN-27): issues of a bound YouTrack project become workflow tasks when they carry the project's
// trigger tag (polling, optional webhook) or a project member starts one by ID. The issue's reporter must be linked
// to a platform account that may start AI in the project: the right to create a YouTrack issue grants nothing.

var (
	// ErrYouTrackLoginTaken: the YouTrack login is already linked to another account.
	ErrYouTrackLoginTaken = errors.New("youtrack login already linked")
	// ErrIssueNotFound: YouTrack has no such issue (or the token may not read it).
	ErrIssueNotFound = errors.New("youtrack issue not found")
	// ErrNotBound: the project is not bound to a YouTrack project.
	ErrNotBound = errors.New("project not bound to a youtrack project")
	// ErrWebhookDisabled: no webhook secret is set, so the webhook endpoint does not exist.
	ErrWebhookDisabled = errors.New("youtrack webhook disabled")
	// ErrWebhookUnauthorized: the webhook request did not carry the shared secret.
	ErrWebhookUnauthorized = errors.New("youtrack webhook secret mismatch")
)

// SecretYouTrackWebhookSecret is the shared secret a YouTrack workflow sends with webhook calls.
const SecretYouTrackWebhookSecret SecretName = "youtrack.webhook_secret" //nolint:gosec // G101: a vault slot name, not a credential

// DefaultTriggerTag marks YouTrack issues for the platform unless a project configures another tag.
const DefaultTriggerTag = "kindevai"

// Poll interval bounds; the default is one minute (KIN-27).
const (
	MinPollSeconds     = 15
	MaxPollSeconds     = 3600
	DefaultPollSeconds = 60
)

// MaxTaskTitle bounds the title of a task made from an issue (the workflow's limit).
const MaxTaskTitle = 200

// MaxSnapshotTags bounds the tags kept from an issue.
const MaxSnapshotTags = 50

var issueIDRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,19}-[1-9][0-9]{0,9}$`)

// NewIssueID validates a readable issue ID such as "KIN-7" (upper-cased). The format keeps it safe in URL paths.
func NewIssueID(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !issueIDRE.MatchString(s) {
		return "", invalid("issueId", "issue_format")
	}
	return s, nil
}

var tagRE = regexp.MustCompile(`^[\p{L}\p{N} ._-]{1,50}$`)

// NewTriggerTag validates a trigger tag ("" means DefaultTriggerTag). It is quoted in a YouTrack query, so braces
// and quotes are refused.
func NewTriggerTag(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultTriggerTag, nil
	}
	if !tagRE.MatchString(s) {
		return "", invalid("triggerTag", "tag_format")
	}
	return s, nil
}

// NewYouTrackLogin validates a YouTrack login: 1–100 printable characters without spaces.
func NewYouTrackLogin(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 100 || !utf8.ValidString(s) ||
		strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
		return "", invalid("youtrackLogin", "youtrack_login_format")
	}
	return s, nil
}

// NewPollInterval validates the polling interval in seconds.
func NewPollInterval(seconds int) (time.Duration, error) {
	if seconds < MinPollSeconds || seconds > MaxPollSeconds {
		return 0, invalid("pollIntervalSeconds", "poll_interval_range")
	}
	return time.Duration(seconds) * time.Second, nil
}

// NewYouTrackWebhookSecret validates the webhook shared secret: 32–200 printable ASCII characters without spaces.
func NewYouTrackWebhookSecret(s string) (Secret, error) {
	if len(s) < 32 || len(s) > 200 || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return Secret{}, invalid("webhookSecret", "webhook_secret_range")
	}
	return Secret{v: s}, nil
}

// IntakeSettings are the installation-wide intake settings.
type IntakeSettings struct {
	PollInterval time.Duration
	UpdatedAt    time.Time // zero while the defaults apply
}

// DefaultIntakeSettings apply until the Owner changes them.
func DefaultIntakeSettings() IntakeSettings {
	return IntakeSettings{PollInterval: DefaultPollSeconds * time.Second}
}

// Binding is a project's link to a YouTrack project with its intake state.
type Binding struct {
	ProjectID    string
	ShortName    string
	TriggerTag   string
	Cursor       int64      // the newest issue `updated` (Unix ms) polling has handed to the inbox; 0 = from the start
	LastPolledAt *time.Time // last successful poll of this binding
}

// Wants reports whether the binding takes the issue: it belongs to the bound YouTrack project and carries the
// trigger tag (case-insensitive) unless a member started it by ID.
func (b Binding) Wants(s IssueSnapshot, manual bool) bool {
	if s.Project != b.ShortName {
		return false
	}
	if manual {
		return true
	}
	for _, t := range s.Tags {
		if strings.EqualFold(strings.TrimSpace(t), b.TriggerTag) {
			return true
		}
	}
	return false
}

// IssueSnapshot is the authoritative state of an issue as read from the YouTrack REST API. Every field is
// untrusted text from YouTrack: it only ever becomes data (a task title), never a permission.
type IssueSnapshot struct {
	ID       string   `json:"id"`      // readable ID, e.g. "KIN-7"
	Project  string   `json:"project"` // short name
	Summary  string   `json:"summary"`
	Updated  int64    `json:"updated"` // Unix ms
	Resolved bool     `json:"resolved"`
	Reporter string   `json:"reporter"` // YouTrack login
	Tags     []string `json:"tags"`
}

// Validate refuses snapshots that cannot be an issue of the project they name.
func (s IssueSnapshot) Validate() error {
	id, err := NewIssueID(s.ID)
	switch {
	case err != nil || id != s.ID:
		return invalid("issueId", "issue_format")
	case !strings.HasPrefix(s.ID, s.Project+"-"):
		return invalid("project", "issue_project")
	case s.Updated <= 0:
		return invalid("updated", "updated_required")
	case len(s.Tags) > MaxSnapshotTags:
		return invalid("tags", "too_many_tags")
	}
	return nil
}

// TaskTitle is "<ID>: <summary>" with whitespace collapsed, control characters dropped and the length capped.
func (s IssueSnapshot) TaskTitle() string {
	summary := strings.Join(strings.FieldsFunc(s.Summary, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	summary = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, summary)
	title := s.ID
	if summary != "" {
		title += ": " + summary
	}
	if r := []rune(title); len(r) > MaxTaskTitle {
		title = string(r[:MaxTaskTitle-1]) + "…"
	}
	return title
}

// Outcome is what intake did with an issue.
type Outcome string

const (
	OutcomeQueued    Outcome = "queued"    // a member started it by ID; the issue is being fetched
	OutcomeCreated   Outcome = "created"   // a task was created
	OutcomeUpdated   Outcome = "updated"   // the existing task's title followed the issue
	OutcomeUnchanged Outcome = "unchanged" // the existing task already matches
	OutcomeRejected  Outcome = "rejected"  // no task: see the reason
)

// Reasons a task was not created. They are shown to project members.
const (
	ReasonUnknownUser   = "unknown_user"    // the reporter is not linked to a platform account
	ReasonNotAllowed    = "not_allowed"     // the initiator may not start AI in the project
	ReasonIssueNotFound = "issue_not_found" // YouTrack has no such issue (run by ID)
	ReasonOtherProject  = "other_project"   // the issue moved to another YouTrack project (run by ID)
	ReasonInvalidIssue  = "invalid_issue"   // the issue cannot become a task (malformed data)
	ReasonUnavailable   = "unavailable"     // YouTrack refused or kept failing (run by ID)
)

// Intake channels.
const (
	ChannelPoll    = "poll"
	ChannelWebhook = "webhook"
	ChannelManual  = "manual"
)

// IntakeRecord is the last intake decision for one issue in one project.
type IntakeRecord struct {
	ProjectID    string
	IssueID      string
	IssueUpdated int64 // the snapshot's `updated`; 0 while queued
	Outcome      Outcome
	Reason       string
	Initiator    string // platform user ID ("" when unknown)
	Reporter     string // YouTrack login of the reporter
	TaskID       string
	Channel      string
	ProcessedAt  time.Time
}

// Supersedes reports whether this record already reflects a newer state of the issue than s, so s arrived out of
// order and must be ignored. Equal states are re-applied (idempotently).
func (r IntakeRecord) Supersedes(s IssueSnapshot) bool {
	return r.Outcome != OutcomeQueued && r.IssueUpdated > s.Updated
}

// UserLink maps a YouTrack login to a platform account (set by the Owner).
type UserLink struct {
	YouTrackLogin string
	UserID        string
	UpdatedAt     time.Time
}
