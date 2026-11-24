package domain

import "slices"

// NotificationType is a kind of Telegram notification a user can turn off.
type NotificationType string

const (
	NotifyApprovalRequested NotificationType = "approval_requested" // an approval waits for the Owner
	NotifyAlertRaised       NotificationType = "alert_raised"       // a new or escalated alert
	NotifyTaskFinished      NotificationType = "task_finished"      // a task completed or failed (sources: KIN-25+)
)

// NotificationTypes lists every type, in display order.
var NotificationTypes = []NotificationType{NotifyApprovalRequested, NotifyAlertRaised, NotifyTaskFinished}

// Language of a user's notifications.
type Language string

const (
	LangEN Language = "en"
	LangRU Language = "ru"
)

// Preferences are a user's notification settings. Everything is on by default; the language defaults to English
// until the user saves the settings (the web UI saves its current language).
type Preferences struct {
	UserID   string
	Language Language
	Disabled []NotificationType
}

// DefaultPreferences are the settings of a user who never changed them.
func DefaultPreferences(userID string) Preferences {
	return Preferences{UserID: userID, Language: LangEN}
}

// NewPreferences validates a user's settings.
func NewPreferences(userID, language string, disabled []NotificationType) (Preferences, error) {
	l := Language(language)
	if l != LangEN && l != LangRU {
		return Preferences{}, &ValidationError{Field: "language", Code: "invalid"}
	}
	out := make([]NotificationType, 0, len(disabled))
	for _, t := range disabled {
		if !slices.Contains(NotificationTypes, t) {
			return Preferences{}, &ValidationError{Field: "disabled", Code: "unknown_type"}
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return Preferences{UserID: userID, Language: l, Disabled: out}, nil
}

// Wants reports whether the user receives notifications of type t.
func (p Preferences) Wants(t NotificationType) bool { return !slices.Contains(p.Disabled, t) }
