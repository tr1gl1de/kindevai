package github_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// fakePulls is GitHub's pulls API for acme/api: open pull requests by head branch; a second create of the same
// head answers 422 like GitHub ("A pull request already exists").
type fakePulls struct {
	mu      sync.Mutex
	open    map[string]int // head branch -> number
	bases   map[int]string
	creates []map[string]any
	hidden  bool // the list does not show the open pull requests (a create raced with another one)
}

func (f *fakePulls) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer ghs_tok" || r.URL.Path != "/repos/acme/api/pulls" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	pr := func(n int, head string) map[string]any {
		return map[string]any{"number": n, "html_url": "https://github.com/acme/api/pull/" + string(rune('0'+n)),
			"head": map[string]any{"ref": head, "sha": "h1"}, "base": map[string]any{"ref": f.bases[n]}}
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		out := []map[string]any{}
		head := strings.TrimPrefix(q.Get("head"), "acme:")
		if n, ok := f.open[head]; ok && !f.hidden && q.Get("state") == "open" && strings.HasPrefix(q.Get("head"), "acme:") &&
			(q.Get("base") == "" || q.Get("base") == f.bases[n]) {
			out = append(out, pr(n, head))
		}
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodPost:
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.creates = append(f.creates, body)
		head, _ := body["head"].(string)
		if _, ok := f.open[head]; ok {
			f.hidden = false
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		n := len(f.open) + 1
		f.open[head], f.bases[n] = n, body["base"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(pr(n, head))
	}
}

func pullsClient(t *testing.T, f *fakePulls) *github.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &github.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// KIN-36: opening a pull request of a branch reuses the open one (idempotent across outbox redeliveries), creates
// it into the given base otherwise, and survives a create that lost a race (422).
func TestOpenPullRequestCreatesOrReusesTheBranchsPullRequest(t *testing.T) {
	t.Parallel()
	f := &fakePulls{open: map[string]int{}, bases: map[int]string{}}
	c := pullsClient(t, f)
	tok, _ := domain.NewSecret("ghs_tok")
	in := domain.PullRequestInput{Base: "dev", Head: "feature/t1--s1", Title: "KIN-9: Fix", Body: "body"}
	pr, err := c.OpenPullRequest(t.Context(), tok, "acme", "api", in)
	if err != nil || pr.Number != 1 || pr.Reused || pr.Base != "dev" || !strings.HasPrefix(pr.URL, "https://github.com/") {
		t.Fatalf("%+v %v", pr, err)
	}
	if b := f.creates[0]; b["base"] != "dev" || b["head"] != "feature/t1--s1" || b["title"] != "KIN-9: Fix" {
		t.Fatalf("create body %v", b)
	}
	again, err := c.OpenPullRequest(t.Context(), tok, "acme", "api", in)
	if err != nil || again.Number != 1 || !again.Reused || len(f.creates) != 1 {
		t.Fatalf("again %+v %v (%d creates)", again, err, len(f.creates))
	}
	// An open pull request of the branch into another base is not ours: the create is refused (422) and nothing is
	// adopted.
	if _, err := c.OpenPullRequest(t.Context(), tok, "acme", "api", domain.PullRequestInput{Base: "release", Head: "feature/t1--s1",
		Title: "t"}); err == nil {
		t.Fatal("adopted a pull request into another base")
	}
	f.hidden = true // the list misses it, the create answers 422: the pull request is found after all
	raced, err := c.OpenPullRequest(t.Context(), tok, "acme", "api", in)
	if err != nil || raced.Number != 1 || !raced.Reused {
		t.Fatalf("raced %+v %v", raced, err)
	}
}

// KIN-36: a pull request into main/master is refused before any request is sent.
func TestOpenPullRequestRefusesAProtectedBase(t *testing.T) {
	t.Parallel()
	f := &fakePulls{open: map[string]int{}, bases: map[int]string{}}
	c := pullsClient(t, f)
	for _, base := range []string{"main", "master", "refs/heads/Main"} {
		tok, _ := domain.NewSecret("ghs_tok")
		_, err := c.OpenPullRequest(t.Context(), tok, "acme", "api",
			domain.PullRequestInput{Base: base, Head: "feature/x", Title: "t"})
		if !errors.Is(err, domain.ErrProtectedBranch) {
			t.Errorf("%s: %v", base, err)
		}
	}
	if len(f.creates) != 0 {
		t.Fatal("a pull request was created")
	}
}
