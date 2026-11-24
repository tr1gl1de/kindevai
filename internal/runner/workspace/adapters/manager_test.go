package adapters_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/fsstore"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/gitcli"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/scan"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=d",
		"GIT_AUTHOR_EMAIL=d@x", "GIT_COMMITTER_NAME=d", "GIT_COMMITTER_EMAIL=d@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The whole workspace lifecycle on real git repositories: prepare from the base, discover instructions, the
// agent changes files and an instruction, finish reports both, removes the worktree and keeps the branch.
func TestWorkspaceLifecycle(t *testing.T) {
	t.Parallel()
	remotes, data, src := t.TempDir(), t.TempDir(), t.TempDir()
	git(t, src, "init", "-q", "-b", "dev")
	write(t, filepath.Join(src, "CLAUDE.md"), "rules\n")
	write(t, filepath.Join(src, ".claude/agents/reviewer.md"), "review\n")
	write(t, filepath.Join(src, "main.go"), "package main\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	base := git(t, src, "rev-parse", "HEAD")
	git(t, remotes, "clone", "-q", "--bare", src, filepath.Join(remotes, "acme", "api.git"))

	g, err := gitcli.New("file://"+remotes, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &app.Manager{Git: g, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: data}}
	req := app.Request{Step: "s1", Fence: 1, Task: "KIN-7", Repository: "acme/api", Base: "dev"}
	p, err := m.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "feature/KIN-7--s1" || p.BaseSHA != base || len(p.Instructions) != 2 ||
		!strings.HasPrefix(p.Dir, filepath.Join(data, "workspaces")+string(filepath.Separator)) {
		t.Fatalf("%+v", p)
	}
	second := req
	second.Fence = 2
	if _, err := m.Prepare(t.Context(), second); !errors.Is(err, app.ErrBusy) {
		t.Fatalf("second writer: %v", err)
	}

	write(t, filepath.Join(p.Dir, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(p.Dir, "CLAUDE.md"), "rules, changed by the agent\n")
	res, err := m.Finish(t.Context(), "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseSHA != base || res.Total != 2 || res.Files[0].Path != "CLAUDE.md" || res.Files[1].Path != "main.go" ||
		len(res.InstructionsChanged) != 1 || res.InstructionsChanged[0].Path != "CLAUDE.md" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(p.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("worktree not removed")
	}
	if got := git(t, data, "--git-dir", filepath.Join(data, "repos", "acme", "api.git"), "rev-parse",
		"refs/heads/feature/KIN-7--s1"); got != res.HeadSHA {
		t.Fatalf("branch %s, head %s", got, res.HeadSHA)
	}
	// The retry of the step continues the branch with the earlier work.
	p2, err := m.Prepare(t.Context(), second)
	if err != nil || p2.BaseSHA != base {
		t.Fatalf("%+v %v", p2, err)
	}
	if b, _ := os.ReadFile(filepath.Join(p2.Dir, "main.go")); !strings.Contains(string(b), "func main") {
		t.Fatal("earlier work missing")
	}
	if _, err := m.Finish(t.Context(), "s1", 2); err != nil {
		t.Fatal(err)
	}
}
