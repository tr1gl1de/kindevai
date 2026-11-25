package github_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// fakeMerge is GitHub's checks and merge API for pull request 7 of acme/api (head h1 into dev).
type fakeMerge struct {
	mu     sync.Mutex
	head   string
	merges []map[string]any
	paths  []string
}

func (f *fakeMerge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer ghs_tok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /repos/acme/api/commits/h1/check-runs":
		if r.URL.Query().Get("filter") != "latest" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{"name": "lint", "status": "completed", "conclusion": "success"},
			{"name": "test", "status": "completed", "conclusion": "failure"}}})
	case "GET /repos/acme/api/pulls/7":
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "merged": false,
			"head": map[string]any{"ref": "feature/t1--s1", "sha": f.head}, "base": map[string]any{"ref": "dev"}})
	case "PUT /repos/acme/api/pulls/7/merge":
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.merges = append(f.merges, body)
		if body["sha"] != f.head {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "m1", "merged": true})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// KIN-37: the client reads a commit's latest check runs and a pull request, and merges it with a merge commit only
// while its head is the expected one (409 → ErrHeadMoved); a main/master base never reaches GitHub.
func TestChecksPullRequestAndMerge(t *testing.T) {
	t.Parallel()
	f := &fakeMerge{head: "h1"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := &github.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	tok, _ := domain.NewSecret("ghs_tok")
	ctx := t.Context()
	runs, total, err := c.CheckRuns(ctx, tok, "acme", "api", "h1")
	if st, detail := domain.CombineChecks(runs, total); err != nil || total != 2 || st != domain.CIStateFailed || detail != "failed checks: test (failure)" {
		t.Fatalf("checks %+v %v", runs, err)
	}
	pr, err := c.PullRequest(ctx, tok, "acme", "api", 7)
	if err != nil || pr.HeadSHA != "h1" || pr.Base != "dev" || pr.HeadRef != "feature/t1--s1" || pr.State != "open" {
		t.Fatalf("pull request %+v %v", pr, err)
	}
	sha, err := c.MergePullRequest(ctx, tok, "acme", "api", 7, "h1", "dev")
	if err != nil || sha != "m1" {
		t.Fatalf("merge %q %v", sha, err)
	}
	if m := f.merges[0]; m["merge_method"] != "merge" || m["sha"] != "h1" {
		t.Fatalf("merge body %+v", m)
	}
	f.mu.Lock()
	f.head = "h2"
	f.mu.Unlock()
	if _, err := c.MergePullRequest(ctx, tok, "acme", "api", 7, "h1", "dev"); !errors.Is(err, domain.ErrHeadMoved) {
		t.Fatalf("moved head: %v", err)
	}
	sent := len(f.paths)
	for _, base := range []string{"main", "master", "refs/heads/Main"} {
		if _, err := c.MergePullRequest(ctx, tok, "acme", "api", 7, "h2", base); !errors.Is(err, domain.ErrProtectedBranch) {
			t.Fatalf("base %s: %v", base, err)
		}
	}
	if len(f.paths) != sent {
		t.Fatalf("a protected merge reached GitHub: %v", f.paths[sent:])
	}
}
