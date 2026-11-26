package app

import (
	"context"
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

type fakeTextSource struct {
	desc     string
	descErr  error
	articles map[string]domain.Article
	calls    []string
}

func (f *fakeTextSource) IssueDescription(_ context.Context, _ string, _ domain.Secret, id string) (string, error) {
	f.calls = append(f.calls, "issue "+id)
	return f.desc, f.descErr
}

func (f *fakeTextSource) Article(_ context.Context, _ string, _ domain.Secret, id string) (domain.Article, error) {
	f.calls = append(f.calls, "article "+id)
	a, ok := f.articles[id]
	if !ok {
		return a, domain.ErrIssueNotFound
	}
	return a, nil
}

func settings(err error) func(context.Context) (YouTrackSettings, error) {
	return func(context.Context) (YouTrackSettings, error) { return YouTrackSettings{BaseURL: "https://yt"}, err }
}

func TestIssueContextReadsTheDescriptionAndLinkedArticles(t *testing.T) {
	t.Parallel()
	src := &fakeTextSource{desc: "Spec: KIN-A-15, KIN-A-14, KIN-A-9, KIN-A-1, KIN-A-15",
		articles: map[string]domain.Article{"KIN-A-15": {ID: "KIN-A-15", Title: "Agents", Content: "text"},
			"KIN-A-9": {ID: "KIN-A-9", Title: "Auth"}}}
	c, err := IssueContextReader{Settings: settings(nil), Source: src}.Read(t.Context(), "KIN-34")
	if err != nil {
		t.Fatal(err)
	}
	if c.Description != src.desc || len(c.Articles) != 2 || c.Articles[0].ID != "KIN-A-15" || len(c.Missing) != 1 ||
		c.Missing[0] != "KIN-A-14" || len(src.calls) != 1+MaxContextArticles {
		t.Fatalf("context %+v calls %v", c, src.calls)
	}
}

func TestIssueContextFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		settings error
		desc     error
		final    bool
	}{
		"not connected": {settings: domain.ErrNotConfigured, final: true},
		"no token":      {settings: domain.ErrSecretNotSet, final: true},
		"gone":          {desc: domain.ErrIssueNotFound, final: true},
		"forbidden":     {desc: &StatusError{Status: 403}, final: true},
		"rate limited":  {desc: &StatusError{Status: 429}},
		"network":       {desc: errors.New("youtrack: connection refused")},
	} {
		_, err := IssueContextReader{Settings: settings(tc.settings), Source: &fakeTextSource{descErr: tc.desc}}.Read(t.Context(), "KIN-34")
		if err == nil || errors.Is(err, ErrContextUnavailable) != tc.final {
			t.Errorf("%s: %v", name, err)
		}
	}
}
