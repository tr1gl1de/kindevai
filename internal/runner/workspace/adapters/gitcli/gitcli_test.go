package gitcli

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

// TestMain doubles as the credential helper: git runs `<test binary> git-credential <socket> get`.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperCommand {
		os.Exit(HelperMain(os.Args[2:], os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// sh runs the system git as a developer would (fixed identity, no user config).
func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=dev",
		"GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// origin creates <root>/acme/api.git (bare) whose dev branch has a few files; it returns the dev commit.
func origin(t *testing.T, root string) string {
	t.Helper()
	src := t.TempDir()
	sh(t, src, "init", "-q", "-b", "dev")
	write(t, filepath.Join(src, "README.md"), "hello\n")
	write(t, filepath.Join(src, "CLAUDE.md"), "be nice\n")
	write(t, filepath.Join(src, "old.txt"), "old\n")
	sh(t, src, "add", "-A")
	sh(t, src, "commit", "-q", "-m", "init")
	sh(t, src, "branch", "main")
	if err := os.MkdirAll(filepath.Join(root, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, root, "clone", "-q", "--bare", src, filepath.Join(root, "acme", "api.git"))
	return sh(t, src, "rev-parse", "HEAD")
}

type env struct {
	git    *Git
	mirror string
	wt     string
	base   string
}

func newEnv(t *testing.T) env {
	t.Helper()
	remotes, data := t.TempDir(), t.TempDir()
	base := origin(t, remotes)
	g, err := New("file://"+remotes, helper(t))
	if err != nil {
		t.Fatal(err)
	}
	return env{git: g, mirror: filepath.Join(data, "repos", "acme", "api.git"),
		wt: filepath.Join(data, "workspaces", "acme", "api", "KIN-1-s1"), base: base}
}

func helper(t *testing.T) []string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, HelperCommand}
}

func TestWorktreeOnAFeatureBranchFromTheBase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	sha, err := e.git.FetchBase(ctx, e.mirror, "acme/api", "dev", app.Token{})
	if err != nil || sha != e.base {
		t.Fatalf("fetch: %q %v (want %s)", sha, err, e.base)
	}
	// The cache is bare and keeps no remote (the URL is given per fetch).
	if out := sh(t, e.mirror, "config", "--list", "--local"); strings.Contains(out, "remote.") || !strings.Contains(out, "core.bare=true") {
		t.Fatalf("mirror config:\n%s", out)
	}
	fork, admin, err := e.git.AddWorktree(ctx, e.mirror, e.wt, "feature/KIN-1-s1", sha)
	if err != nil || fork != sha {
		t.Fatalf("worktree: %q %v", fork, err)
	}
	if got := sh(t, e.wt, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/KIN-1-s1" {
		t.Fatalf("branch %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(admin, "gitdir")); strings.TrimSpace(string(b)) != filepath.Join(e.wt, ".git") {
		t.Fatalf("admin dir %s: %s", admin, b)
	}
	if _, err := os.Stat(filepath.Join(e.wt, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedBranchesAreRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sha, err := e.git.FetchBase(t.Context(), e.mirror, "acme/api", "dev", app.Token{})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"main", "master"} {
		if _, _, err := e.git.AddWorktree(t.Context(), e.mirror, e.wt, b, sha); !errors.Is(err, domain.ErrProtectedBranch) {
			t.Errorf("%s: %v", b, err)
		}
	}
	if _, err := os.Stat(e.wt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a worktree was created")
	}
}

func TestSnapshotDiffAndRemoveKeepTheBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	sha, _ := e.git.FetchBase(ctx, e.mirror, "acme/api", "dev", app.Token{})
	_, admin, err := e.git.AddWorktree(ctx, e.mirror, e.wt, "feature/KIN-1-s1", sha)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.wt, "README.md"), "changed\n")
	write(t, filepath.Join(e.wt, "dir", "new file.go"), "package x\n")
	if err := os.Remove(filepath.Join(e.wt, "old.txt")); err != nil {
		t.Fatal(err)
	}
	// A hook planted in the cache must not run on the runner's commit.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	write(t, filepath.Join(e.mirror, "hooks", "pre-commit"), "#!/bin/sh\ntouch "+marker+"\n")
	_ = os.Chmod(filepath.Join(e.mirror, "hooks", "pre-commit"), 0o755)
	head, err := e.git.Snapshot(ctx, admin, e.wt, "snapshot")
	if err != nil || head == sha {
		t.Fatalf("snapshot: %q %v", head, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook ran")
	}
	files, err := e.git.Diff(ctx, admin, sha, head)
	want := []domain.Change{{Path: "README.md", Status: "M"}, {Path: "dir/new file.go", Status: "A"}, {Path: "old.txt", Status: "D"}}
	if err != nil || !slices.Equal(files, want) {
		t.Fatalf("diff: %+v %v", files, err)
	}
	if again, err := e.git.Snapshot(ctx, admin, e.wt, "snapshot"); err != nil || again != head {
		t.Fatalf("clean snapshot made a commit: %v", err)
	}
	if err := e.git.RemoveWorktree(ctx, e.mirror, e.wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.wt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("worktree left")
	}
	if got := sh(t, e.mirror, "rev-parse", "refs/heads/feature/KIN-1-s1"); got != head {
		t.Fatalf("branch lost: %s", got)
	}
	// The next attempt continues on the branch; its fork point stays the base.
	fork, _, err := e.git.AddWorktree(ctx, e.mirror, e.wt, "feature/KIN-1-s1", sha)
	if err != nil || fork != sha {
		t.Fatalf("re-add: %q %v", fork, err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.wt, "README.md")); string(b) != "changed\n" {
		t.Fatalf("work not continued: %q", b)
	}
}

// An agent that rewrites the worktree's .git file cannot make the runner's git use its own repository (and its
// core.fsmonitor command): worktree commands name the administrative directory explicitly.
func TestRewrittenDotGitIsNotFollowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	sha, _ := e.git.FetchBase(ctx, e.mirror, "acme/api", "dev", app.Token{})
	_, admin, err := e.git.AddWorktree(ctx, e.mirror, e.wt, "feature/KIN-1-s1", sha)
	if err != nil {
		t.Fatal(err)
	}
	evil, marker := t.TempDir(), filepath.Join(t.TempDir(), "pwned")
	sh(t, evil, "init", "-q")
	sh(t, evil, "config", "core.fsmonitor", "touch "+marker+"; false")
	write(t, filepath.Join(e.wt, ".git"), "gitdir: "+filepath.Join(evil, ".git")+"\n")
	write(t, filepath.Join(e.wt, "x.txt"), "x\n")
	head, err := e.git.Snapshot(ctx, admin, e.wt, "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent's fsmonitor command ran")
	}
	if got := sh(t, e.mirror, "rev-parse", "refs/heads/feature/KIN-1-s1"); got != head {
		t.Fatal("commit did not land on the branch")
	}
}

func TestBadArgumentsAreRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, base := range []string{"--upload-pack=touch /tmp/x", "a..b"} {
		if _, err := e.git.FetchBase(t.Context(), e.mirror, "acme/api", base, app.Token{}); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	if _, err := e.git.Diff(t.Context(), e.mirror, "--output=/tmp/x", "HEAD"); err == nil {
		t.Error("diff option accepted as a commit")
	}
	for _, u := range []string{"ftp://x", "https://user:pw@github.com", "https://github.com?x=1", "github.com"} {
		if _, err := New(u, nil); err == nil {
			t.Errorf("base URL %q accepted", u)
		}
	}
}

// The token reaches git only through the credential helper (a unix socket the runner serves while one git
// command runs): never on argv, in the environment, the remote URL or any file.
func TestTokenOnlyThroughTheCredentialHelper(t *testing.T) {
	t.Parallel()
	remotes, data := t.TempDir(), t.TempDir()
	origin(t, remotes)
	const token = "ghs_TopSecretToken123"
	out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + remotes, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}}
	var seen bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.WriteString(r.URL.String() + "\n")
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g, err := New(srv.URL, helper(t))
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(data, "repos", "acme", "api.git")
	if _, err := g.FetchBase(t.Context(), mirror, "acme/api", "dev", app.Token{}); err == nil {
		t.Fatal("fetched without a token")
	}
	if _, err := g.FetchBase(t.Context(), mirror, "acme/api", "dev", app.NewToken("wrong")); err == nil {
		t.Fatal("fetched with a wrong token")
	}
	sha, err := g.FetchBase(t.Context(), mirror, "acme/api", "dev", app.NewToken(token))
	if err != nil || len(sha) != 40 {
		t.Fatalf("fetch with token: %q %v", sha, err)
	}
	if strings.Contains(seen.String(), token) {
		t.Fatal("token in a URL")
	}
	_ = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(token)) {
				t.Errorf("token written to %s", p)
			}
		}
		return nil
	})
}

func TestCredentialAnswersOnlyTheConfiguredHost(t *testing.T) {
	t.Parallel()
	c := credential{scheme: "https", host: "github.com", token: "tok"}
	for in, want := range map[string]string{
		"action=get\nprotocol=https\nhost=github.com\n":      "username=x-access-token\npassword=tok\n",
		"action=get\nprotocol=https\nhost=evil.example\n":    "",
		"action=get\nprotocol=http\nhost=github.com\n":       "",
		"action=store\nprotocol=https\nhost=github.com\n":    "",
		"action=get\nprotocol=https\nhost=github.com.evil\n": "",
	} {
		if got := c.answer(strings.NewReader(in)); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
