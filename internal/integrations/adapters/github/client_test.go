package github_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func creds(t *testing.T) app.AppCredentials {
	t.Helper()
	s, err := domain.NewPrivateKey(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey())})))
	if err != nil {
		t.Fatal(err)
	}
	return app.AppCredentials{AppID: 42, PrivateKey: s}
}

// fakeGitHub is an httptest double of the endpoints the client uses. It verifies the App JWT with the public key.
type fakeGitHub struct {
	t         *testing.T
	mu        sync.Mutex
	mints     int
	mintBody  []string
	installed bool
	appStatus int
	rules     map[string]bool // branch -> ruleset applies
	branches  map[string]bool // branch -> exists (classic protection off)
	hits      []string
}

func (f *fakeGitHub) jwtOK(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	return json.Unmarshal(claims, &c) == nil && c.Iss == "42" && c.Exp-c.Iat <= 600
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	token := r.Header.Get("Authorization") == "Bearer ghs_fake-installation-token"
	switch {
	case r.URL.Path == "/app":
		if f.appStatus != 0 {
			w.WriteHeader(f.appStatus)
			return
		}
		if !f.jwtOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"slug":"kindev-bot","id":42}`)
	case r.URL.Path == "/repos/acme/api/installation":
		if !f.jwtOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !f.installed {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"id":77}`)
	case r.URL.Path == "/app/installations/77/access_tokens" && r.Method == http.MethodPost:
		if !f.jwtOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mints++
		f.mintBody = append(f.mintBody, string(b))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"ghs_fake-installation-token","expires_at":"2026-11-24T13:00:00Z"}`)
	case !token:
		w.WriteHeader(http.StatusUnauthorized)
	case r.URL.Path == "/repos/acme/api":
		_, _ = io.WriteString(w, `{"full_name":"acme/api","default_branch":"main"}`)
	case strings.HasPrefix(r.URL.Path, "/repos/acme/api/branches/"):
		b := strings.TrimPrefix(r.URL.Path, "/repos/acme/api/branches/")
		if !f.branches[b] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"name":"`+b+`","protected":false}`)
	case strings.HasPrefix(r.URL.Path, "/repos/acme/api/rules/branches/"):
		if f.rules[strings.TrimPrefix(r.URL.Path, "/repos/acme/api/rules/branches/")] {
			_, _ = io.WriteString(w, `[{"type":"pull_request","ruleset_id":1}]`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func newClient(t *testing.T, f *fakeGitHub) (*github.Client, *time.Time) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	now := t0
	return &github.Client{BaseURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return now }}, &now
}

func TestCheckApp(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, &fakeGitHub{})
	if res := c.CheckApp(t.Context(), creds(t)); !res.OK || res.Detail != "kindev-bot" {
		t.Fatalf("check: %+v", res)
	}
	bad, _ := newClient(t, &fakeGitHub{appStatus: http.StatusUnauthorized})
	if res := bad.CheckApp(t.Context(), creds(t)); res.OK || res.Code != "unauthorized" || res.Detail != "HTTP 401" {
		t.Fatalf("unauthorized: %+v", res)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	down := &github.Client{BaseURL: closed.URL, HTTP: &http.Client{Transport: &http.Transport{}}, Now: func() time.Time { return t0 }}
	if res := down.CheckApp(t.Context(), creds(t)); res.Code != "unreachable" || strings.Contains(res.Detail, "127.0.0.1") {
		t.Fatalf("unreachable: %+v", res)
	}
}

func TestReadRepository(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{installed: true, branches: map[string]bool{"main": true, "master": true}, rules: map[string]bool{"master": true}}
	c, _ := newClient(t, f)
	res := c.ReadRepository(t.Context(), creds(t), "acme", "api")
	if !res.OK || res.State.InstallationID != 77 || res.State.DefaultBranch != "main" {
		t.Fatalf("read: %+v", res)
	}
	st, open := res.State.Protection()
	if st != domain.ProtectionUnprotected || len(open) != 1 || open[0] != "main" {
		t.Fatalf("protection: %s %v (%+v)", st, open, res.State.Branches)
	}
	if strings.Contains(res.Detail, "ghs_") {
		t.Fatal("token in result")
	}

	f.rules["main"] = true
	c2, _ := newClient(t, f)
	if res := c2.ReadRepository(t.Context(), creds(t), "acme", "api"); !res.OK || res.Detail != "protected" {
		t.Fatalf("protected: %+v", res)
	}

	none, _ := newClient(t, &fakeGitHub{})
	if res := none.ReadRepository(t.Context(), creds(t), "acme", "api"); res.OK || res.Code != "not_installed" {
		t.Fatalf("not installed: %+v", res)
	}
}

func TestInstallationTokenIsScopedAndCachedInMemoryBelowTTL(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{installed: true}
	c, now := newClient(t, f)
	ctx := t.Context()
	for range 3 {
		tok, err := c.InstallationToken(ctx, creds(t), 77, "api")
		if err != nil || tok.Reveal() != "ghs_fake-installation-token" {
			t.Fatalf("token: %v", err)
		}
	}
	if f.mints != 1 || f.mintBody[0] != `{"permissions":{"checks":"read","contents":"write","metadata":"read","pull_requests":"write"},"repositories":["api"]}` {
		t.Fatalf("mints = %d, bodies %v: want one, restricted to the repository", f.mints, f.mintBody)
	}
	// The token expires at 13:00; it is cached for at most 50 minutes (and never within 5 minutes of expiry).
	*now = t0.Add(49 * time.Minute)
	if _, err := c.InstallationToken(ctx, creds(t), 77, "api"); err != nil || f.mints != 1 {
		t.Fatalf("still cached: %d %v", f.mints, err)
	}
	*now = t0.Add(51 * time.Minute)
	if _, err := c.InstallationToken(ctx, creds(t), 77, "api"); err != nil || f.mints != 2 {
		t.Fatalf("near expiry must mint again: %d %v", f.mints, err)
	}
	// Another repository gets its own token.
	if _, err := c.InstallationToken(ctx, creds(t), 77, "web"); err != nil || f.mints != 3 {
		t.Fatalf("other repository: %d %v", f.mints, err)
	}
}

func TestInstallationTokenErrorsCarryNoCredentials(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, &fakeGitHub{})
	_, err := c.InstallationToken(t.Context(), creds(t), 99, "api")
	if err == nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("err = %v", err)
	}
}

func TestGuardRefusesWritesToMainAndMaster(t *testing.T) {
	t.Parallel()
	for name, prefix := range map[string]string{"github.com": "", "enterprise server": "/api/v3"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeGitHub{}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			hc := &http.Client{Transport: github.Guard{Next: srv.Client().Transport, Prefix: prefix}}
			do := func(ctx context.Context, method, path, body string) error {
				req, _ := http.NewRequestWithContext(ctx, method, srv.URL+prefix+path, strings.NewReader(body))
				resp, err := hc.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
				return err
			}
			ctx := t.Context()
			for _, tc := range [][3]string{
				{"POST", "/repos/acme/api/pulls", `{"head":"feature/x","base":"main"}`},
				{"POST", "/repos/acme/api/pulls", `{"head":"feature/x"}`},
				{"PATCH", "/repos/acme/api/pulls/3", `{"base":"Master"}`},
				{"PATCH", "/repos/acme/api/pulls/3", `{"base":7}`},
				{"POST", "/repos/acme/api/merges", `{"base":"master","head":"dev"}`},
				{"PATCH", "/repos/acme/api/git/refs/heads/main", `{"sha":"abc","force":true}`},
				{"DELETE", "/repos/acme/api/git/refs/heads/master", ``},
				{"PATCH", "/repos/acme/api/git/refs/heads/x/../main", `{"sha":"abc"}`},
				{"POST", "/repos/acme/api/git/refs", `{"ref":"refs/heads/main","sha":"abc"}`},
				{"PUT", "/repos/acme/api/contents/README.md", `{"message":"x","content":"eA==","branch":"main"}`},
				{"PUT", "/repos/acme/api/contents/README.md", `{"message":"x","content":"eA=="}`}, // default branch
				{"PUT", "/repos/acme/api/branches/main/protection", `{}`},
				{"POST", "/repos/acme/api/branches/master/rename", `{"new_name":"dev"}`},
				{"POST", "/repos/acme/api/git/refs/heads/%6Dain", `{}`},
				{"PATCH", "/repos/acme/api/git/refs/heads/%6Dain", `{}`},
				{"PUT", "/repos/acme/api/pulls/3/merge", `{}`}, // base not checked
				{"POST", "/graphql", `{"query":"mutation{mergePullRequest}"}`},
				{"PATCH", "/repos/acme/api", `{"default_branch":"dev"}`},
				{"POST", "/repos/acme/api/pulls", `not json`},
			} {
				if err := do(ctx, tc[0], tc[1], tc[2]); !errors.Is(err, domain.ErrProtectedBranch) {
					t.Errorf("%s %s %s: err = %v", tc[0], tc[1], tc[2], err)
				}
			}
			if _, err := github.AllowMerge(ctx, "main"); !errors.Is(err, domain.ErrProtectedBranch) {
				t.Errorf("AllowMerge(main) = %v", err)
			}
			f.mu.Lock()
			if len(f.hits) != 0 {
				t.Errorf("refused writes reached GitHub: %v", f.hits)
			}
			f.mu.Unlock()
			mergeCtx, err := github.AllowMerge(ctx, "dev")
			if err != nil {
				t.Fatal(err)
			}
			allowed := []struct {
				ctx             context.Context
				method, path, b string
			}{
				{ctx, "POST", "/repos/acme/api/pulls", `{"head":"feature/x","base":"dev"}`},
				{ctx, "GET", "/repos/acme/api/branches/main", ``},
				{ctx, "PATCH", "/repos/acme/api/git/refs/heads/feature/main", `{"sha":"abc"}`},
				{ctx, "PUT", "/repos/acme/api/contents/a.txt", `{"branch":"feature/x"}`},
				{mergeCtx, "PUT", "/repos/acme/api/pulls/3/merge", `{}`},
				{ctx, "POST", "/app/installations/77/access_tokens", `{}`},
			}
			for _, tc := range allowed {
				if err := do(tc.ctx, tc.method, tc.path, tc.b); err != nil {
					t.Errorf("%s %s: %v", tc.method, tc.path, err)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.hits) != len(allowed) {
				t.Errorf("allowed requests: %v", f.hits)
			}
		})
	}
}
