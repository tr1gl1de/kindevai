package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/audit/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/redact"
)

type memRepo struct {
	mu     sync.Mutex
	events []domain.Event
	last   domain.Filter
}

func (m *memRepo) Append(_ context.Context, e domain.Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.events) + 1)
	m.events = append(m.events, e)
	return e.ID, nil
}

// Query honours only Before and Limit; the SQLite adapter tests cover the other filters.
func (m *memRepo) Query(_ context.Context, f domain.Filter) ([]domain.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = f
	var out []domain.Event
	for i := len(m.events) - 1; i >= 0 && len(out) < f.Limit; i-- {
		if f.Before == 0 || m.events[i].ID < f.Before {
			out = append(out, m.events[i])
		}
	}
	return out, nil
}

func (m *memRepo) Kinds(context.Context) ([]string, error) { return []string{"user.created"}, nil }

var (
	owner = auth.Principal{UserID: "u-owner", Login: "owner", Owner: true}
	bob   = auth.Principal{UserID: "u-bob", Login: "bob"}
	now   = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
)

func newService() (*Service, *memRepo) {
	r := &memRepo{}
	return &Service{Repo: r, Now: func() time.Time { return now }}, r
}

func TestRecordStoresEventWithTimeAndActor(t *testing.T) {
	t.Parallel()
	s, r := newService()
	err := s.Record(t.Context(), Entry{Actor: UserActor(owner), Kind: "invitation.created", ProjectID: "p1",
		Subject: "invitation:i1", Details: map[string]string{"role": "member"}})
	if err != nil {
		t.Fatal(err)
	}
	e := r.events[0]
	if !e.At.Equal(now) || e.Actor != (Actor{Type: domain.ActorUser, ID: "u-owner", Label: "owner"}) ||
		e.Kind != "invitation.created" || e.ProjectID != "p1" || e.Details["role"] != "member" {
		t.Fatalf("event = %+v", e)
	}
	if err := s.Record(t.Context(), Entry{Actor: UserActor(owner), Kind: "bad"}); !errors.Is(err, domain.ErrInvalidEvent) {
		t.Fatalf("invalid kind = %v", err)
	}
}

func TestRecordRedactsSecretLikeValues(t *testing.T) {
	t.Parallel()
	s, r := newService()
	ghToken := "ghp_" + strings.Repeat("aB3", 12)
	err := s.Record(t.Context(), Entry{Actor: System(), Kind: "integration.configured", Details: map[string]string{
		"password":    "correct horse battery staple",
		"api_key":     "plain-looking",
		"header":      "Bearer abc.def.ghi",
		"token_value": ghToken,
		"note":        "rotated to " + ghToken + " by owner",
		"botToken":    "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw0",
		"remote":      "https://x:secret-pass@github.com/a/b.git",
		"login":       "alice",
		"repo":        "https://github.com/a/b",
	}, Subject: "credential:" + ghToken})
	if err != nil {
		t.Fatal(err)
	}
	e := r.events[0]
	for _, k := range []string{"password", "api_key", "header", "token_value", "botToken", "remote"} {
		if e.Details[k] != redact.Placeholder {
			t.Errorf("details[%s] = %q, want redacted", k, e.Details[k])
		}
	}
	if strings.Contains(e.Details["note"], ghToken) || !strings.HasPrefix(e.Details["note"], "rotated to ") {
		t.Errorf("note = %q", e.Details["note"])
	}
	if strings.Contains(e.Subject, ghToken) {
		t.Errorf("subject = %q", e.Subject)
	}
	if e.Details["login"] != "alice" || e.Details["repo"] != "https://github.com/a/b" {
		t.Errorf("plain values changed: %+v", e.Details)
	}
}

func TestQueryIsOwnerOnlyAndPaginates(t *testing.T) {
	t.Parallel()
	s, r := newService()
	for range 5 {
		if err := s.Record(t.Context(), Entry{Actor: UserActor(owner), Kind: "user.created"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Query(t.Context(), bob, domain.Filter{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner query = %v", err)
	}
	if _, err := s.Kinds(t.Context(), bob); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner kinds = %v", err)
	}
	p, err := s.Query(t.Context(), owner, domain.Filter{Limit: 2})
	if err != nil || len(p.Events) != 2 || p.Events[0].ID != 5 || p.Next != 4 {
		t.Fatalf("page 1 = %+v, %v", p, err)
	}
	p, _ = s.Query(t.Context(), owner, domain.Filter{Limit: 2, Before: p.Next})
	if len(p.Events) != 2 || p.Events[0].ID != 3 || p.Next != 2 {
		t.Fatalf("page 2 = %+v", p)
	}
	p, _ = s.Query(t.Context(), owner, domain.Filter{Limit: 2, Before: p.Next})
	if len(p.Events) != 1 || p.Next != 0 {
		t.Fatalf("last page = %+v", p)
	}
	if _, err := s.Query(t.Context(), owner, domain.Filter{Limit: -1}); !errors.Is(err, domain.ErrInvalidFilter) {
		t.Fatalf("bad filter = %v", err)
	}
	if r.last.Limit != 3 {
		t.Fatalf("repository asked for %d rows, want limit+1 = 3", r.last.Limit)
	}
}
