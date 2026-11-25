package domain

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Analysis task suggested from an alert (KIN-61). Alert First: the platform only drafts it; a YouTrack issue is
// created after the Owner or a Project Admin confirms the draft, and at most one per alert.

// Limits of an analysis task draft.
const (
	MaxAnalysisSummary     = 200     // runes; YouTrack's summary limit is far higher
	MaxAnalysisDescription = 8 << 10 // bytes
)

// AnalysisDraft is the YouTrack issue prefilled from an alert.
type AnalysisDraft struct {
	Summary     string
	Description string
}

// AnalysisMarker is the reference to the alert written into the issue: it lets a retried creation find the issue
// an earlier attempt already created instead of creating a second one.
func AnalysisMarker(alertID string) string { return "kindevai-alert-" + alertID }

// oneLine replaces control characters (newlines included) with spaces.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// DraftAnalysis builds the issue from the alert's own fields only (kind, severity, subject, details, counts); the
// caller still masks anything credential-shaped before it leaves the platform. projectName "" is a platform alert.
func DraftAnalysis(l Language, a Alert, projectName string) AnalysisDraft {
	l = lang(l)
	ru := l == LangRU
	text := RenderAlert(l, a)
	body := text
	if _, after, ok := strings.Cut(text, ": "); ok {
		body = after // without the severity label
	}
	prefix := "Analyze alert: "
	if ru {
		prefix = "Разобрать алерт: "
	}
	summary := clipRunes(strings.TrimSpace(oneLine(prefix+body)), MaxAnalysisSummary)

	project := clip(projectName)
	if project == "" {
		project = map[Language]string{LangEN: "Platform", LangRU: "Платформа"}[l]
	}
	label := func(en, ruText string) string {
		if ru {
			return ruText
		}
		return en
	}
	var b strings.Builder
	b.WriteString(label("Analysis task suggested from a kindevai alert.", "Задача на анализ, предложенная по алерту kindevai."))
	b.WriteString("\n\n")
	b.WriteString(oneLine(text))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "- %s: %s\n", label("Project", "Проект"), project)
	fmt.Fprintf(&b, "- %s: %s\n", label("Kind", "Тип"), clip(string(a.Kind)))
	fmt.Fprintf(&b, "- %s: %s\n", label("Severity", "Уровень"), severityLabel[l][a.Severity])
	fmt.Fprintf(&b, "- %s: %s\n", label("Subject", "Объект"), clip(a.Subject))
	fmt.Fprintf(&b, "- %s: %s\n", label("State", "Состояние"), a.State)
	fmt.Fprintf(&b, "- %s: %d\n", label("Occurrences", "Повторений"), a.Occurrences)
	if !a.FirstSeenAt.IsZero() {
		fmt.Fprintf(&b, "- %s: %s — %s\n", label("Seen", "Наблюдался"), a.FirstSeenAt.UTC().Format(time.RFC3339),
			a.LastSeenAt.UTC().Format(time.RFC3339))
	}
	if len(a.Details) > 0 {
		fmt.Fprintf(&b, "- %s:\n", label("Details", "Детали"))
		keys := make([]string, 0, len(a.Details))
		for k := range a.Details {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  - %s: %s\n", clip(k), clip(a.Details[k]))
		}
	}
	b.WriteString("\n")
	b.WriteString(label("The platform does not act on alerts automatically (Alert First): investigate the cause, then plan the fix.",
		"Платформа не действует по алертам автоматически (Alert First): выясните причину и спланируйте исправление."))
	b.WriteString("\n\n")
	b.WriteString(AnalysisMarker(clip(a.ID)))
	desc := b.String()
	if len(desc) > MaxAnalysisDescription {
		n := MaxAnalysisDescription
		for n > 0 && !utf8.RuneStart(desc[n]) {
			n--
		}
		desc = desc[:n]
	}
	return AnalysisDraft{Summary: summary, Description: desc}
}

// AnalysisTask is the YouTrack issue requested for an alert: requested on confirmation, created once the outbox
// delivered it. One per alert, whatever the alert's state.
type AnalysisTask struct {
	AlertID         string
	ProjectID       string // the platform project whose YouTrack project receives the issue
	YouTrackProject string // short name
	RequestedBy     string // user id
	RequestedAt     time.Time
	IssueID         string // "" until created
	IssueURL        string
	CreatedAt       *time.Time
}

var (
	shortNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,49}$`)
	issueIDRE   = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,49}-[1-9][0-9]{0,9}$`)
)

// NewAnalysisTask records a confirmed request.
func NewAnalysisTask(alertID, projectID, youtrackProject, userID string, now time.Time) (AnalysisTask, error) {
	switch {
	case alertID == "":
		return AnalysisTask{}, &ValidationError{Field: "alertId", Code: "required"}
	case projectID == "":
		return AnalysisTask{}, &ValidationError{Field: "projectId", Code: "required"}
	case !shortNameRE.MatchString(youtrackProject):
		return AnalysisTask{}, &ValidationError{Field: "youtrackProject", Code: "invalid"}
	case userID == "":
		return AnalysisTask{}, &ValidationError{Field: "requestedBy", Code: "required"}
	}
	return AnalysisTask{AlertID: alertID, ProjectID: projectID, YouTrackProject: youtrackProject, RequestedBy: userID,
		RequestedAt: now}, nil
}

// Created reports whether the issue exists in YouTrack.
func (t AnalysisTask) Created() bool { return t.IssueID != "" }

// MarkCreated stores the created issue. Recording the same issue again (a redelivery) is a no-op; another issue is
// ErrInvalidTransition.
func (t *AnalysisTask) MarkCreated(issueID, issueURL string, now time.Time) error {
	u, err := url.Parse(issueURL)
	if !issueIDRE.MatchString(issueID) || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		len(issueURL) > 2048 {
		return &ValidationError{Field: "issue", Code: "invalid"}
	}
	if t.Created() {
		if t.IssueID == issueID {
			return nil
		}
		return fmt.Errorf("alert %s already has issue %s: %w", t.AlertID, t.IssueID, ErrInvalidTransition)
	}
	t.IssueID, t.IssueURL, t.CreatedAt = issueID, issueURL, &now
	return nil
}
