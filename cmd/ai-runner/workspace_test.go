package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

func TestGitCredentialSubcommand(t *testing.T) {
	t.Parallel()
	// store/erase are ignored (nothing is ever stored); a get without a reachable runner fails.
	if r := cli(t.Context(), t.TempDir(), "protocol=https\nhost=github.com\n", nil, "git-credential", "/nonexistent/s", "store"); r.err != nil || r.stdout != "" {
		t.Fatalf("%+v", r)
	}
	if r := cli(t.Context(), t.TempDir(), "", nil, "git-credential", "/nonexistent/s", "get"); r.err == nil {
		t.Fatal("get without the runner's socket succeeded")
	}
}

func TestRunRefusesAGitBaseURLWithCredentials(t *testing.T) {
	t.Parallel()
	if _, err := newWorkspaces(t.TempDir(), "/bin/ai-runner", "https://user:pw@github.com", 0); err == nil {
		t.Fatal("accepted")
	}
}

// The session context gets the worktree, the instruction manifest and the changed files from the workspace
// context on a real repository.
func TestSessionWorkspacesOnARealRepository(t *testing.T) {
	t.Parallel()
	remotes, src := t.TempDir(), t.TempDir()
	g := func(dir string, args ...string) {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=d", "GIT_AUTHOR_EMAIL=d@x",
			"GIT_COMMITTER_NAME=d", "GIT_COMMITTER_EMAIL=d@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	g(src, "init", "-q", "-b", "dev")
	if err := os.WriteFile(filepath.Join(src, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(src, "add", "-A")
	g(src, "commit", "-q", "-m", "init")
	g(remotes, "clone", "-q", "--bare", src, filepath.Join(remotes, "acme", "api.git"))
	ws, err := newWorkspaces(t.TempDir(), "/bin/ai-runner", "file://"+remotes, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, prep, err := ws.Prepare(t.Context(), sessionapp.WorkspaceRequest{Step: "s1", Fence: 1, Task: "KIN-9",
		Repository: "acme/api", Base: "dev", Token: sessiondomain.NewSecret("")})
	if err != nil || prep.Branch != "feature/KIN-9--s1" || len(prep.Instructions) != 1 || prep.Instructions[0].Path != "AGENTS.md" {
		t.Fatalf("%+v %v", prep, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := ws.Finish(t.Context(), "s1", 1)
	if err != nil || ch.FilesTotal != 1 || ch.Files[0].Path != "new.txt" || ch.Files[0].Status != "A" ||
		!strings.HasPrefix(ch.Branch, "feature/") {
		t.Fatalf("%+v %v", ch, err)
	}
}
