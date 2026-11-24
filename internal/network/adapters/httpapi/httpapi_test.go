package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/network/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/network/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/network/app"
	"github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

type vault struct {
	mu sync.Mutex
	m  map[string]string
}

func (v *vault) Put(_ context.Context, n, s, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[n] = s
	return nil
}
func (v *vault) Get(_ context.Context, n string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.m[n]
	return s, ok, nil
}
func (v *vault) IsSet(ctx context.Context, n string) (bool, error) {
	_, ok, err := v.Get(ctx, n)
	return ok, err
}
func (v *vault) Delete(_ context.Context, n string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, n)
	return nil
}

type runners struct{}

func (runners) Runner(_ context.Context, id string) (app.RunnerRef, error) {
	if id != "r1" {
		return app.RunnerRef{}, domain.ErrNotFound
	}
	return app.RunnerRef{ID: "r1", Name: "box", Active: true}, nil
}
func (r runners) Runners(ctx context.Context) ([]app.RunnerRef, error) {
	x, _ := r.Runner(ctx, "r1")
	return []app.RunnerRef{x}, nil
}

type projects struct{}

func (projects) Exists(context.Context, string) error { return nil }

type commands struct {
	mu   sync.Mutex
	sent []string
}

func (c *commands) Send(_ context.Context, runnerID, id, kind string, body any) error {
	b, _ := json.Marshal(body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, runnerID+" "+kind+" "+string(b))
	return nil
}

var principals = map[string]auth.Principal{
	"owner":  {UserID: "owner1", Login: "owner", Owner: true},
	"member": {UserID: "u2", Login: "bob"},
}

func server(t *testing.T) (*httptest.Server, *commands) {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	var k secrets.Key
	_, _ = rand.Read(k[:])
	box, _ := secrets.NewBox(k)
	cmds := &commands{}
	svc := &app.Service{Repo: sqlite.New(d), Secrets: &vault{m: map[string]string{}}, Cipher: box, Runners: runners{},
		Projects: projects{}, Commands: cmds, Tx: db.Transactor{DB: d}, Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now},
		Now: time.Now, NewID: func() string { return strings.ToLower(rand.Text()) }}
	mux := http.NewServeMux()
	(&httpapi.Handler{Service: svc}).Register(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-User")]; ok {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, cmds
}

func call(t *testing.T, srv *httptest.Server, user, method, path string, body any) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", user)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	return res.StatusCode, buf.String()
}

func TestProfileAPINeverReturnsThePassword(t *testing.T) {
	t.Parallel()
	srv, _ := server(t)
	code, body := call(t, srv, "owner", "POST", "/api/network/profiles", map[string]any{"name": "Corp",
		"proxyUrl": "http://alice:t0p-s3cret@proxy.example:3128", "enforcement": "strict", "expectedIpv4": "203.0.113.7"})
	if code != http.StatusCreated || strings.Contains(body, "t0p-s3cret") || !strings.Contains(body, `"passwordSet":true`) ||
		!strings.Contains(body, `"url":"http://alice@proxy.example:3128"`) {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, body = call(t, srv, "owner", "GET", "/api/network/profiles", nil); code != 200 || strings.Contains(body, "t0p-s3cret") {
		t.Fatalf("list = %d %s", code, body)
	}
	var ps []httpapi.ProfileJSON
	_ = json.Unmarshal([]byte(body), &ps)
	if len(ps) != 1 || !ps[0].FailClosed || ps[0].DNS != "proxy" {
		t.Fatalf("profiles = %+v", ps)
	}
}

func TestProfileAPIErrors(t *testing.T) {
	t.Parallel()
	srv, _ := server(t)
	if code, _ := call(t, srv, "member", "GET", "/api/network/profiles", nil); code != http.StatusForbidden {
		t.Fatalf("member list = %d", code)
	}
	if code, _ := call(t, srv, "", "GET", "/api/network/profiles", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", code)
	}
	code, body := call(t, srv, "owner", "POST", "/api/network/profiles", map[string]any{"name": "x",
		"proxyUrl": "ftp://u:pw-secret@h:1"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "proxy_scheme") || strings.Contains(body, "pw-secret") {
		t.Fatalf("bad proxy = %d %s", code, body)
	}
	code, body = call(t, srv, "owner", "POST", "/api/network/profiles", map[string]any{"name": "x",
		"allow": []map[string]any{{"cidr": "0.0.0.0/0", "ports": []int{443}}}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "allow_cidr_too_wide") {
		t.Fatalf("wide allow = %d %s", code, body)
	}
}

func TestAssignmentAndTestNetworkAPI(t *testing.T) {
	t.Parallel()
	srv, cmds := server(t)
	_, body := call(t, srv, "owner", "POST", "/api/network/profiles", map[string]any{"name": "Sys",
		"proxy": map[string]any{"scheme": "socks5", "host": "proxy.example", "port": 1080}, "password": "pa55word",
		"enforcement": "strict"})
	var p httpapi.ProfileJSON
	_ = json.Unmarshal([]byte(body), &p)
	if code, body := call(t, srv, "owner", "PUT", "/api/network/assignments", map[string]any{"level": "system", "profileId": p.ID}); code != http.StatusNoContent {
		t.Fatalf("assign = %d %s", code, body)
	}
	if code, body := call(t, srv, "owner", "DELETE", "/api/network/profiles/"+p.ID, nil); code != http.StatusConflict || !strings.Contains(body, "in_use") {
		t.Fatalf("delete assigned = %d %s", code, body)
	}
	code, body := call(t, srv, "owner", "GET", "/api/network/effective?runnerId=r1&projectId=p1&role=claude", nil)
	if code != 200 || !strings.Contains(body, `"enforcement":"strict"`) || !strings.Contains(body, `"strictFrom":"system"`) {
		t.Fatalf("effective = %d %s", code, body)
	}
	code, body = call(t, srv, "owner", "POST", "/api/network/tests", map[string]any{"runnerId": "r1"})
	if code != http.StatusAccepted || !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("test = %d %s", code, body)
	}
	if code, body = call(t, srv, "owner", "GET", "/api/network/tests?runnerId=r1", nil); code != 200 || !strings.Contains(body, `"runnerId":"r1"`) {
		t.Fatalf("tests = %d %s", code, body)
	}
	if code, _ := call(t, srv, "owner", "POST", "/api/network/tests", map[string]any{"runnerId": "nope"}); code != http.StatusNotFound {
		t.Fatalf("unknown runner = %d", code)
	}
	cmds.mu.Lock()
	defer cmds.mu.Unlock()
	if len(cmds.sent) != 2 || !strings.Contains(cmds.sent[1], "network.test") {
		t.Fatalf("commands = %v", cmds.sent)
	}
	for _, s := range cmds.sent {
		if strings.Contains(s, "pa55word") {
			t.Fatal("plaintext password in a queued command")
		}
	}
}
