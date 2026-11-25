package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// TopicYouTrackWrite delivers one write-back of a task to YouTrack (KIN-38): a comment, a state change or a
// Knowledge Base section. The message key is the write's correlation id.
const TopicYouTrackWrite = "youtrack.write"

// Alert Center hooks of the write-back (KIN-107): a write marked failed raises a project alert; the next write of
// the task that is published closes it.
const (
	TopicYouTrackWriteFailed    = "alerts.youtrack_write_failed"
	TopicYouTrackWritePublished = "alerts.youtrack_write_published"
)

// WriteOrderingKey is the outbox ordering key of a task's writes: they reach YouTrack in the order they were
// enqueued (the final comment never before the state change of the same transition).
func WriteOrderingKey(taskID string) string { return "youtrack/task/" + taskID }

// AlertOrderingKey orders a task's write-back alerts, so a close never overtakes the failure it closes.
func AlertOrderingKey(taskID string) string { return "youtrack-write/task/" + taskID }

// MaxArticleAttempts bounds the read → recompute → revision check rounds of one article write.
const MaxArticleAttempts = 3

// ArticleRevision is an article's content and its `updated` time (Unix ms), the revision a write checks.
type ArticleRevision struct {
	Content string
	Updated int64
}

// SyncClient writes to YouTrack as the integration user (the connection's token). It runs only from outbox
// deliveries.
type SyncClient interface {
	// IssueComments returns the texts of the issue's comments (the newest ones; the marker search needs only those
	// the platform may have posted recently).
	IssueComments(ctx context.Context, baseURL string, token domain.Secret, issueID string) ([]string, error)
	AddComment(ctx context.Context, baseURL string, token domain.Secret, issueID, text string) error
	ApplyCommand(ctx context.Context, baseURL string, token domain.Secret, issueID, command string) error
	ArticleRevision(ctx context.Context, baseURL string, token domain.Secret, articleID string) (ArticleRevision, error)
	UpdateArticle(ctx context.Context, baseURL string, token domain.Secret, articleID, content string) error
}

// WriteStore keeps the writes and the projects' state maps.
type WriteStore interface {
	StateMap(ctx context.Context, projectID string) (domain.StateMap, error) // domain.ErrNotFound when unbound
	SaveIssueWrite(ctx context.Context, w domain.IssueWrite) error
	MarkIssueWrite(ctx context.Context, key string, s domain.WriteStatus, code string, attempts int, at time.Time) error
	TaskPublication(ctx context.Context, taskID string) (domain.Publication, error)
}

// IssueSync is the write-back: the request methods run in the caller's transaction (the workflow's task change)
// and enqueue; HandleWrite delivers. Writes run as the integration user.
type IssueSync struct {
	Connection YouTrackConnection
	Client     SyncClient
	Outbox     Outbox
	Store      WriteStore
	// Tx (optional) records a delivery's outcome and its alert atomically.
	Tx  TxRunner
	Now func() time.Time
	// MaxAttempts must equal the outbox's MaxAttempts (default: the messaging default): the last attempt's failure
	// fails the write.
	MaxAttempts int
}

func (s *IssueSync) maxAttempts() int {
	if s.MaxAttempts > 0 {
		return s.MaxAttempts
	}
	return messaging.Config{}.WithDefaults().MaxAttempts
}

type writePayload struct {
	Kind      domain.WriteKind `json:"kind"`
	TaskID    string           `json:"taskId"`
	ProjectID string           `json:"projectId,omitempty"` // KIN-107: the project of the failure alert
	Issue     string           `json:"issue"`
	Text      string           `json:"text,omitempty"`
	Command   string           `json:"command,omitempty"`
	Article   string           `json:"article,omitempty"`
	Section   string           `json:"section,omitempty"`
	Content   string           `json:"content,omitempty"`
}

// Bounds of a write (the outbox payload holds at most messaging.MaxPayloadBytes).
const (
	MaxCommentBytes = 16 << 10
	MaxSectionBytes = 1 << 10
	MaxContentBytes = 24 << 10
	maxWriteKey     = 200
	maxAlertKey     = 256
)

var articleRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,19}-A-[1-9][0-9]{0,9}$`)

func (s *IssueSync) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// Comment enqueues a comment on the task's issue; a text over MaxCommentBytes is cut (a comment never fails the
// task change that writes it).
func (s *IssueSync) Comment(ctx context.Context, key, taskID, projectID, issueID, text string) error {
	code := ""
	if strings.TrimSpace(text) == "" {
		code = "invalid_text"
	}
	text = clip(text, MaxCommentBytes)
	return s.enqueue(ctx, key, writePayload{Kind: domain.WriteComment, TaskID: taskID, ProjectID: projectID, Issue: issueID,
		Text: text}, code)
}

// State enqueues the YouTrack state the project maps taskState to; an unmapped state or an unbound project writes
// nothing (the default: comments only).
func (s *IssueSync) State(ctx context.Context, key, taskID, projectID, issueID, taskState string) error {
	m, err := s.Store.StateMap(ctx, projectID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cmd := m.Command(taskState)
	if cmd == "" {
		return nil
	}
	return s.enqueue(ctx, key, writePayload{Kind: domain.WriteState, TaskID: taskID, ProjectID: projectID, Issue: issueID,
		Command: cmd}, "")
}

// Article enqueues a Knowledge Base section (replaced or added) for the task.
func (s *IssueSync) Article(ctx context.Context, key, taskID, projectID, issueID, articleID, section, content string) error {
	code := ""
	switch {
	case !articleRE.MatchString(articleID):
		code, articleID = "invalid_article", ""
	case strings.TrimSpace(section) == "" || len(section) > MaxSectionBytes || strings.ContainsAny(section, "\r\n"):
		code = "invalid_section"
	case strings.TrimSpace(content) == "" || len(content) > MaxContentBytes:
		code = "invalid_content"
	}
	return s.enqueue(ctx, key, writePayload{Kind: domain.WriteArticle, TaskID: taskID, ProjectID: projectID, Issue: issueID,
		Article: articleID,
		Section: strings.TrimSpace(section), Content: content}, code)
}

// Publication sums the task's writes for the task page.
func (s *IssueSync) Publication(ctx context.Context, taskID string) (domain.Publication, error) {
	return s.Store.TaskPublication(ctx, taskID)
}

// enqueue records the write and its outbox message. A write that cannot be sent (reject, a malformed issue id, a
// payload over the outbox limit) is recorded as failed instead: the write-back never fails the task change that
// asked for it. Only storage errors are returned.
func (s *IssueSync) enqueue(ctx context.Context, key string, p writePayload, reject string) error {
	if key == "" || len(key) > maxWriteKey || p.TaskID == "" {
		return &domain.ValidationError{Field: "key", Code: "invalid"}
	}
	if id, err := domain.NewIssueID(p.Issue); reject == "" && (err != nil || id != p.Issue) {
		reject = "invalid_issue"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // Markdown keeps its < > & unexpanded within the payload limit
	if err := enc.Encode(p); err != nil {
		return err
	}
	b := bytes.TrimSpace(buf.Bytes())
	if reject == "" && len(b) > messaging.MaxPayloadBytes {
		reject = "too_large"
	}
	now := s.now()
	if err := s.Store.SaveIssueWrite(ctx, domain.IssueWrite{Key: key, TaskID: p.TaskID, IssueID: clip(p.Issue, 64),
		Kind: p.Kind, Target: p.Article, CreatedAt: now}); err != nil {
		return err
	}
	if reject != "" {
		if err := s.Store.MarkIssueWrite(ctx, key, domain.WriteFailed, reject, 0, now); err != nil {
			return err
		}
		return s.alertFailed(ctx, key, p, reject)
	}
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterYouTrack, Topic: TopicYouTrackWrite, Key: key,
		Payload: b, OrderingKey: WriteOrderingKey(p.TaskID)}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", TopicYouTrackWrite, err)
	}
	return nil
}

// errConflict: the article kept changing between our read and our revision check.
var errConflict = errors.New("youtrack article changed while writing")

// HandleWrite is the outbox handler of TopicYouTrackWrite. Each kind is idempotent: a comment is looked up by its
// marker first, a state command sets the same state again, and an article section already in place is not
// rewritten. A retryable failure leaves the write pending with its error ("awaiting publication").
func (s *IssueSync) HandleWrite(ctx context.Context, d messaging.Delivery) error {
	var p writePayload
	if err := json.Unmarshal(d.Payload, &p); err != nil {
		return messaging.Permanent(errors.New("malformed youtrack write payload"))
	}
	set, err := s.Connection.YouTrackSettings(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured), errors.Is(err, domain.ErrSecretNotSet):
		return s.fail(ctx, d, p, "not_configured", fmt.Errorf("youtrack: %w", err))
	case err != nil:
		return err
	}
	switch p.Kind {
	case domain.WriteComment:
		err = s.comment(ctx, set, d.Key, p)
	case domain.WriteState:
		err = s.Client.ApplyCommand(ctx, set.BaseURL, set.Token, p.Issue, p.Command)
	case domain.WriteArticle:
		err = s.article(ctx, set, p)
	default:
		return s.fail(ctx, d, p, "malformed", fmt.Errorf("youtrack write kind %q", p.Kind))
	}
	if err != nil {
		return s.classify(ctx, d, p, err)
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		if err := s.Store.MarkIssueWrite(ctx, d.Key, domain.WritePublished, "", d.Attempt, s.now()); err != nil {
			return err
		}
		return s.alertPublished(ctx, d.Key, p)
	})
}

func (s *IssueSync) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.Tx == nil {
		return fn(ctx)
	}
	return s.Tx.InTx(ctx, fn)
}

// alertFailed raises the Alert Center's project alert of a failed write (KIN-107). A write enqueued before the
// payload carried its project raises none.
func (s *IssueSync) alertFailed(ctx context.Context, key string, p writePayload, code string) error {
	if p.ProjectID == "" {
		return nil
	}
	return s.alert(ctx, TopicYouTrackWriteFailed, "youtrack/"+key+"/failed", p.TaskID, map[string]string{
		"projectId": p.ProjectID, "taskId": p.TaskID, "issue": clip(p.Issue, 64), "kind": string(p.Kind), "code": code})
}

// alertPublished closes the task's write-back alert when a write is published after one of its writes failed.
func (s *IssueSync) alertPublished(ctx context.Context, key string, p writePayload) error {
	if p.ProjectID == "" {
		return nil
	}
	pub, err := s.Store.TaskPublication(ctx, p.TaskID)
	if err != nil || pub.Failed == 0 {
		return err
	}
	return s.alert(ctx, TopicYouTrackWritePublished, "youtrack/"+key+"/published", p.TaskID,
		map[string]string{"projectId": p.ProjectID, "taskId": p.TaskID})
}

func (s *IssueSync) alert(ctx context.Context, topic, key, taskID string, body map[string]string) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(key) > maxAlertKey { // the write key is at most maxWriteKey; keep the alert's within the outbox limit
		key = key[:maxAlertKey]
	}
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b,
		OrderingKey: AlertOrderingKey(taskID)}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}

func (s *IssueSync) comment(ctx context.Context, set YouTrackSettings, key string, p writePayload) error {
	marker := domain.WriteMarker(key)
	texts, err := s.Client.IssueComments(ctx, set.BaseURL, set.Token, p.Issue)
	if err != nil {
		return err
	}
	for _, t := range texts {
		if strings.Contains(t, marker) {
			return nil // posted by an attempt whose response was lost
		}
	}
	return s.Client.AddComment(ctx, set.BaseURL, set.Token, p.Issue, strings.TrimRight(p.Text, "\n")+"\n\n`"+marker+"`")
}

// article recomputes the section on the article as read, then checks right before the write that the article is
// still at that revision; a change in between re-reads and recomputes. YouTrack has no conditional update, so the
// window between the check and the write stays as small as one request.
func (s *IssueSync) article(ctx context.Context, set YouTrackSettings, p writePayload) error {
	for range MaxArticleAttempts {
		rev, err := s.Client.ArticleRevision(ctx, set.BaseURL, set.Token, p.Article)
		if err != nil {
			return err
		}
		next := domain.ReplaceSection(rev.Content, p.Section, p.Content)
		if next == rev.Content {
			return nil
		}
		check, err := s.Client.ArticleRevision(ctx, set.BaseURL, set.Token, p.Article)
		if err != nil {
			return err
		}
		if check.Updated != rev.Updated || check.Content != rev.Content {
			continue
		}
		return s.Client.UpdateArticle(ctx, set.BaseURL, set.Token, p.Article, next)
	}
	return errConflict
}

// fail marks the write failed, raises its alert and drops the message; if that cannot be recorded, the delivery is
// retried.
func (s *IssueSync) fail(ctx context.Context, d messaging.Delivery, p writePayload, code string, cause error) error {
	err := s.inTx(ctx, func(ctx context.Context) error {
		err := s.Store.MarkIssueWrite(ctx, d.Key, domain.WriteFailed, code, d.Attempt, s.now())
		if errors.Is(err, domain.ErrNotFound) {
			return nil // not tracked (a write enqueued before its record): nothing to show or alert
		}
		if err != nil {
			return err
		}
		return s.alertFailed(ctx, d.Key, p, code)
	})
	if err != nil {
		return fmt.Errorf("recording failure %s: %w", code, err)
	}
	return messaging.Permanent(cause)
}

// classify fails permanently on what a retry cannot fix; anything else stays pending with its error and is retried.
func (s *IssueSync) classify(ctx context.Context, d messaging.Delivery, p writePayload, err error) error {
	var se *StatusError
	switch {
	case errors.Is(err, errConflict):
		return s.fail(ctx, d, p, "conflict", err)
	case errors.As(err, &se) && (se.Status == 401 || se.Status == 403):
		return s.fail(ctx, d, p, "unauthorized", err)
	case errors.As(err, &se) && se.Permanent():
		return s.fail(ctx, d, p, fmt.Sprintf("http_%d", se.Status), err)
	case errors.Is(err, domain.ErrIssueNotFound):
		return s.fail(ctx, d, p, "not_found", err)
	}
	code := "unavailable"
	if errors.As(err, &se) {
		code = fmt.Sprintf("http_%d", se.Status)
	}
	if d.Attempt >= s.maxAttempts() {
		return s.fail(ctx, d, p, code, err) // the outbox gives up now: the write is not "awaiting publication" any more
	}
	if merr := s.Store.MarkIssueWrite(ctx, d.Key, domain.WritePending, code, d.Attempt, s.now()); merr != nil &&
		!errors.Is(merr, domain.ErrNotFound) {
		return errors.Join(err, merr)
	}
	return err
}

// clip cuts s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const mark = "\n…"
	s = s[:n-len(mark)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + mark
}
