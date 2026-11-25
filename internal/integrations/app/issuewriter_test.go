package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type ytConn struct {
	err      error
	bindings map[string]string
}

func (c ytConn) YouTrackSettings(context.Context) (app.YouTrackSettings, error) {
	if c.err != nil {
		return app.YouTrackSettings{}, c.err
	}
	tok, _ := domain.NewYouTrackToken("perm:abc")
	return app.YouTrackSettings{BaseURL: "https://yt.example", Token: tok}, nil
}

func (c ytConn) YouTrackProjectOf(_ context.Context, id string) (string, error) {
	return c.bindings[id], nil
}

type issueClient struct {
	mu       sync.Mutex
	created  []app.NewIssue
	existing map[string]string // marker → issue id
	err      error
	lost     bool // the issue is created but the response is lost
}

func (c *issueClient) ProjectID(_ context.Context, _ string, _ domain.Secret, short string) (string, error) {
	if short == "GONE" {
		return "", domain.ErrNotFound
	}
	return "0-" + short, nil
}

func (c *issueClient) FindIssue(_ context.Context, _ string, _ domain.Secret, short, marker string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.existing[marker]
	return id, ok, nil
}

func (c *issueClient) CreateIssue(_ context.Context, _ string, _ domain.Secret, projectID string, n app.NewIssue) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	c.created = append(c.created, n)
	id := "SHOP-" + string(rune('0'+len(c.created)))
	if c.existing == nil {
		c.existing = map[string]string{}
	}
	c.existing[n.Marker] = id
	if c.lost {
		c.lost = false
		return "", errors.New("connection reset")
	}
	return id, nil
}

type created struct {
	mu   sync.Mutex
	seen []string
}

func (c *created) record(_ context.Context, key, id, url string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, key+" "+id+" "+url)
	return nil
}

type queue struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (q *queue) Enqueue(_ context.Context, m messaging.Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.msgs = append(q.msgs, m)
	return nil
}

func writer() (*app.IssueWriter, *issueClient, *created, *queue) {
	c, cr, q := &issueClient{}, &created{}, &queue{}
	return &app.IssueWriter{Connection: ytConn{bindings: map[string]string{"p1": "SHOP"}}, Client: c, Outbox: q,
		OnCreated: cr.record}, c, cr, q
}

func req() app.IssueRequest {
	return app.IssueRequest{Key: "alert/A1/analysis", Project: "SHOP", Summary: "Analyze alert: CI failed",
		Description: "CI failed.\n\nkindevai-alert-A1", Marker: "kindevai-alert-A1"}
}

func delivery(t *testing.T, q *queue, attempt int) messaging.Delivery {
	t.Helper()
	if len(q.msgs) != 1 {
		t.Fatalf("messages = %+v", q.msgs)
	}
	m := q.msgs[0]
	return messaging.Delivery{Adapter: m.Adapter, Topic: m.Topic, Key: m.Key, Payload: m.Payload, Attempt: attempt}
}

// AC: the request goes to the outbox (never to YouTrack inline) with a per-alert idempotency key.
func TestRequestIssueEnqueuesCreateIssue(t *testing.T) {
	t.Parallel()
	w, c, _, q := writer()
	if err := w.RequestIssue(t.Context(), req()); err != nil {
		t.Fatal(err)
	}
	m := q.msgs[0]
	if m.Adapter != app.AdapterYouTrack || m.Topic != app.TopicYouTrackCreateIssue || m.Key != "create_issue/alert/A1/analysis" {
		t.Fatalf("message = %+v", m)
	}
	var p map[string]string
	if err := json.Unmarshal(m.Payload, &p); err != nil || p["project"] != "SHOP" || p["marker"] != "kindevai-alert-A1" {
		t.Fatalf("payload = %s", m.Payload)
	}
	if len(c.created) != 0 {
		t.Fatal("YouTrack was called inline")
	}
	for name, r := range map[string]app.IssueRequest{
		"no key":     {Project: "SHOP", Summary: "s", Marker: "m"},
		"bad short":  {Key: "k", Project: "shop x", Summary: "s", Marker: "m"},
		"no summary": {Key: "k", Project: "SHOP", Marker: "m"},
		"bad marker": {Key: "k", Project: "SHOP", Summary: "s", Marker: "a b"},
	} {
		if err := w.RequestIssue(t.Context(), r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if ok, err := w.Configured(t.Context()); !ok || err != nil {
		t.Fatalf("configured = %v, %v", ok, err)
	}
	if s, err := w.Binding(t.Context(), "p1"); s != "SHOP" || err != nil {
		t.Fatalf("binding = %q, %v", s, err)
	}
}

// The handler creates the issue and reports it; a retry after a lost response finds it instead of duplicating.
func TestHandleCreateIssueIsIdempotent(t *testing.T) {
	t.Parallel()
	w, c, cr, q := writer()
	ctx := t.Context()
	if err := w.RequestIssue(ctx, req()); err != nil {
		t.Fatal(err)
	}
	c.lost = true
	if err := w.HandleCreateIssue(ctx, delivery(t, q, 1)); err == nil || messaging.IsPermanent(err) {
		t.Fatalf("lost response = %v, want a retry", err)
	}
	if err := w.HandleCreateIssue(ctx, delivery(t, q, 2)); err != nil {
		t.Fatal(err)
	}
	if err := w.HandleCreateIssue(ctx, delivery(t, q, 3)); err != nil {
		t.Fatal(err)
	}
	if len(c.created) != 1 {
		t.Fatalf("created %d issues", len(c.created))
	}
	if !strings.Contains(c.created[0].Description, "kindevai-alert-A1") || c.created[0].Summary != "Analyze alert: CI failed" {
		t.Fatalf("issue = %+v", c.created[0])
	}
	want := "alert/A1/analysis SHOP-1 https://yt.example/issue/SHOP-1"
	if len(cr.seen) != 2 || cr.seen[0] != want || cr.seen[1] != want {
		t.Fatalf("reported = %q", cr.seen)
	}
}

// Failures that a retry cannot fix are permanent; outages are retried.
func TestHandleCreateIssueErrors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cases := map[string]struct {
		setup     func(w *app.IssueWriter, c *issueClient)
		permanent bool
	}{
		"not configured": {func(w *app.IssueWriter, _ *issueClient) { w.Connection = ytConn{err: domain.ErrNotConfigured} }, true},
		"no token":       {func(w *app.IssueWriter, _ *issueClient) { w.Connection = ytConn{err: domain.ErrSecretNotSet} }, true},
		"forbidden":      {func(_ *app.IssueWriter, c *issueClient) { c.err = &app.StatusError{Status: 403} }, true},
		"unavailable":    {func(_ *app.IssueWriter, c *issueClient) { c.err = &app.StatusError{Status: 503} }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, c, cr, q := writer()
			if err := w.RequestIssue(ctx, req()); err != nil {
				t.Fatal(err)
			}
			tc.setup(w, c)
			err := w.HandleCreateIssue(ctx, delivery(t, q, 1))
			if err == nil || messaging.IsPermanent(err) != tc.permanent {
				t.Fatalf("err = %v, permanent want %v", err, tc.permanent)
			}
			if len(cr.seen) != 0 {
				t.Fatalf("reported %q", cr.seen)
			}
		})
	}
	w, _, _, q := writer()
	r := req()
	r.Project = "GONE"
	if err := w.RequestIssue(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := w.HandleCreateIssue(ctx, delivery(t, q, 1)); !messaging.IsPermanent(err) {
		t.Fatalf("unknown project = %v", err)
	}
	if err := w.HandleCreateIssue(ctx, messaging.Delivery{Payload: []byte("{")}); !messaging.IsPermanent(err) {
		t.Fatalf("malformed = %v", err)
	}
}
