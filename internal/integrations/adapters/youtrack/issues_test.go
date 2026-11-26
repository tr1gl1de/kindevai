package youtrack_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

const issueJSON = `{"idReadable":"KIN-7","summary":"Login page","updated":1732450000123,"resolved":null,
	"reporter":{"login":"jdoe","$type":"User"},"tags":[{"name":"kindevai"},{"name":"ui"}],
	"project":{"shortName":"KIN"},"description":"ignored","$type":"Issue"}`

func TestUpdatedIssuesQueriesIncrementallyAndParsesSnapshots(t *testing.T) {
	t.Parallel()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/yt/api/issues" || r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		got = append(got, q.Get("query"), q.Get("$skip"), q.Get("$top"))
		if !strings.Contains(q.Get("fields"), "idReadable") {
			t.Errorf("fields = %q", q.Get("fields"))
		}
		_, _ = w.Write([]byte(`[` + issueJSON + `,{"idReadable":"KIN-8","summary":"Done","updated":1732450000999,
			"resolved":1732450000999,"reporter":null,"tags":[],"project":{"shortName":"KIN"}}]`))
	}))
	t.Cleanup(srv.Close)
	secret, _ := domain.NewYouTrackToken(tok)
	c := &youtrack.Client{HTTP: srv.Client(), Now: func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) }}
	since := time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC).UnixMilli()
	issues, err := c.UpdatedIssues(t.Context(), srv.URL+"/yt", secret, app.IssueQuery{ShortName: "KIN", Tag: "ai run", Since: since, Skip: 50, Top: 25})
	if err != nil {
		t.Fatal(err)
	}
	wantQuery := "project: {KIN} tag: {ai run} updated: 2026-11-19 .. 2026-11-25 sort by: updated asc"
	if len(got) != 3 || got[0] != wantQuery || got[1] != "50" || got[2] != "25" {
		t.Fatalf("request = %q", got)
	}
	if len(issues) != 2 || issues[0].ID != "KIN-7" || issues[0].Project != "KIN" || issues[0].Reporter != "jdoe" ||
		issues[0].Updated != 1732450000123 || len(issues[0].Tags) != 2 || issues[0].Resolved || !issues[1].Resolved ||
		issues[1].Reporter != "" {
		t.Fatalf("issues = %+v", issues)
	}
	// The first poll of a binding has no lower bound.
	if _, err := c.UpdatedIssues(t.Context(), srv.URL+"/yt", secret, app.IssueQuery{ShortName: "KIN", Tag: "kindevai", Top: 10}); err != nil {
		t.Fatal(err)
	}
	if got[3] != "project: {KIN} tag: {kindevai} sort by: updated asc" {
		t.Fatalf("first query = %q", got[3])
	}
}

func TestIssueAndErrors(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/issues/KIN-7", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(issueJSON)) })
	mux.HandleFunc("GET /api/issues/KIN-404", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("GET /api/issues/KIN-429", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(r.Header.Get("Authorization"))) // an echoed token must not reach the error text
	})
	mux.HandleFunc("GET /api/issues/KIN-401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	mux.HandleFunc("GET /api/issues/KIN-1", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	mux.HandleFunc("GET /api/issues/KIN-2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
	})
	mux.HandleFunc("GET /api/issues/KIN-3", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"idReadable":"KIN-3","summary":"` + strings.Repeat("x", 5<<20) + `"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	secret, _ := domain.NewYouTrackToken(tok)
	c := &youtrack.Client{HTTP: srv.Client()}
	s, err := c.Issue(t.Context(), srv.URL, secret, "KIN-7")
	if err != nil || s.ID != "KIN-7" || s.Project != "KIN" || s.Summary != "Login page" {
		t.Fatalf("issue %+v, %v", s, err)
	}
	if _, err := c.Issue(t.Context(), srv.URL, secret, "KIN-404"); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Fatalf("404: %v", err)
	}
	_, err = c.Issue(t.Context(), srv.URL, secret, "KIN-429")
	var se *app.StatusError
	if !errors.As(err, &se) || se.Status != 429 || se.RetryAfter != 2*time.Minute || se.Permanent() || strings.Contains(err.Error(), tok) {
		t.Fatalf("429: %v", err)
	}
	if _, err := c.Issue(t.Context(), srv.URL, secret, "KIN-401"); !errors.As(err, &se) || se.Status != 401 || !se.Permanent() {
		t.Fatalf("401: %v", err)
	}
	for _, id := range []string{"KIN-1", "KIN-2", "KIN-3"} {
		if _, err := c.Issue(t.Context(), srv.URL, secret, id); err == nil {
			t.Errorf("%s: accepted", id)
		}
	}
	if _, err := c.Issue(t.Context(), srv.URL, secret, "../admin"); err == nil {
		t.Error("an invalid issue ID reached the URL")
	}
}

// KIN-50: the tag history comes from the issue's activities (TagsCategory) with the author's login.
func TestTagEventsReadsTheTagHistory(t *testing.T) {
	t.Parallel()
	var query []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/issues/KIN-7/activities", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		query = append(query, q.Get("categories"), q.Get("fields"), q.Get("$top"), q.Get("reverse"))
		_, _ = w.Write([]byte(`[
			{"id":"1-1","timestamp":100,"author":{"login":"alice","$type":"User"},"added":[{"name":"kindevai","$type":"IssueTag"}],"removed":[],"$type":"TagsActivityItem"},
			{"id":"1-2","timestamp":200,"author":null,"added":null,"removed":[{"name":"ui"}],"$type":"TagsActivityItem"}]`))
	})
	mux.HandleFunc("GET /api/issues/KIN-404/activities", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("GET /api/issues/KIN-403/activities", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	mux.HandleFunc("GET /api/issues/KIN-1/activities", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"timestamp":100,"author":{"login":"alice"},"added":{"name":"kindevai"}}]`)) // not a list
	})
	mux.HandleFunc("GET /api/issues/KIN-2/activities", func(w http.ResponseWriter, _ *http.Request) {
		ev := `{"timestamp":100,"author":{"login":"alice"},"added":[{"name":"kindevai"}]}`
		_, _ = w.Write([]byte("[" + strings.Repeat(ev+",", domain.MaxTagEvents) + ev + "]")) // more than asked for
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	secret, _ := domain.NewYouTrackToken(tok)
	c := &youtrack.Client{HTTP: srv.Client()}

	events, err := c.TagEvents(t.Context(), srv.URL, secret, "KIN-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(query) != 4 || query[0] != "TagsCategory" || !strings.Contains(query[1], "author(login)") ||
		!strings.Contains(query[1], "added(name)") || !strings.Contains(query[1], "removed(name)") ||
		!strings.Contains(query[1], "timestamp") || query[2] != "101" || query[3] != "true" {
		t.Fatalf("request %q", query)
	}
	if len(events) != 2 || events[0].At != 100 || events[0].Author != "alice" || len(events[0].Added) != 1 ||
		events[0].Added[0] != "kindevai" || events[1].Author != "" || len(events[1].Removed) != 1 || events[1].Removed[0] != "ui" {
		t.Fatalf("events %+v", events)
	}
	if _, err := c.TagEvents(t.Context(), srv.URL, secret, "KIN-404"); !errors.Is(err, domain.ErrIssueNotFound) {
		t.Fatalf("404: %v", err)
	}
	var se *app.StatusError
	if _, err := c.TagEvents(t.Context(), srv.URL, secret, "KIN-403"); !errors.As(err, &se) || !se.Permanent() {
		t.Fatalf("403: %v", err)
	}
	for _, id := range []string{"KIN-1", "KIN-2"} {
		if _, err := c.TagEvents(t.Context(), srv.URL, secret, id); !errors.Is(err, app.ErrTagHistoryUnusable) {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := c.TagEvents(t.Context(), srv.URL, secret, "../admin"); err == nil {
		t.Error("an invalid issue ID reached the URL")
	}
}

// KIN-53: responses captured from a live YouTrack Cloud (2026-11) for the exact request TagEvents sends
// (categories=TagsCategory, reverse=true); logins redacted. A tag set in the body of POST /api/issues is recorded
// as a TagsActivityItem by the reporter a few ms after the issue's `created`, so creation-time tags need no
// fallback. reverse=true returns the newest change first, and a removal is its own item.
const (
	// The issue was created with the tag in the creation request (created = 1795552509701).
	liveTagAtCreation = `[{"removed":[],"added":[{"name":"kindevai","$type":"Tag"}],"author":{"login":"alice","$type":"User"},"id":"0-0.14-1243","timestamp":1795552509717,"$type":"TagsActivityItem"}]`
	// The same issue after the tag was removed and applied again by command.
	liveTagRemovedAndReapplied = `[{"removed":[],"added":[{"name":"kindevai","$type":"Tag"}],"author":{"login":"alice","$type":"User"},"id":"0-0.14-1247","timestamp":1795552523343,"$type":"TagsActivityItem"},` +
		`{"removed":[{"name":"kindevai","$type":"Tag"}],"added":[],"author":{"login":"alice","$type":"User"},"id":"0-0.14-1246","timestamp":1795552522385,"$type":"TagsActivityItem"},` +
		`{"removed":[],"added":[{"name":"kindevai","$type":"Tag"}],"author":{"login":"alice","$type":"User"},"id":"0-0.14-1243","timestamp":1795552509717,"$type":"TagsActivityItem"}]`
	// While the tag was off: the newest change is the removal.
	liveTagRemoved = `[{"removed":[{"name":"kindevai","$type":"Tag"}],"added":[],"author":{"login":"alice","$type":"User"},"id":"0-0.14-1246","timestamp":1795552522385,"$type":"TagsActivityItem"},` +
		`{"removed":[],"added":[{"name":"kindevai","$type":"Tag"}],"author":{"login":"alice","$type":"User"},"id":"0-0.14-1243","timestamp":1795552509717,"$type":"TagsActivityItem"}]`
)

func TestTagEventsContractWithLiveYouTrackResponses(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{"KIN-1": liveTagAtCreation, "KIN-2": liveTagRemovedAndReapplied, "KIN-3": liveTagRemoved}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+tok ||
			q.Get("categories") != "TagsCategory" || q.Get("reverse") != "true" {
			w.WriteHeader(http.StatusBadRequest) // not the request the responses were captured for
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/issues/"), "/activities")
		_, _ = w.Write([]byte(bodies[id]))
	}))
	t.Cleanup(srv.Close)
	secret, _ := domain.NewYouTrackToken(tok)
	c := &youtrack.Client{HTTP: srv.Client()}
	cases := []struct {
		id, want string
		events   int
	}{
		{"KIN-1", "alice", 1}, // tag applied at creation is in the history
		{"KIN-2", "alice", 3}, // newest re-add wins
		{"KIN-3", "", 2},      // removed: no applier (fail closed)
	}
	for _, tc := range cases {
		events, err := c.TagEvents(t.Context(), srv.URL, secret, tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if len(events) != tc.events {
			t.Fatalf("%s: events %+v, want %d", tc.id, events, tc.events)
		}
		for i := 1; i < len(events); i++ {
			if events[i-1].At <= events[i].At {
				t.Fatalf("%s: events %+v, want newest first (server order kept)", tc.id, events)
			}
		}
		got, ok := domain.TagApplier(events, "kindevai")
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: applier %q %v, want %q", tc.id, got, ok, tc.want)
		}
	}
}
