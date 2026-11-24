package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/projects/app"
	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

var people = map[string]app.UserRef{
	"owner": {ID: "u-owner", Login: "owner", Owner: true},
	"alice": {ID: "u-alice", Login: "alice"},
	"bob":   {ID: "u-bob", Login: "bob"},
}

type dir struct{}

func (dir) ByLogin(_ context.Context, l string) (app.UserRef, error) {
	if u, ok := people[l]; ok {
		return u, nil
	}
	return app.UserRef{}, domain.ErrUserNotFound
}

func (dir) ByIDs(_ context.Context, ids []string) (map[string]app.UserRef, error) {
	out := map[string]app.UserRef{}
	for _, u := range people {
		for _, id := range ids {
			if u.ID == id {
				out[id] = u
			}
		}
	}
	return out, nil
}

// stack serves the projects API; the test principal comes from X-Test-User (stands in for the identity middleware).
func stack(t *testing.T) http.Handler {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, u := range people {
		role := map[bool]string{true: "owner", false: "user"}[u.Owner]
		if _, err := d.ExecContext(t.Context(), "INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES (?, ?, 'h', ?, 'x')", u.ID, u.Login, role); err != nil {
			t.Fatal(err)
		}
	}
	var n atomic.Int64
	svc := &app.Service{Repo: sqlite.New(d), Users: dir{}, Now: time.Now, NewID: func() string { return fmt.Sprint("p", n.Add(1)) }}
	mux := http.NewServeMux()
	(&Handler{Service: svc}).Register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := people[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{UserID: u.ID, Login: u.Login, Owner: u.Owner}))
		}
		mux.ServeHTTP(w, r)
	})
}

func do(t *testing.T, h http.Handler, user, method, target, body string) (int, map[string]any, []any) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Test-User", user)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var obj map[string]any
	var arr []any
	_ = json.Unmarshal(rec.Body.Bytes(), &obj)
	_ = json.Unmarshal(rec.Body.Bytes(), &arr)
	return rec.Code, obj, arr
}

func seed(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	h := stack(t)
	code, web, _ := do(t, h, "owner", "POST", "/api/projects", `{"name":"Web","slug":"web"}`)
	if code != 201 {
		t.Fatalf("create web = %d %v", code, web)
	}
	_, api, _ := do(t, h, "owner", "POST", "/api/projects", `{"name":"API","slug":"api"}`)
	webID, apiID := web["id"].(string), api["id"].(string)
	if code, body, _ := do(t, h, "owner", "PUT", "/api/projects/"+webID+"/members/alice", `{"role":"project_admin"}`); code != 200 {
		t.Fatalf("add alice = %d %v", code, body)
	}
	if code, _, _ := do(t, h, "owner", "PUT", "/api/projects/"+apiID+"/members/bob", `{"role":"member"}`); code != 200 {
		t.Fatalf("add bob = %d", code)
	}
	return h, webID, apiID
}

func TestProjectsAPI(t *testing.T) {
	t.Parallel()
	h, webID, _ := seed(t)
	if code, _, list := do(t, h, "owner", "GET", "/api/projects", ""); code != 200 || len(list) != 2 {
		t.Fatalf("owner list = %d %v", code, list)
	}
	code, _, list := do(t, h, "alice", "GET", "/api/projects", "")
	if code != 200 || len(list) != 1 || list[0].(map[string]any)["myRole"] != "project_admin" {
		t.Fatalf("alice list = %d %v", code, list)
	}
	if code, p, _ := do(t, h, "alice", "GET", "/api/projects/"+webID, ""); code != 200 || p["slug"] != "web" {
		t.Fatalf("alice get web = %d %v", code, p)
	}
	if code, _, ms := do(t, h, "alice", "GET", "/api/projects/"+webID+"/members", ""); code != 200 || len(ms) != 1 {
		t.Fatalf("members = %d %v", code, ms)
	}
	if code, body, _ := do(t, h, "owner", "POST", "/api/projects", `{"name":"Dup","slug":"web"}`); code != 409 || body["error"] != "slug_taken" {
		t.Fatalf("dup slug = %d %v", code, body)
	}
	if code, body, _ := do(t, h, "owner", "POST", "/api/projects", `{"name":"","slug":"x1"}`); code != 422 || body["field"] != "name" {
		t.Fatalf("validation = %d %v", code, body)
	}
	if code, _, _ := do(t, h, "owner", "DELETE", "/api/projects/"+webID+"/members/alice", ""); code != 204 {
		t.Fatalf("remove = %d", code)
	}
	if code, _, _ := do(t, h, "alice", "GET", "/api/projects/"+webID, ""); code != 404 {
		t.Fatalf("removed member get = %d", code)
	}
}

// Cross-project authorisation: a user never reaches another project's data or membership API, and cannot
// tell a foreign project from a missing one.
func TestCrossProjectAuthorization(t *testing.T) {
	t.Parallel()
	h, webID, apiID := seed(t)
	tests := []struct {
		name, user, method, target, body string
		want                             int
	}{
		{"alice reads api", "alice", "GET", "/api/projects/" + apiID, "", 404},
		{"alice reads api members", "alice", "GET", "/api/projects/" + apiID + "/members", "", 404},
		{"alice joins api", "alice", "PUT", "/api/projects/" + apiID + "/members/alice", `{"role":"member"}`, 404},
		{"alice removes bob from api", "alice", "DELETE", "/api/projects/" + apiID + "/members/bob", "", 404},
		{"missing looks the same", "alice", "GET", "/api/projects/does-not-exist", "", 404},
		{"project admin grants membership", "alice", "PUT", "/api/projects/" + webID + "/members/bob", `{"role":"member"}`, 403},
		{"project admin self-removes", "alice", "DELETE", "/api/projects/" + webID + "/members/alice", "", 403},
		{"member creates project", "bob", "POST", "/api/projects", `{"name":"X","slug":"xx"}`, 403},
		{"bob reads web", "bob", "GET", "/api/projects/" + webID, "", 404},
		{"anonymous lists", "", "GET", "/api/projects", "", 401},
		{"owner role via membership", "owner", "PUT", "/api/projects/" + webID + "/members/bob", `{"role":"owner"}`, 422},
		{"unknown user", "owner", "PUT", "/api/projects/" + webID + "/members/ghost", `{"role":"member"}`, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if code, body, _ := do(t, h, tt.user, tt.method, tt.target, tt.body); code != tt.want {
				t.Fatalf("= %d %v, want %d", code, body, tt.want)
			}
		})
	}
	if _, foreign, _ := do(t, h, "alice", "GET", "/api/projects/"+apiID, ""); fmt.Sprint(foreign) != fmt.Sprint(func() map[string]any {
		_, m, _ := do(t, h, "alice", "GET", "/api/projects/does-not-exist", "")
		return m
	}()) {
		t.Fatal("foreign and missing projects answer differently")
	}
}
