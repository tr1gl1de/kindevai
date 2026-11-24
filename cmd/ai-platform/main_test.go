package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
)

func TestServeHealthAndShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan net.Addr, 1)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	dataDir := filepath.Join(t.TempDir(), "data")
	go func() {
		done <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir}, io.Discard, &stderr, ready)
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr.String()+"/api/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("health = %d %s", resp.StatusCode, body)
	}

	for _, p := range []string{bootstrap.DBPath(dataDir), bootstrap.KeyPath(dataDir)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("bootstrap did not create %s: %v", p, err)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRunCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		wantOut string
	}{
		{"no command", nil, true, ""},
		{"unknown", []string{"bogus"}, true, ""},
		{"version", []string{"version"}, false, "dev"},
		{"help", []string{"help"}, false, "serve"},
		{"bad flag", []string{"serve", "--nope"}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := run(t.Context(), tt.args, &out, io.Discard, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestServeBadAddr(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), []string{"serve", "--addr", "256.0.0.1:99999", "--data-dir", t.TempDir()}, io.Discard, io.Discard, nil)
	if err == nil {
		t.Fatal("want listen error")
	}
}

func TestServeFailsWhenKeyMissingForExistingDB(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	inst, err := bootstrap.Run(t.Context(), bootstrap.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	_ = inst.Close()
	if err := os.Remove(bootstrap.KeyPath(dataDir)); err != nil {
		t.Fatal(err)
	}
	err = run(t.Context(), []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "encryption key") {
		t.Fatalf("err = %v, want a clear encryption key error", err)
	}
}

// startServe runs `serve` on a random port and returns its base URL and a stop function.
func startServe(t *testing.T, dataDir string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir}, io.Discard, io.Discard, ready)
	}()
	select {
	case addr := <-ready:
		stop := func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
		return "http://" + addr.String(), stop
	case err := <-done:
		cancel()
		t.Fatalf("serve exited: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("serve did not start")
	}
	return "", nil
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFirstRunSetupEndToEnd(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	base, stop := startServe(t, dataDir)
	tokenFile := filepath.Join(dataDir, "secrets", "setup-token")
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("setup token file: %v", err)
	}
	if code, body := call(t, "GET", base+"/api/setup/status", ""); code != 200 || !strings.Contains(body, `"setupRequired":true`) {
		t.Fatalf("status = %d %s", code, body)
	}
	payload := `{"token":"` + strings.TrimSpace(string(tok)) + `","login":"owner","password":"correct horse battery","platformName":"Acme"}`
	if code, body := call(t, "POST", base+"/api/setup", payload); code != 201 {
		t.Fatalf("setup = %d %s", code, body)
	}
	stop()

	base, stop = startServe(t, dataDir)
	defer stop()
	if code, body := call(t, "GET", base+"/api/setup/status", ""); !strings.Contains(body, `"setupRequired":false`) {
		t.Fatalf("status after restart = %d %s", code, body)
	}
	if code, _ := call(t, "POST", base+"/api/setup", payload); code != 409 {
		t.Fatalf("setup after restart = %d, want 409", code)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup token file still present after setup")
	}
}

func TestLoginAfterSetupEndToEnd(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	base, stop := startServe(t, dataDir)
	defer stop()
	tok, _ := os.ReadFile(filepath.Join(dataDir, "secrets", "setup-token"))
	setup := `{"token":"` + strings.TrimSpace(string(tok)) + `","login":"owner","password":"correct horse battery","platformName":"Acme"}`
	if code, body := call(t, "POST", base+"/api/setup", setup); code != 201 {
		t.Fatalf("setup = %d %s", code, body)
	}
	if code, _ := call(t, "GET", base+"/api/auth/me", ""); code != 401 {
		t.Fatalf("me without session = %d", code)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	post := func(path, body string) int {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	get := func(path string) int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", base+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/api/auth/login", `{"login":"owner","password":"correct horse battery"}`); code != 200 {
		t.Fatalf("login = %d", code)
	}
	if code := get("/api/auth/me"); code != 200 {
		t.Fatalf("me = %d", code)
	}
	if code := post("/api/projects", `{"name":"Web","slug":"web"}`); code != 201 {
		t.Fatalf("create project = %d", code)
	}
	if code := get("/api/projects"); code != 200 {
		t.Fatalf("list projects = %d", code)
	}
	if code := get("/api/integrations"); code != 200 {
		t.Fatalf("integrations settings: %d", code)
	}
	if code := get("/api/integrations/status"); code != 200 {
		t.Fatalf("integrations status = %d (messaging module not wired)", code)
	}
	if code := post("/api/users", `{"login":"carol"}`); code != 201 {
		t.Fatalf("create user = %d", code)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", base+"/api/audit?kind=user", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(audit), `"user.created"`) || !strings.Contains(string(audit), `"carol"`) {
		t.Fatalf("audit = %d %s (audit module not wired or user creation not recorded)", resp.StatusCode, audit)
	}
	if code := post("/api/auth/logout", ""); code != 204 {
		t.Fatalf("logout = %d", code)
	}
	if code := get("/api/auth/me"); code != 401 {
		t.Fatalf("me after logout = %d", code)
	}
}

func TestHealthCommand(t *testing.T) {
	t.Parallel()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(ok.Close)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"healthy", []string{"health", "--url", ok.URL + "/api/health"}, false},
		{"unhealthy", []string{"health", "--url", bad.URL + "/api/health"}, true},
		{"unreachable", []string{"health", "--url", "http://127.0.0.1:1/api/health", "--timeout", "1s"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := run(t.Context(), tt.args, io.Discard, io.Discard, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSetupTokenCommand(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	var out bytes.Buffer
	if err := run(t.Context(), []string{"setup-token", "--data-dir", dataDir}, &out, io.Discard, nil); err == nil {
		t.Fatal("want an error when no setup token exists")
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets", "setup-token"), []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"setup-token", "--data-dir", dataDir}, &out, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "tok-123\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestServeRejectsBadTrustedProxies(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", t.TempDir(), "--trusted-proxies", "nope"}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "trusted proxy") {
		t.Fatalf("err = %v", err)
	}
}

// A runner enrolls through the real middleware without a session; the Owner sees it pending and approves it.
func TestRunnerEnrollmentEndToEnd(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	base, stop := startServe(t, dataDir)
	defer stop()
	tok, _ := os.ReadFile(filepath.Join(dataDir, "secrets", "setup-token"))
	setup := `{"token":"` + strings.TrimSpace(string(tok)) + `","login":"owner","password":"correct horse battery","platformName":"Acme"}`
	if code, body := call(t, "POST", base+"/api/setup", setup); code != 201 {
		t.Fatalf("setup = %d %s", code, body)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	owner := func(method, path, body string) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := owner("POST", "/api/auth/login", `{"login":"owner","password":"correct horse battery"}`); code != 200 {
		t.Fatalf("login = %d", code)
	}
	code, body := owner("POST", "/api/projects", `{"name":"Web","slug":"web"}`)
	var project struct{ ID string }
	if err := json.Unmarshal([]byte(body), &project); code != 201 || err != nil {
		t.Fatalf("create project = %d %s", code, body)
	}
	code, body = owner("POST", "/api/runners/enrollment-tokens", "")
	var created struct{ Token string }
	if err := json.Unmarshal([]byte(body), &created); code != 201 || err != nil || created.Token == "" {
		t.Fatalf("create token = %d %s", code, body)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	enroll := `{"token":"` + created.Token + `","publicKey":"` + base64.StdEncoding.EncodeToString(pub) + `","hostname":"wsl-box","os":"linux","arch":"amd64","version":"0.1.0"}`
	code, body = call(t, "POST", base+"/api/runners/enroll", enroll) // no session
	var reg struct{ RunnerID, Status string }
	if err := json.Unmarshal([]byte(body), &reg); code != 201 || err != nil || reg.Status != "pending" {
		t.Fatalf("enroll = %d %s", code, body)
	}
	if code, _ := call(t, "GET", base+"/api/runners", ""); code != 401 {
		t.Fatalf("runners list without session = %d", code)
	}
	if code, body := owner("POST", "/api/runners/"+reg.RunnerID+"/approve", `{"projectIds":["`+project.ID+`"]}`); code != 200 || !strings.Contains(body, `"approved"`) {
		t.Fatalf("approve = %d %s", code, body)
	}
}
