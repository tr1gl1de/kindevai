package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnershttp "github.com/tr1gl1de/kindevai/internal/runners/adapters/httpapi"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
)

// controlPlane serves the real runners API of the Control Plane over httptest and issues enrollment tokens.
type controlPlane struct {
	url string
	svc *runnersapp.Service
	hub *runnersapp.Hub
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(),
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner1', 'owner', 'h', 'owner', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(),
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')"); err != nil {
		t.Fatal(err)
	}
	hub := runnersapp.NewHub()
	svc := &runnersapp.Service{Repo: runnersdb.New(d), Projects: projectsStub{}, Tx: db.Transactor{DB: d},
		Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Rand: rand.Reader, Now: time.Now, NewID: rand.Text,
		Sessions: hub}
	mux := http.NewServeMux()
	(&runnershttp.Handler{Service: svc,
		Limiter: ratelimit.New(ratelimit.Config{Burst: 100, Every: time.Minute, MaxKeys: 10}, time.Now)}).Register(mux)
	(&runnersgw.Handler{Gateway: &runnersapp.Gateway{Runners: runnersdb.New(d), Hub: hub, Rand: rand.Reader, Now: time.Now,
		NewID: rand.Text}}).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() { hub.Shutdown(); ts.Close() })
	return &controlPlane{url: ts.URL, svc: svc, hub: hub}
}

type projectsStub struct{}

func (projectsStub) Lookup(_ context.Context, id string) (runnersapp.ProjectRef, error) {
	return runnersapp.ProjectRef{ID: id, Name: id}, nil
}

var owner = auth.Principal{UserID: "owner1", Login: "owner", Owner: true}

func (c *controlPlane) token(t *testing.T) string {
	t.Helper()
	_, plain, err := c.svc.CreateToken(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

type result struct {
	err            error
	stdout, stderr string
}

// cli runs the command like main does, with the given stdin and data dir defaulting to dataDir.
func cli(ctx context.Context, dataDir, stdin string, prompt func(string) (string, error), args ...string) result {
	var out, errOut bytes.Buffer
	err := run(ctx, args, &env{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut, readSecret: prompt,
		defaultDataDir: dataDir, hostname: func() (string, error) { return "test-box", nil }})
	return result{err: err, stdout: out.String(), stderr: errOut.String()}
}

var fingerprintRe = func(s string) string {
	i := strings.Index(s, "SHA256:")
	if i < 0 {
		return ""
	}
	return strings.Fields(s[i:])[0]
}

// AC: `enroll --server` against the Control Plane: reads the token from stdin, registers the runner as pending
// and prints the fingerprint the Owner compares on the runner's card.
func TestEnrollAgainstControlPlane(t *testing.T) {
	t.Parallel()
	cp := newControlPlane(t)
	dataDir := filepath.Join(t.TempDir(), "runner-data")
	tok := cp.token(t)

	r := cli(t.Context(), dataDir, tok+"\n", nil, "enroll", "--server", cp.url)
	if r.err != nil {
		t.Fatalf("enroll: %v\n%s", r.err, r.stderr)
	}
	fp := fingerprintRe(r.stdout)
	runners, err := cp.svc.List(t.Context(), owner)
	if err != nil || len(runners) != 1 {
		t.Fatalf("runners = %d, %v", len(runners), err)
	}
	got := runners[0].Runner
	if fp == "" || got.Fingerprint != fp || string(got.Status) != "pending" || got.Metadata.Hostname != "test-box" {
		t.Fatalf("stdout %q; server has %+v", r.stdout, got)
	}
	if strings.Contains(r.stdout+r.stderr, tok) {
		t.Fatal("token echoed")
	}
	for _, p := range []string{filepath.Join(dataDir, "keys", "ed25519.pem"), filepath.Join(dataDir, "config.json")} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, st.Mode(), err)
		}
	}

	// A retry with the same token (e.g. after a dropped response) is idempotent and keeps the key.
	r = cli(t.Context(), dataDir, tok, nil, "enroll", "--server", cp.url)
	if r.err != nil || fingerprintRe(r.stdout) != fp {
		t.Fatalf("retry: %v %q", r.err, r.stdout)
	}
	// A second token cannot enroll the same key again.
	r = cli(t.Context(), dataDir, cp.token(t), nil, "enroll", "--server", cp.url)
	if !errors.Is(r.err, domain.ErrKeyEnrolled) || !strings.Contains(r.err.Error(), "ed25519.pem") {
		t.Fatalf("second token: %v", r.err)
	}

	r = cli(t.Context(), dataDir, "", nil, "fingerprint")
	if r.err != nil || strings.TrimSpace(r.stdout) != fp {
		t.Fatalf("fingerprint = %q, %v", r.stdout, r.err)
	}
}

func TestEnrollErrorsFromControlPlane(t *testing.T) {
	t.Parallel()
	cp := newControlPlane(t)
	r := cli(t.Context(), t.TempDir(), "not-a-real-token", nil, "enroll", "--server", cp.url)
	if !errors.Is(r.err, domain.ErrTokenInvalid) {
		t.Fatalf("err = %v", r.err)
	}
}

// AC: the token is never accepted from argv (it would land in shell history and the process list).
func TestEnrollRefusesTokenInArguments(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(ts.Close)
	for _, args := range [][]string{
		{"enroll", "--server", ts.URL, "--token", "abc"},
		{"enroll", "--server", ts.URL, "--token=abc"},
		{"enroll", "--server", ts.URL, "abc"},
	} {
		r := cli(t.Context(), t.TempDir(), "tok", nil, args...)
		if r.err == nil || strings.Contains(r.err.Error()+r.stderr, "abc") {
			t.Errorf("%v: err = %v, stderr %q", args, r.err, r.stderr)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("Control Plane called")
	}
}

// A fake Control Plane that echoes a valid registration for any key.
func echoServer(t *testing.T, gotToken *atomic.Value) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in protocol.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotToken.Store(in.Token)
		raw, _ := base64.StdEncoding.DecodeString(in.PublicKey)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.EnrollResult{RunnerID: "r1", Status: "pending", Fingerprint: domain.FingerprintOf(raw)})
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestEnrollTokenFile(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	url := echoServer(t, &got)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli(t.Context(), filepath.Join(dir, "data"), "stdin-token", nil, "enroll", "--server", url, "--token-file", tf)
	if r.err != nil || got.Load() != "file-token" {
		t.Fatalf("err = %v, token %v", r.err, got.Load())
	}
	// A token file others can read is refused.
	if err := os.Chmod(tf, 0o644); err != nil {
		t.Fatal(err)
	}
	r = cli(t.Context(), filepath.Join(dir, "data2"), "", nil, "enroll", "--server", url, "--token-file", tf)
	if !errors.Is(r.err, secfile.ErrInsecure) {
		t.Fatalf("err = %v", r.err)
	}
}

// On a terminal the token is read through the no-echo prompt, never from the visible stdin.
func TestEnrollPromptsOnTerminal(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	url := echoServer(t, &got)
	var asked string
	prompt := func(p string) (string, error) { asked = p; return "typed-token", nil }
	r := cli(t.Context(), filepath.Join(t.TempDir(), "d"), "", prompt, "enroll", "--server", url)
	if r.err != nil || got.Load() != "typed-token" || !strings.Contains(asked, "token") {
		t.Fatalf("err = %v, token %v, prompt %q", r.err, got.Load(), asked)
	}
}

// AC: plain http:// only for loopback; nothing is created or asked before the URL is accepted.
func TestEnrollRejectsRemotePlainHTTP(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "d")
	asked := false
	prompt := func(string) (string, error) { asked = true; return "t", nil }
	r := cli(t.Context(), dataDir, "", prompt, "enroll", "--server", "http://10.1.2.3:8080")
	if !errors.Is(r.err, domain.ErrPlainHTTP) || asked {
		t.Fatalf("err = %v, asked = %v", r.err, asked)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("data dir created")
	}
	if r := cli(t.Context(), dataDir, "t", nil, "enroll"); r.err == nil || !strings.Contains(r.err.Error(), "--server") {
		t.Fatalf("missing --server: %v", r.err)
	}
}

func TestRunNeedsEnrollment(t *testing.T) {
	t.Parallel()
	r := cli(t.Context(), t.TempDir(), "", nil, "run")
	if !errors.Is(r.err, domain.ErrNotEnrolled) {
		t.Fatalf("err = %v", r.err)
	}
	if exitCode(r.err) != exitConfig || exitCode(errors.New("network down")) != 1 {
		t.Fatal("not-enrolled must exit with EX_CONFIG so services stop restarting")
	}
	if r := cli(t.Context(), t.TempDir(), "", nil, "fingerprint"); r.err == nil {
		t.Fatal("fingerprint without a key succeeded")
	}
}

// AC: the daemon refuses to start with a key other users can read.
func TestRunWithEnrolledKey(t *testing.T) {
	t.Parallel()
	var got atomic.Value
	url := echoServer(t, &got)
	dataDir := filepath.Join(t.TempDir(), "d")
	if r := cli(t.Context(), dataDir, "tok", nil, "enroll", "--server", url); r.err != nil {
		t.Fatal(r.err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the stub returns once shut down
	if r := cli(ctx, dataDir, "", nil, "run"); r.err != nil || !strings.Contains(r.stderr, "r1") {
		t.Fatalf("run = %v, %q", r.err, r.stderr)
	}
	key := filepath.Join(dataDir, "keys", "ed25519.pem")
	if err := os.Chmod(key, 0o640); err != nil {
		t.Fatal(err)
	}
	if r := cli(ctx, dataDir, "", nil, "run"); !errors.Is(r.err, secfile.ErrInsecure) {
		t.Fatalf("run with group-readable key: %v", r.err)
	}
}

func TestVersionAndUsage(t *testing.T) {
	t.Parallel()
	if r := cli(t.Context(), "", "", nil, "version"); r.err != nil || !strings.Contains(r.stdout, "ai-runner") {
		t.Fatalf("version = %q, %v", r.stdout, r.err)
	}
	if r := cli(t.Context(), "", "", nil); r.err == nil || !strings.Contains(r.stderr, "enroll") {
		t.Fatalf("no command: %v %q", r.err, r.stderr)
	}
	if r := cli(t.Context(), "", "", nil, "bogus"); r.err == nil {
		t.Fatal("unknown command accepted")
	}
}

// AC: service templates for systemd (Linux/WSL2) and launchd (macOS) run the daemon with an explicit data dir.
func TestServiceTemplates(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"kindevai-runner.service", "kindevai-runner-user.service", "io.kindevai.runner.plist"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "runner", name))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "ai-runner") || !strings.Contains(s, "run") || !strings.Contains(s, "--data-dir") {
			t.Errorf("%s does not start `ai-runner run --data-dir`", name)
		}
		if strings.HasSuffix(name, ".plist") {
			dec := xml.NewDecoder(bytes.NewReader(b))
			for {
				if _, err := dec.Token(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
		} else if !strings.Contains(s, "UMask=0077") || !strings.Contains(s, "NoNewPrivileges=yes") ||
			!strings.Contains(s, "RestartPreventExitStatus=78") {
			t.Errorf("%s lacks hardening", name)
		}
	}
}

// AC: `ai-runner run` keeps an authenticated connection once the runner is approved, and revocation closes the session and stops the daemon with EX_CONFIG.
func TestRunConnectsAfterApprovalAndStopsWhenRevoked(t *testing.T) {
	t.Parallel()
	cp := newControlPlane(t)
	dataDir := filepath.Join(t.TempDir(), "d")
	r := cli(t.Context(), dataDir, cp.token(t), nil, "enroll", "--server", cp.url)
	if r.err != nil {
		t.Fatal(r.err)
	}
	runners, err := cp.svc.List(t.Context(), owner)
	if err != nil || len(runners) != 1 {
		t.Fatalf("runners %v %v", runners, err)
	}
	id := runners[0].Runner.ID
	done := make(chan result, 1)
	go func() { done <- cli(t.Context(), dataDir, "", nil, "run") }()

	waitUntil := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			select {
			case r := <-done:
				t.Fatalf("daemon exited while waiting for %s: %v\n%s", what, r.err, r.stderr)
			case <-time.After(10 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	if _, err := cp.svc.Approve(t.Context(), owner, id, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	waitUntil("the runner to connect", func() bool { return cp.hub.Online(id) })
	if _, err := cp.svc.Revoke(t.Context(), owner, id); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if !errors.Is(r.err, domain.ErrRunnerRevoked) || exitCode(r.err) != exitConfig {
			t.Fatalf("run = %v (exit %d)\n%s", r.err, exitCode(r.err), r.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after revocation")
	}
}
