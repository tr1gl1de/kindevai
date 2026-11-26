package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/audit/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func setup(t *testing.T) http.Handler {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	clock := t0
	svc := &app.Service{Repo: sqlite.New(d), Now: func() time.Time { return clock }}
	owner := auth.Principal{UserID: "u1", Login: "owner", Owner: true}
	for i, e := range []app.Entry{
		{Actor: app.UserActor(owner), Kind: "user.created", Subject: "user:u2", Details: map[string]string{"login": "carol"}},
		{Actor: app.UserActor(owner), Kind: "invitation.created", ProjectID: "p1", Subject: "invitation:i1"},
		{Actor: app.User("u2", "carol"), Kind: "invitation.accepted", ProjectID: "p1", Subject: "invitation:i1"},
		{Actor: app.UserActor(owner), Kind: "invitation.created", ProjectID: "p2", Subject: "invitation:i2",
			Details: map[string]string{"token": "should-never-show"}},
	} {
		clock = t0.Add(time.Duration(i) * time.Hour)
		if err := svc.Record(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	(&httpapi.Handler{Service: svc}).Register(mux)
	users := map[string]auth.Principal{"owner": owner, "alice": {UserID: "u3", Login: "alice"}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := users[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(w, r)
	})
}

func get(t *testing.T, h http.Handler, user, target string, out any) int {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	r.Header.Set("X-Test-User", user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if out != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: %v: %s", target, err, w.Body)
		}
	}
	return w.Code
}

func eventIDs(p httpapi.PageView) []int64 {
	out := []int64{}
	for _, e := range p.Events {
		out = append(out, e.ID)
	}
	return out
}

func TestAuditAccess(t *testing.T) {
	t.Parallel()
	h := setup(t)
	if c := get(t, h, "", "/api/audit", nil); c != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", c)
	}
	if c := get(t, h, "alice", "/api/audit", nil); c != http.StatusForbidden {
		t.Fatalf("non-owner = %d, want 403", c)
	}
	if c := get(t, h, "alice", "/api/audit/kinds", nil); c != http.StatusForbidden {
		t.Fatalf("non-owner kinds = %d, want 403", c)
	}
	if c := get(t, h, "alice", "/api/audit?project=p1", nil); c != http.StatusForbidden {
		t.Fatalf("non-owner with a project filter = %d, want 403", c)
	}
}

func TestAuditFiltersAndPagination(t *testing.T) {
	t.Parallel()
	h := setup(t)
	var all httpapi.PageView
	if c := get(t, h, "owner", "/api/audit", &all); c != http.StatusOK || len(all.Events) != 4 || all.Next != nil {
		t.Fatalf("all = %d %+v", c, all)
	}
	e := all.Events[0]
	if e.Kind != "invitation.created" || e.ProjectID != "p2" || e.Actor.Label != "owner" || e.Actor.Type != "user" ||
		!e.At.Equal(t0.Add(3*time.Hour)) || e.Details["token"] != "[redacted]" {
		t.Fatalf("newest = %+v", e)
	}
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"project=p1", []int64{3, 2}},
		{"project=p2", []int64{4}},
		{"actor=carol", []int64{3}},
		{"kind=invitation", []int64{4, 3, 2}},
		{"kind=user.created", []int64{1}},
		{"from=" + url.QueryEscape(t0.Add(time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(t0.Add(3*time.Hour).Format(time.RFC3339)), []int64{3, 2}},
	} {
		var p httpapi.PageView
		if c := get(t, h, "owner", "/api/audit?"+tc.query, &p); c != http.StatusOK {
			t.Fatalf("%s: status %d", tc.query, c)
		}
		if got := eventIDs(p); len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) || got[len(got)-1] != tc.want[len(tc.want)-1] {
			t.Errorf("%s: ids = %v, want %v", tc.query, got, tc.want)
		}
	}

	var p1 httpapi.PageView
	get(t, h, "owner", "/api/audit?limit=3", &p1)
	if len(p1.Events) != 3 || p1.Next == nil || *p1.Next != "2" {
		t.Fatalf("page 1 = %+v", p1)
	}
	var p2 httpapi.PageView
	get(t, h, "owner", "/api/audit?limit=3&before="+*p1.Next, &p2)
	if got := eventIDs(p2); len(got) != 1 || got[0] != 1 || p2.Next != nil {
		t.Fatalf("page 2 = %+v", p2)
	}

	for _, bad := range []string{"limit=x", "limit=-1", "before=abc", "from=yesterday", "to=2026-13-01", "kind=Bad%20Kind",
		"from=2026-11-25T00:00:00Z&to=2026-11-24T00:00:00Z"} {
		if c := get(t, h, "owner", "/api/audit?"+bad, nil); c != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, c)
		}
	}

	var kinds []string
	if c := get(t, h, "owner", "/api/audit/kinds", &kinds); c != http.StatusOK || len(kinds) != 3 || kinds[0] != "invitation.accepted" {
		t.Fatalf("kinds = %d %v", c, kinds)
	}
}

func TestNoMutatingRoutes(t *testing.T) {
	t.Parallel()
	h := setup(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, target := range []string{"/api/audit", "/api/audit/1"} {
			r := httptest.NewRequestWithContext(t.Context(), m, target, nil)
			r.Header.Set("X-Test-User", "owner")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code < 400 {
				t.Errorf("%s %s = %d, want an error: the audit log has no write API", m, target, w.Code)
			}
		}
	}
}
