package app_test

import (
	"context"
	"errors"
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
	if err := s.Comment(ctx, "task/t1/v3/plan", "t1", "KIN-7", "Plan revision 1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Comment(ctx, "task/t1/v3/plan", "t1", "kin-x", "x"); err == nil {
		t.Fatal("a malformed issue id was accepted")
	}
	if len(c.comments) != 0 {
		t.Fatal("YouTrack was called inline")
	}
	m := q.msgs[0]
	if len(q.msgs) != 1 || m.Adapter != app.AdapterYouTrack || m.Topic != app.TopicYouTrackWrite || m.Key != "task/t1/v3/plan" {
		t.Fatalf("messages = %+v", q.msgs)
	}
	if p, _ := s.Publication(ctx, "t1"); p != (domain.Publication{Pending: 1}) {
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
	if p, _ := s.Publication(ctx, "t1"); p != (domain.Publication{Published: 1}) {
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
	if err := s.Article(ctx, "task/t1/v9/article/KIN-A-19", "t1", "KIN-7", "KIN-A-19", "KIN-38", "Write-back."); err != nil {
		t.Fatal(err)
	}
	if err := s.Article(ctx, "k", "t1", "KIN-7", "../admin", "S", "x"); err == nil {
		t.Fatal("a malformed article id was accepted")
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
	if err := s2.Article(ctx, "task/t2/v1/article/KIN-A-19", "t2", "KIN-8", "KIN-A-19", "S", "x"); err != nil {
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
	if err := s.Comment(ctx, "task/t1/v3/final", "t1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	c.err = &app.StatusError{Status: 503}
	if err := s.HandleWrite(ctx, deliveries(q, 1)[0]); err == nil || messaging.IsPermanent(err) {
		t.Fatalf("outage = %v, want a retry", err)
	}
	if p, _ := s.Publication(ctx, "t1"); p != (domain.Publication{Pending: 1, LastError: "http_503"}) {
		t.Fatalf("publication = %+v", p)
	}
	c.err = &app.StatusError{Status: 403}
	if err := s.HandleWrite(ctx, deliveries(q, 2)[0]); !messaging.IsPermanent(err) {
		t.Fatalf("forbidden = %v, want permanent", err)
	}
	if status, code := st.status("task/t1/v3/final"); status != domain.WriteFailed || code != "unauthorized" {
		t.Fatalf("write = %s %s", status, code)
	}

	s2, _, st2, q2 := issueSync(ytConn{err: domain.ErrNotConfigured})
	if err := s2.Comment(ctx, "task/t3/v1/final", "t3", "KIN-9", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := s2.HandleWrite(ctx, deliveries(q2, 1)[0]); !messaging.IsPermanent(err) {
		t.Fatalf("not configured = %v", err)
	}
	if status, code := st2.status("task/t3/v1/final"); status != domain.WriteFailed || code != "not_configured" {
		t.Fatalf("write = %s %s", status, code)
	}
}
