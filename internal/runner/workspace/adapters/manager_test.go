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

// Acceptance (KIN-106): the runner that held a step's branch is lost during a CI fix round; the next runner starts
// from the published head of the remote work branch (BranchHead), so its push is a fast-forward. A runner whose
// cache still has an older copy of the branch catches up to the published head; a branch that moved on the remote
// is never worked on.
func TestARunnerSwitchContinuesFromThePublishedBranch(t *testing.T) {
	t.Parallel()
	remotes, src := t.TempDir(), t.TempDir()
	git(t, src, "init", "-q", "-b", "dev")
	write(t, filepath.Join(src, "main.go"), "package main\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	base := git(t, src, "rev-parse", "HEAD")
	remote := filepath.Join(remotes, "acme", "api.git")
	git(t, remotes, "clone", "-q", "--bare", src, remote)
	g, err := gitcli.New("file://"+remotes, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := func() *app.Manager {
		return &app.Manager{Git: g, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: t.TempDir()}}
	}
	ctx := t.Context()
	// work prepares attempt fence on m from head, writes file and pushes the result.
	work := func(m *app.Manager, fence int64, head, file string) string {
		t.Helper()
		p, err := m.Prepare(ctx, app.Request{Step: "s1", Fence: fence, Task: "KIN-7", Repository: "acme/api", Base: "dev",
			BranchHead: head})
		if err != nil {
			t.Fatalf("prepare %d: %v", fence, err)
		}
		if p.BaseSHA != base {
			t.Fatalf("fork %s, want %s", p.BaseSHA, base)
		}
		write(t, filepath.Join(p.Dir, file), file+"\n")
		res, err := m.Finish(ctx, "s1", fence)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Push(ctx, app.PushRequest{Step: "s1", Task: "KIN-7", Repository: "acme/api", Head: res.HeadSHA}); err != nil {
			t.Fatalf("push %d: %v", fence, err)
		}
		return res.HeadSHA
	}
	a, b := runner(), runner()
	h1 := work(a, 1, "", "first.go") // published; then runner a is lost
	h2 := work(b, 2, h1, "second.go")
	if git(t, remote, "rev-parse", "refs/heads/feature/KIN-7--s1") != h2 || git(t, remote, "rev-parse", h2+"^") != h1 {
		t.Fatal("the fix round did not continue the published branch")
	}
	h3 := work(a, 3, h2, "third.go") // a is back with its stale copy (h1)
	if git(t, remote, "rev-parse", h3+"^") != h2 {
		t.Fatal("the stale copy was not brought up to the published head")
	}
	// Someone else pushed to the branch: the published head the Control Plane knows is no longer the remote's.
	if _, err := runner().Prepare(ctx, app.Request{Step: "s1", Fence: 4, Task: "KIN-7", Repository: "acme/api",
		Base: "dev", BranchHead: h2}); err == nil {
		t.Fatal("prepared from a branch that moved on")
	}
}

// Review (KIN-106): a worktree an earlier attempt left on the branch keeps its work: the cached branch is not moved
// to a newer published head under it (the attempt fails instead; another runner continues the round).
func TestALeftoverWorktreeKeepsItsBranch(t *testing.T) {
	t.Parallel()
	remotes, src, dataA := t.TempDir(), t.TempDir(), t.TempDir()
	git(t, src, "init", "-q", "-b", "dev")
	write(t, filepath.Join(src, "main.go"), "package main\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, remotes, "clone", "-q", "--bare", src, filepath.Join(remotes, "acme", "api.git"))
	g, err := gitcli.New("file://"+remotes, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &app.Manager{Git: g, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: dataA}}
	b := &app.Manager{Git: g, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: t.TempDir()}}
	ctx := t.Context()
	req := func(fence int64, head string) app.Request {
		return app.Request{Step: "s1", Fence: fence, Task: "KIN-7", Repository: "acme/api", Base: "dev", BranchHead: head}
	}
	publish := func(m *app.Manager, fence int64, head, file string) string {
		t.Helper()
		p, err := m.Prepare(ctx, req(fence, head))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(p.Dir, file), file+"\n")
		res, err := m.Finish(ctx, "s1", fence)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Push(ctx, app.PushRequest{Step: "s1", Task: "KIN-7", Repository: "acme/api", Head: res.HeadSHA}); err != nil {
			t.Fatal(err)
		}
		return res.HeadSHA
	}
	h1 := publish(a, 1, "", "first.go")
	left, err := a.Prepare(ctx, req(2, "")) // never finished: its worktree stays, with work in progress
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(left.Dir, "wip.go"), "wip\n")
	h2 := publish(b, 3, h1, "second.go")
	mirror := fsstore.Store{DataDir: dataA}.MirrorPath("acme", "api")
	if err := g.SyncBranch(ctx, mirror, "acme/api", "feature/KIN-7--s1", h2, app.NewToken("")); err == nil {
		t.Fatal("moved a branch a worktree has checked out")
	}
	if got := git(t, mirror, "rev-parse", "refs/heads/feature/KIN-7--s1"); got != h1 {
		t.Fatalf("branch at %s, want %s", got, h1)
	}
	if _, err := os.Stat(filepath.Join(left.Dir, "wip.go")); err != nil {
		t.Fatal(err)
	}
}
