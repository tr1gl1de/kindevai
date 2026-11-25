package youtrack_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// A fake YouTrack: project lookup, issue search and creation.
func writeServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var created []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/projects":
			if q := r.URL.Query().Get("query"); q != "SHOP" && q != "NONE" {
				t.Errorf("project query = %q", r.URL.Query().Get("query"))
			}
			_, _ = w.Write([]byte(`[{"id":"0-9","shortName":"SHOPX"},{"id":"0-1","shortName":"SHOP","$type":"Project"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/issues":
			if q := r.URL.Query().Get("query"); q != `project: {SHOP} "kindevai-alert-A1"` {
				t.Errorf("search = %q", q)
			}
			_, _ = w.Write([]byte(`[{"idReadable":"SHOP-1","description":"mentions kindevai-alert-A12 only"},
				{"idReadable":"SHOP-2","description":"x\n\nkindevai-alert-A1"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content type = %q", r.Header.Get("Content-Type"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			created = append(created, body)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"idReadable":"SHOP-3","$type":"Issue"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &created
}

func TestCreateIssueResolvesTheProjectAndPosts(t *testing.T) {
	t.Parallel()
	srv, created := writeServer(t)
	secret, _ := domain.NewYouTrackToken(tok)
	c := &youtrack.Client{HTTP: srv.Client()}
	ctx := t.Context()
	pid, err := c.ProjectID(ctx, srv.URL, secret, "SHOP")
	if err != nil || pid != "0-1" {
		t.Fatalf("project id = %q, %v", pid, err)
	}
	if _, err := c.ProjectID(ctx, srv.URL, secret, "NONE"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project = %v", err)
	}
	id, err := c.CreateIssue(ctx, srv.URL, secret, pid, app.NewIssue{Summary: "Analyze", Description: "body\n\nkindevai-alert-A1"})
	if err != nil || id != "SHOP-3" {
		t.Fatalf("create = %q, %v", id, err)
	}
	got := (*created)[0]
	if got["summary"] != "Analyze" || got["description"] != "body\n\nkindevai-alert-A1" ||
		got["project"].(map[string]any)["id"] != "0-1" {
		t.Fatalf("body = %+v", got)
	}
	// Only an exact marker matches (A12 is another alert).
	id, ok, err := c.FindIssue(ctx, srv.URL, secret, "SHOP", "kindevai-alert-A1")
	if err != nil || !ok || id != "SHOP-2" {
		t.Fatalf("find = %q %v %v", id, ok, err)
	}
	bad, _ := domain.NewYouTrackToken("perm:wrong")
	var se *app.StatusError
	if _, err := c.CreateIssue(ctx, srv.URL, bad, pid, app.NewIssue{Summary: "x"}); !errors.As(err, &se) || se.Status != 401 {
		t.Fatalf("unauthorized = %v", err)
	}
}
