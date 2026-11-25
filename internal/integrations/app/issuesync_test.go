package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// syncClient is an in-memory YouTrack: comments per issue, the last command, articles with an update counter.
type syncClient struct {
	mu       sync.Mutex
	comments map[string][]string
	commands []string
	articles map[string]app.ArticleRevision
	err      error // every call fails with it
	lost     bool  // the next comment is stored but its response is lost
	race     int   // that many revision checks (every second read) see a concurrent edit of the article
	reads    int
	writes   []string
}

func newSyncClient() *syncClient {
	return &syncClient{comments: map[string][]string{}, articles: map[string]app.ArticleRevision{
		"KIN-A-19": {Content: "# Integrations\n\n## YouTrack\n\nold\n", Updated: 100}}}
}

func (c *syncClient) IssueComments(_ context.Context, _ string, _ domain.Secret, issueID string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	return c.comments[issueID], nil
}

func (c *syncClient) AddComment(_ context.Context, _ string, _ domain.Secret, issueID, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.comments[issueID] = append(c.comments[issueID], text)
	if c.lost {
		c.lost = false
		return errors.New("connection reset")
	}
	return nil
}

func (c *syncClient) ApplyCommand(_ context.Context, _ string, _ domain.Secret, issueID, command string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.commands = append(c.commands, issueID+" "+command)
	return nil
}

func (c *syncClient) ArticleRevision(_ context.Context, _ string, _ domain.Secret, id string) (app.ArticleRevision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return app.ArticleRevision{}, c.err
	}
	a, ok := c.articles[id]
	if !ok {
		return a, domain.ErrIssueNotFound
	}
	c.reads++
	if c.race > 0 && c.reads%2 == 0 {
		c.race--
		a.Content += "\nedited by a person\n"
		a.Updated++
		c.articles[id] = a
	}
	return a, nil
}

func (c *syncClient) UpdateArticle(_ context.Context, _ string, _ domain.Secret, id, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	a := c.articles[id]
	a.Content, a.Updated = content, a.Updated+1
	c.articles[id] = a
	c.writes = append(c.writes, id)
	return nil
}

type writeStore struct {
	mu     sync.Mutex
	states map[string]domain.StateMap
	writes map[string]domain.IssueWrite
	order  []string // keys in enqueue order
}

func (s *writeStore) FollowsFailure(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	taskID, failed, between := s.writes[key].TaskID, -1, false
	for i, k := range s.order {
		w := s.writes[k]
		switch {
		case k == key:
			return failed >= 0 && !between && failed < i, nil
		case w.TaskID != taskID:
		case w.Status == domain.WriteFailed:
			failed, between = i, false
		case w.Status == domain.WritePublished:
			between = true
		}
	}
	return false, nil
}

func (s *writeStore) StateMap(_ context.Context, projectID string) (domain.StateMap, error) {
	m, ok := s.states[projectID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return m, nil
}

func (s *writeStore) SaveIssueWrite(_ context.Context, w domain.IssueWrite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.writes[w.Key]; !ok {
		w.Status = domain.WritePending
		s.writes[w.Key] = w
		s.order = append(s.order, w.Key)
	}
	return nil
}

func (s *writeStore) MarkIssueWrite(_ context.Context, key string, st domain.WriteStatus, code string, attempts int, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.writes[key]
	if !ok {
		return domain.ErrNotFound
	}
	w.Status, w.Error, w.Attempts = st, code, max(w.Attempts, attempts)
	s.writes[key] = w
	return nil
}

func (s *writeStore) MoveIssueWrite(_ context.Context, key string, from, to domain.WriteStatus, code string, attempts int,
	_ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.writes[key]
	if !ok {
		return false, domain.ErrNotFound
	}
	if w.Status != from {
		return false, nil
	}
	w.Status, w.Attempts = to, max(w.Attempts, attempts)
	if code != "" {
		w.Error = code
	}
	s.writes[key] = w
	return true, nil
}

func (s *writeStore) TaskPublication(_ context.Context, taskID string) (domain.Publication, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p domain.Publication
	for _, w := range s.writes {
		if w.TaskID != taskID {
			continue
		}
		switch w.Status {
		case domain.WritePending:
			p.Pending++
		case domain.WriteFailed:
			p.Failed++
		default:
			p.Published++
		}
		if w.Status != domain.WritePublished && w.Error != "" {
			p.LastError = w.Error
		}
	}
	return p, nil
}

func (s *writeStore) status(key string) (domain.WriteStatus, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[key].Status, s.writes[key].Error
}

func issueSync(conn ytConn) (*app.IssueSync, *syncClient, *writeStore, *queue) {
	c, q := newSyncClient(), &queue{}
	st := &writeStore{states: map[string]domain.StateMap{"p1": {}, "p2": {"RUNNING": "In Progress"}},
		writes: map[string]domain.IssueWrite{}}
	return &app.IssueSync{Connection: conn, Client: c, Outbox: q, Store: st,
		Now: func() time.Time { return time.Unix(1700000000, 0) }}, c, st, q
}

func deliveries(q *queue, attempt int) []messaging.Delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]messaging.Delivery, 0, len(q.msgs))
	for _, m := range q.msgs {
		out = append(out, messaging.Delivery{Adapter: m.Adapter, Topic: m.Topic, Key: m.Key, Payload: m.Payload, Attempt: attempt})
	}
	return out
}

// AC1: every write goes through the outbox under its correlation key; nothing reaches YouTrack inline. A comment
// carries its marker, so a retry after a lost response does not post it twice.
func TestIssueSyncCommentThroughTheOutboxIsIdempotent(t *testing.T) {
	t.Parallel()
	s, c, _, q := issueSync(ytConn{})
	ctx := t.Context()
	if err := s.Comment(ctx, "task/t1/v3/plan", "t1", "p1", "KIN-7", "Plan revision 1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t1/v3/bad", "t1", "p1", "kin-x", "x"); err != nil {
		t.Fatalf("a malformed issue id must not fail the caller: %v", err)
	}
	if len(c.comments) != 0 {
		t.Fatal("YouTrack was called inline")
	}
	m := q.msgs[0]
	if len(messagesOf(q, app.TopicYouTrackWrite)) != 1 || m.Adapter != app.AdapterYouTrack || m.Topic != app.TopicYouTrackWrite || m.Key != "task/t1/v3/plan" {
		t.Fatalf("messages = %+v", q.msgs)
	}
	if p, _ := s.Publication(ctx, "t1"); p != (domain.Publication{Pending: 1, Failed: 1, LastError: "invalid_issue"}) {
		t.Fatalf("publication before delivery = %+v", p)
	}
	c.lost = true
	d := deliveries(q, 1)[0]
	if err := s.HandleWrite(ctx, d); err == nil || messaging.IsPermanent(err) {
		t.Fatalf("lost response = %v, want a retry", err)
	}
	d.Attempt = 2
	if err := s.HandleWrite(ctx, d); err != nil {
		t.Fatal(err)
	}
	got := c.comments["KIN-7"]
	if len(got) != 1 || !strings.HasPrefix(got[0], "Plan revision 1") ||
		!strings.Contains(got[0], domain.WriteMarker("task/t1/v3/plan")) {
		t.Fatalf("comments = %q", got)
	}
	if p, _ := s.Publication(ctx, "t1"); p != (domain.Publication{Published: 1, Failed: 1, LastError: "invalid_issue"}) {
		t.Fatalf("publication = %+v", p)
	}
}

// Decision: state changes follow the project's map; the default (empty) map writes comments only.
func TestIssueSyncStateFollowsTheProjectMap(t *testing.T) {
	t.Parallel()
	s, c, _, q := issueSync(ytConn{})
	ctx := t.Context()
	for _, p := range []string{"p1", "p2", "p9"} {
		if err := s.State(ctx, "task/t1/v4/state", "t1", p, "KIN-7", "RUNNING"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.State(ctx, "task/t1/v5/state", "t1", "p2", "KIN-7", "READY"); err != nil {
		t.Fatal(err)
	}
	if len(q.msgs) != 1 {
		t.Fatalf("messages = %+v", q.msgs)
	}
	if err := s.HandleWrite(ctx, deliveries(q, 1)[0]); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.commands, ";") != "KIN-7 State {In Progress}" {
		t.Fatalf("commands = %q", c.commands)
	}
}

// AC3: an article section is recomputed on the article as it is right before the write; a concurrent edit makes it
// re-read and recompute, and one that keeps happening fails the write (shown on the task) instead of overwriting.
func TestIssueSyncArticleChecksTheRevisionBeforeWriting(t *testing.T) {
	t.Parallel()
	s, c, _, q := issueSync(ytConn{})
	ctx := t.Context()
	if err := s.Article(ctx, "task/t1/v9/article/KIN-A-19", "t1", "p1", "KIN-7", "KIN-A-19", "KIN-38", "Write-back."); err != nil {
		t.Fatal(err)
	}
	if err := s.Article(ctx, "k", "t1", "p1", "KIN-7", "../admin", "S", "x"); err != nil {
		t.Fatalf("a malformed article id must not fail the caller: %v", err)
	}
	if len(messagesOf(q, app.TopicYouTrackWrite)) != 1 {
		t.Fatalf("a malformed article was enqueued: %+v", q.msgs)
	}
	c.race = 1 // a person edits the article between our read and our check
	if err := s.HandleWrite(ctx, deliveries(q, 1)[0]); err != nil {
		t.Fatal(err)
	}
	a := c.articles["KIN-A-19"]
	if !strings.Contains(a.Content, "edited by a person") || !strings.Contains(a.Content, "## KIN-38\n\nWrite-back.") ||
		!strings.Contains(a.Content, "## YouTrack\n\nold") {
		t.Fatalf("article = %q", a.Content)
	}
	if err := s.HandleWrite(ctx, deliveries(q, 1)[0]); err != nil { // redelivery: the section is already there
		t.Fatal(err)
	}
	if len(c.writes) != 1 {
		t.Fatalf("writes = %v", c.writes)
	}

	s2, c2, st2, q2 := issueSync(ytConn{})
	if err := s2.Article(ctx, "task/t2/v1/article/KIN-A-19", "t2", "p1", "KIN-8", "KIN-A-19", "S", "x"); err != nil {
		t.Fatal(err)
	}
	c2.race = 100
	err := s2.HandleWrite(ctx, deliveries(q2, 1)[0])
	if !messaging.IsPermanent(err) || len(c2.writes) != 0 {
		t.Fatalf("endless conflict = %v, writes %v", err, c2.writes)
	}
	if status, code := st2.status("task/t2/v1/article/KIN-A-19"); status != domain.WriteFailed || code != "conflict" {
		t.Fatalf("write = %s %s", status, code)
	}
}

// AC5: YouTrack unavailable → the write is retried and stays "awaiting publication" with the error; a failure a
// retry cannot fix fails it.
func TestIssueSyncRetriesWhileYouTrackIsDown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, c, st, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v3/final", "t1", "p1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	c.err = &app.StatusError{Status: 503}
	if err := s.HandleWrite(ctx, deliveries(q, 1)[0]); err == nil || messaging.IsPermanent(err) {
		t.Fatalf("outage = %v, want a retry", err)
	}
	if p, _ := s.Publication(ctx, "t1"); p != (domain.Publication{Pending: 1, LastError: "http_503"}) {
		t.Fatalf("publication = %+v", p)
	}
	d := deliveries(q, 10) // the outbox's last attempt: the write is given up, not awaiting publication forever
	s.MaxAttempts = 10
	if err := s.HandleWrite(ctx, d[0]); !messaging.IsPermanent(err) {
		t.Fatalf("last attempt = %v", err)
	}
	if status, code := st.status("task/t1/v3/final"); status != domain.WriteFailed || code != "http_503" {
		t.Fatalf("after the last attempt: %s %s", status, code)
	}
	c.err = &app.StatusError{Status: 403}
	if err := s.HandleWrite(ctx, deliveries(q, 2)[0]); !messaging.IsPermanent(err) {
		t.Fatalf("forbidden = %v, want permanent", err)
	}
	if status, code := st.status("task/t1/v3/final"); status != domain.WriteFailed || code != "unauthorized" {
		t.Fatalf("write = %s %s", status, code)
	}

	s2, _, st2, q2 := issueSync(ytConn{err: domain.ErrNotConfigured})
	if err := s2.Comment(ctx, "task/t3/v1/final", "t3", "p1", "KIN-9", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := s2.HandleWrite(ctx, deliveries(q2, 1)[0]); !messaging.IsPermanent(err) {
		t.Fatalf("not configured = %v", err)
	}
	if status, code := st2.status("task/t3/v1/final"); status != domain.WriteFailed || code != "not_configured" {
		t.Fatalf("write = %s %s", status, code)
	}
}

// A comment never fails the task change that writes it: an oversized text is cut, and the largest article and
// Markdown full of < > & stay within the outbox payload limit.
func TestIssueSyncBounds(t *testing.T) {
	t.Parallel()
	s, _, _, q := issueSync(ytConn{})
	ctx := t.Context()
	if err := s.Comment(ctx, "k1", "t1", "p1", "KIN-7", strings.Repeat("é", app.MaxCommentBytes)); err != nil {
		t.Fatal(err)
	}
	if err := s.Article(ctx, "k2", "t1", "p1", "KIN-7", "KIN-A-1", "S", strings.Repeat("<&>\"", app.MaxContentBytes/4)); err != nil {
		t.Fatal(err)
	}
	for _, m := range q.msgs {
		if len(m.Payload) > messaging.MaxPayloadBytes {
			t.Fatalf("%s: payload %d bytes", m.Key, len(m.Payload))
		}
	}
	if err := s.Article(ctx, "k3", "t1", "p1", "KIN-7", "KIN-A-1", "S", strings.Repeat("x", app.MaxContentBytes+1)); err != nil {
		t.Fatal(err)
	}
	if n := len(messagesOf(q, app.TopicYouTrackWrite)); n != 2 {
		t.Fatalf("an oversized article was enqueued: %d messages", n)
	}
	if p, _ := s.Publication(ctx, "t1"); p.Failed != 1 || p.LastError != "invalid_content" {
		t.Fatalf("publication = %+v", p)
	}
}

func writeDelivery(q *queue, key string, attempt int) messaging.Delivery {
	for _, d := range deliveries(q, attempt) {
		if d.Topic == app.TopicYouTrackWrite && d.Key == key {
			return d
		}
	}
	panic("no write " + key)
}

func messagesOf(q *queue, topic string) []messaging.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []messaging.Message
	for _, m := range q.msgs {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

// KIN-107 AC3: all writes of one task share an ordering key, so the outbox delivers them in enqueue order: the final
// comment never lands before the state change enqueued with it.
func TestIssueSyncWritesOfATaskAreOrdered(t *testing.T) {
	t.Parallel()
	s, _, _, q := issueSync(ytConn{})
	ctx := t.Context()
	if err := s.State(ctx, "task/t1/v4/state", "t1", "p2", "KIN-7", "RUNNING"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t1/v4/final", "t1", "p2", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Article(ctx, "task/t1/e1/article/0", "t1", "p2", "KIN-7", "KIN-A-19", "S", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t2/v1/final", "t2", "p2", "KIN-8", "Done"); err != nil {
		t.Fatal(err)
	}
	if len(q.msgs) != 4 {
		t.Fatalf("messages = %+v", q.msgs)
	}
	for _, m := range q.msgs[:3] {
		if m.OrderingKey != app.WriteOrderingKey("t1") {
			t.Fatalf("%s ordering key = %q", m.Key, m.OrderingKey)
		}
	}
	if q.msgs[3].OrderingKey != app.WriteOrderingKey("t2") || app.WriteOrderingKey("t1") == app.WriteOrderingKey("t2") {
		t.Fatalf("another task shares the ordering key: %q", q.msgs[3].OrderingKey)
	}
}

// KIN-107 AC2: a write marked failed (rejected at enqueue, conflict, unauthorized, attempts exhausted) raises a
// project alert in the Alert Center; the next write of the task that is published closes it. Both alert messages
// share the task's ordering key, so a close never overtakes the failure it closes.
func TestIssueSyncFailedWriteRaisesAnAlertTheNextPublicationClosesIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, c, _, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v3/plan", "t1", "p1", "KIN-7", "Plan"); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleWrite(ctx, deliveries(q, 1)[0]); err != nil { // nothing failed yet: no alert at all
		t.Fatal(err)
	}
	if n := len(q.msgs); n != 1 {
		t.Fatalf("a publication without a failure enqueued alerts: %+v", q.msgs[1:])
	}
	if err := s.Comment(ctx, "task/t1/v4/final", "t1", "p1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	c.err = &app.StatusError{Status: 401}
	if err := s.HandleWrite(ctx, deliveries(q, 1)[1]); !messaging.IsPermanent(err) {
		t.Fatalf("unauthorized = %v", err)
	}
	failed := messagesOf(q, app.TopicYouTrackWriteFailed)
	if len(failed) != 1 || failed[0].Adapter != app.AdapterAlerts || failed[0].OrderingKey != app.AlertOrderingKey("t1") {
		t.Fatalf("failure alerts = %+v", failed)
	}
	var body map[string]string
	if err := json.Unmarshal(failed[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"projectId": "p1", "taskId": "t1", "issue": "KIN-7", "kind": "comment", "code": "unauthorized"}
	if !maps.Equal(body, want) {
		t.Fatalf("failure alert = %v, want %v", body, want)
	}

	c.err = nil
	if err := s.Comment(ctx, "task/t1/v5/note", "t1", "p1", "KIN-7", "Again"); err != nil {
		t.Fatal(err)
	}
	ds := deliveries(q, 1)
	if err := s.HandleWrite(ctx, ds[len(ds)-1]); err != nil {
		t.Fatal(err)
	}
	closed := messagesOf(q, app.TopicYouTrackWritePublished)
	if len(closed) != 1 || closed[0].OrderingKey != app.AlertOrderingKey("t1") ||
		!strings.Contains(string(closed[0].Payload), `"taskId":"t1"`) {
		t.Fatalf("close alerts = %+v", closed)
	}

	// Rejected at enqueue (in the caller's transaction) and exhausted attempts alert too.
	s2, c2, _, q2 := issueSync(ytConn{})
	if err := s2.Comment(ctx, "task/t2/v1/bad", "t2", "p1", "kin-x", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s2.Comment(ctx, "task/t2/v2/final", "t2", "p1", "KIN-8", "x"); err != nil {
		t.Fatal(err)
	}
	c2.err = &app.StatusError{Status: 503}
	s2.MaxAttempts = 3
	if err := s2.HandleWrite(ctx, writeDelivery(q2, "task/t2/v2/final", 3)); !messaging.IsPermanent(err) {
		t.Fatalf("last attempt = %v", err)
	}
	var codes []string
	for _, m := range messagesOf(q2, app.TopicYouTrackWriteFailed) {
		var b map[string]string
		_ = json.Unmarshal(m.Payload, &b)
		codes = append(codes, b["code"])
	}
	if strings.Join(codes, ",") != "invalid_issue,http_503" {
		t.Fatalf("failure alert codes = %v", codes)
	}
}

// Review finding: only the first write published after the task's latest failure closes the alert. A write enqueued
// before a failure (published later) does not close it, nor does every later publication enqueue a close again.
func TestIssueSyncOnlyAPublicationAfterTheFailureClosesTheAlert(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, _, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v4/plan", "t1", "p1", "KIN-7", "Plan"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t1/v5/final", "t1", "p1", "KIN-7", " "); err != nil { // rejected: invalid_text
		t.Fatal(err)
	}
	if err := s.HandleWrite(ctx, writeDelivery(q, "task/t1/v4/plan", 1)); err != nil {
		t.Fatal(err)
	}
	if n := len(messagesOf(q, app.TopicYouTrackWritePublished)); n != 0 {
		t.Fatalf("a write enqueued before the failure closed its alert (%d closes)", n)
	}
	for _, k := range []string{"task/t1/v6/a", "task/t1/v7/b"} {
		if err := s.Comment(ctx, k, "t1", "p1", "KIN-7", "x"); err != nil {
			t.Fatal(err)
		}
		if err := s.HandleWrite(ctx, writeDelivery(q, k, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if closes := messagesOf(q, app.TopicYouTrackWritePublished); len(closes) != 1 || closes[0].Key != "youtrack/task/t1/v6/a/published" {
		t.Fatalf("closes = %+v, want one for the first write after the failure", closes)
	}
}

// Review finding: an undecodable delivery fails its write (shown on the task) instead of leaving it pending.
func TestIssueSyncMalformedPayloadFailsTheWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, st, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v4/plan", "t1", "p1", "KIN-7", "Plan"); err != nil {
		t.Fatal(err)
	}
	d := writeDelivery(q, "task/t1/v4/plan", 1)
	d.Payload = []byte("{")
	if err := s.HandleWrite(ctx, d); !messaging.IsPermanent(err) {
		t.Fatalf("malformed = %v", err)
	}
	if status, code := st.status("task/t1/v4/plan"); status != domain.WriteFailed || code != "malformed" {
		t.Fatalf("write = %s %s", status, code)
	}
}

// KIN-108 AC1: an outbox message of a pending write that goes dead outside the handler (handler panic, no handler,
// a crash on the last attempt) fails the write with code "dead" and raises the write-back alert, once.
func TestIssueSyncDeadMessageFailsThePendingWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, st, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v4/final", "t1", "p1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	d := writeDelivery(q, "task/t1/v4/final", 10)
	for range 2 { // the hook is idempotent: a second call neither changes the write nor alerts again
		if err := s.HandleDead(ctx, d, errors.New("handler panic")); err != nil {
			t.Fatal(err)
		}
	}
	if status, code := st.status("task/t1/v4/final"); status != domain.WriteFailed || code != "dead" {
		t.Fatalf("write = %s %s, want failed dead", status, code)
	}
	failed := messagesOf(q, app.TopicYouTrackWriteFailed)
	if len(failed) != 1 || failed[0].OrderingKey != app.AlertOrderingKey("t1") {
		t.Fatalf("failure alerts = %+v, want one", failed)
	}
	var body map[string]string
	if err := json.Unmarshal(failed[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"projectId": "p1", "taskId": "t1", "issue": "KIN-7", "kind": "comment", "code": "dead"}
	if !maps.Equal(body, want) {
		t.Fatalf("failure alert = %v, want %v", body, want)
	}
}

// A write the handler already failed (its message then goes dead) or published keeps its outcome: no second alert.
func TestIssueSyncDeadMessageKeepsAFinishedWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, c, st, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v4/final", "t1", "p1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t1/v5/note", "t1", "p1", "KIN-7", "Note"); err != nil {
		t.Fatal(err)
	}
	c.err = &app.StatusError{Status: 401}
	d := writeDelivery(q, "task/t1/v4/final", 1)
	if err := s.HandleWrite(ctx, d); !messaging.IsPermanent(err) {
		t.Fatalf("unauthorized = %v", err)
	}
	if err := s.HandleDead(ctx, d, err401{}); err != nil {
		t.Fatal(err)
	}
	if status, code := st.status("task/t1/v4/final"); status != domain.WriteFailed || code != "unauthorized" {
		t.Fatalf("write = %s %s, want the handler's failure kept", status, code)
	}
	c.err = nil
	pub := writeDelivery(q, "task/t1/v5/note", 1)
	if err := s.HandleWrite(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDead(ctx, pub, errors.New("stale")); err != nil {
		t.Fatal(err)
	}
	if status, _ := st.status("task/t1/v5/note"); status != domain.WritePublished {
		t.Fatalf("published write = %s", status)
	}
	if n := len(messagesOf(q, app.TopicYouTrackWriteFailed)); n != 1 {
		t.Fatalf("failure alerts = %d, want only the handler's", n)
	}
	// An untracked key and an undecodable payload are no error (nothing to show; the outbox must not loop).
	if err := s.HandleDead(ctx, messaging.Delivery{Topic: app.TopicYouTrackWrite, Key: "task/x/none", Payload: []byte("{")},
		errors.New("x")); err != nil {
		t.Fatal(err)
	}
}

type err401 struct{}

func (err401) Error() string { return "401" }

// A write enqueued before the payload carried its project fails without an alert (KIN-107 rule).
func TestIssueSyncDeadLegacyWriteFailsWithoutAlert(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, st, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v4/final", "t1", "", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleDead(ctx, writeDelivery(q, "task/t1/v4/final", 10), errors.New("handler panic")); err != nil {
		t.Fatal(err)
	}
	if status, code := st.status("task/t1/v4/final"); status != domain.WriteFailed || code != "dead" {
		t.Fatalf("write = %s %s", status, code)
	}
	if n := len(messagesOf(q, app.TopicYouTrackWriteFailed)); n != 0 {
		t.Fatalf("failure alerts = %d, want none without a project", n)
	}
}

// KIN-108 AC2: retrying the dead message (outbox API, RetryUnhandled) puts its failed write back to pending.
func TestIssueSyncRetriedMessageReopensTheWrite(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, _, st, q := issueSync(ytConn{})
	if err := s.Comment(ctx, "task/t1/v4/final", "t1", "p1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t1/v5/note", "t1", "p1", "KIN-7", "Note"); err != nil {
		t.Fatal(err)
	}
	d := writeDelivery(q, "task/t1/v4/final", 0)
	if err := s.HandleDead(ctx, d, errors.New("handler panic")); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleRetried(ctx, d); err != nil {
		t.Fatal(err)
	}
	if status, code := st.status("task/t1/v4/final"); status != domain.WritePending || code != "dead" {
		t.Fatalf("write = %s %s, want pending with its last error", status, code)
	}
	if p, _ := s.Publication(ctx, "t1"); p.Pending != 2 || p.Failed != 0 {
		t.Fatalf("publication = %+v", p)
	}
	// A published write and an untracked key are left alone.
	pub := writeDelivery(q, "task/t1/v5/note", 1)
	if err := s.HandleWrite(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleRetried(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if status, _ := st.status("task/t1/v5/note"); status != domain.WritePublished {
		t.Fatalf("published write = %s", status)
	}
	if err := s.HandleRetried(ctx, messaging.Delivery{Key: "task/x/none"}); err != nil {
		t.Fatal(err)
	}
}
