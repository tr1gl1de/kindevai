package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// TopicYouTrackCreateIssue creates a YouTrack issue another context asked for (KIN-61: an alert's analysis task).
const TopicYouTrackCreateIssue = "youtrack.create_issue"

// Limits of a requested issue.
const (
	MaxIssueSummaryBytes     = 1 << 10
	MaxIssueDescriptionBytes = 16 << 10
)

// IssueRequest is an issue to create. Key identifies the request (one issue per key); Marker is a token that the
// description contains, so a retry can find an issue created by an attempt whose response was lost.
type IssueRequest struct {
	Key, Project, Summary, Description, Marker string
}

// NewIssue is what the YouTrack client sends.
type NewIssue struct {
	Summary, Description, Marker string
}

// IssueClient writes issues to YouTrack. It runs only from outbox deliveries.
type IssueClient interface {
	// ProjectID resolves a project short name to YouTrack's database id; domain.ErrNotFound if there is none.
	ProjectID(ctx context.Context, baseURL string, token domain.Secret, shortName string) (string, error)
	// FindIssue returns the issue of the project whose description contains marker.
	FindIssue(ctx context.Context, baseURL string, token domain.Secret, shortName, marker string) (string, bool, error)
	CreateIssue(ctx context.Context, baseURL string, token domain.Secret, projectID string, n NewIssue) (string, error)
}

// YouTrackConnection is the installation's connection and the project bindings (Service).
type YouTrackConnection interface {
	YouTrackSettings(ctx context.Context) (YouTrackSettings, error)
	YouTrackProjectOf(ctx context.Context, projectID string) (string, error)
}

// IssueWriter creates YouTrack issues through the outbox: RequestIssue enqueues in the caller's transaction,
// HandleCreateIssue delivers and reports the issue to OnCreated(key, issueID, issueURL).
type IssueWriter struct {
	Connection YouTrackConnection
	Client     IssueClient
	Outbox     Outbox
	OnCreated  func(ctx context.Context, key, issueID, issueURL string) error
}

type createIssuePayload struct {
	Key         string `json:"key"`
	Project     string `json:"project"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Marker      string `json:"marker"`
}

var markerRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func (r IssueRequest) validate() error {
	if _, err := domain.NewShortName(r.Project); err != nil || r.Project != strings.ToUpper(r.Project) {
		return &domain.ValidationError{Field: "project", Code: "short_name_format"}
	}
	switch {
	case r.Key == "" || len(r.Key) > 200:
		return &domain.ValidationError{Field: "key", Code: "invalid"}
	case strings.TrimSpace(r.Summary) == "" || len(r.Summary) > MaxIssueSummaryBytes || !utf8.ValidString(r.Summary):
		return &domain.ValidationError{Field: "summary", Code: "invalid"}
	case len(r.Description) > MaxIssueDescriptionBytes || !utf8.ValidString(r.Description):
		return &domain.ValidationError{Field: "description", Code: "invalid"}
	case !markerRE.MatchString(r.Marker):
		return &domain.ValidationError{Field: "marker", Code: "invalid"}
	}
	return nil
}

// RequestIssue enqueues the creation; the message commits with the caller's transaction.
func (w *IssueWriter) RequestIssue(ctx context.Context, r IssueRequest) error {
	if err := r.validate(); err != nil {
		return err
	}
	if !strings.Contains(r.Description, r.Marker) {
		r.Description = strings.TrimRight(r.Description, "\n") + "\n\n" + r.Marker
	}
	b, err := json.Marshal(createIssuePayload(r))
	if err != nil {
		return err
	}
	return w.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterYouTrack, Topic: TopicYouTrackCreateIssue,
		Key: "create_issue/" + r.Key, Payload: b})
}

// Configured reports a YouTrack connection with a stored token.
func (w *IssueWriter) Configured(ctx context.Context) (bool, error) {
	_, err := w.Connection.YouTrackSettings(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured), errors.Is(err, domain.ErrSecretNotSet):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Binding returns the YouTrack short name bound to a project ("" if unbound).
func (w *IssueWriter) Binding(ctx context.Context, projectID string) (string, error) {
	return w.Connection.YouTrackProjectOf(ctx, projectID)
}

// HandleCreateIssue is the outbox handler of TopicYouTrackCreateIssue. A retry first looks for the issue by its
// marker, so a response lost after YouTrack created the issue never creates a second one.
func (w *IssueWriter) HandleCreateIssue(ctx context.Context, d messaging.Delivery) error {
	var p createIssuePayload
	if err := json.Unmarshal(d.Payload, &p); err != nil {
		return messaging.Permanent(errors.New("malformed create issue payload"))
	}
	if err := IssueRequest(p).validate(); err != nil {
		return messaging.Permanent(fmt.Errorf("create issue payload: %w", err))
	}
	s, err := w.Connection.YouTrackSettings(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured), errors.Is(err, domain.ErrSecretNotSet):
		return messaging.Permanent(fmt.Errorf("youtrack: %w", err))
	case err != nil:
		return err
	}
	id, found := "", false
	if d.Attempt > 1 {
		if id, found, err = w.Client.FindIssue(ctx, s.BaseURL, s.Token, p.Project, p.Marker); err != nil {
			return classify(err)
		}
	}
	if !found {
		pid, err := w.Client.ProjectID(ctx, s.BaseURL, s.Token, p.Project)
		if errors.Is(err, domain.ErrNotFound) {
			return messaging.Permanent(fmt.Errorf("youtrack project %s not found", p.Project))
		}
		if err != nil {
			return classify(err)
		}
		if id, err = w.Client.CreateIssue(ctx, s.BaseURL, s.Token, pid,
			NewIssue{Summary: p.Summary, Description: p.Description, Marker: p.Marker}); err != nil {
			return classify(err)
		}
	}
	if id, err = domain.NewIssueID(id); err != nil {
		return messaging.Permanent(errors.New("youtrack returned a malformed issue id"))
	}
	if w.OnCreated == nil {
		return nil
	}
	return w.OnCreated(ctx, p.Key, id, s.BaseURL+"/issue/"+id)
}

// classify marks client errors that a retry cannot fix as permanent (a 404 means a wrong base URL here).
func classify(err error) error {
	var se *StatusError
	if (errors.As(err, &se) && se.Permanent()) || errors.Is(err, domain.ErrIssueNotFound) {
		return messaging.Permanent(err)
	}
	return err
}
