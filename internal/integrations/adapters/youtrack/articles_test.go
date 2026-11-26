package youtrack_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func TestIssueDescriptionAndArticle(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path + "?" + r.URL.RawQuery {
		case "/yt/api/issues/KIN-34?fields=description":
			_, _ = w.Write([]byte(`{"description":"See KIN-A-15","$type":"Issue"}`))
		case "/yt/api/articles/KIN-A-15?fields=idReadable,summary,content":
			_, _ = w.Write([]byte(`{"idReadable":"KIN-A-15","summary":"Agents","content":"# Agents\ntext"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	secret, _ := domain.NewYouTrackToken(tok)
	c := &youtrack.Client{HTTP: srv.Client()}
	d, err := c.IssueDescription(t.Context(), srv.URL+"/yt", secret, "KIN-34")
	if err != nil || d != "See KIN-A-15" {
		t.Fatalf("description %q %v", d, err)
	}
	a, err := c.Article(t.Context(), srv.URL+"/yt", secret, "KIN-A-15")
	if err != nil || a.ID != "KIN-A-15" || a.Title != "Agents" || a.Content != "# Agents\ntext" {
		t.Fatalf("article %+v %v", a, err)
	}
	if _, err := c.Article(t.Context(), srv.URL+"/yt", secret, "KIN-A-99"); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Fatalf("missing article: %v", err)
	}
	if _, err := c.Article(t.Context(), srv.URL+"/yt", secret, "../admin"); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}
