package youtrack_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// KIN-38: the write-back requests YouTrack receives, as the connection's user.
func TestSyncClientRequests(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		b, _ := json.Marshal(body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/issues/KIN-7" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"comments":[` + strings.Repeat(`{"id":"1"},`, 249) + `{"id":"1"}]}`))
		case r.URL.Path == "/api/issues/KIN-7/comments" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"text":"a"},{"text":"b kindev-1"}]`))
		case r.URL.Path == "/api/articles/KIN-A-19" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"content":"# T","updated":1700}`))
		case r.URL.Path == "/api/articles/KIN-A-404":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"x"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := &youtrack.Client{HTTP: srv.Client()}
	ctx := t.Context()
	token, _ := domain.NewYouTrackToken(tok)
	texts, err := c.IssueComments(ctx, srv.URL, token, "KIN-7")
	if err != nil || len(texts) != 2 || texts[1] != "b kindev-1" {
		t.Fatalf("comments = %q, %v", texts, err)
	}
	if err := c.AddComment(ctx, srv.URL, token, "KIN-7", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyCommand(ctx, srv.URL, token, "KIN-7", "State {In Progress}"); err != nil {
		t.Fatal(err)
	}
	rev, err := c.ArticleRevision(ctx, srv.URL, token, "KIN-A-19")
	if err != nil || rev != (app.ArticleRevision{Content: "# T", Updated: 1700}) {
		t.Fatalf("revision = %+v, %v", rev, err)
	}
	if err := c.UpdateArticle(ctx, srv.URL, token, "KIN-A-19", "# New"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ArticleRevision(ctx, srv.URL, token, "KIN-A-404"); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Fatalf("missing article: %v", err)
	}
	if err := c.UpdateArticle(ctx, srv.URL, token, "../x", "y"); err == nil {
		t.Fatal("a malformed article id was sent")
	}
	if err := c.AddComment(ctx, srv.URL, token, "kin 7/../x", "y"); err == nil {
		t.Fatal("a malformed issue id was sent")
	}
	want := []string{
		"GET /api/issues/KIN-7?fields=comments(id) null",
		"GET /api/issues/KIN-7/comments?%24skip=50&%24top=200&fields=text null",
		`POST /api/issues/KIN-7/comments?fields=id {"text":"hello"}`,
		`POST /api/commands?fields=id {"issues":[{"idReadable":"KIN-7"}],"query":"State {In Progress}"}`,
		"GET /api/articles/KIN-A-19?fields=content,updated null",
		`POST /api/articles/KIN-A-19?fields=id {"content":"# New"}`,
		"GET /api/articles/KIN-A-404?fields=content,updated null",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s", strings.Join(got, "\n"))
	}
}
