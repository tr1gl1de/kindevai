package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/journal"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/gitcli"

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

// recordedEvents keeps the attempts' events in memory (kind and body).
type recordedEvents struct {
	mu  sync.Mutex
	evs []string
}

func (e *recordedEvents) Attempt(string, int64, int64, func(int64) error) sessionapp.Stream { return e }
func (e *recordedEvents) Emit(kind string, body any) error {
	b, _ := json.Marshal(body)
	e.mu.Lock()
	e.evs = append(e.evs, kind+" "+string(b))
	e.mu.Unlock()
	return nil
}
func (e *recordedEvents) Output(string) io.Writer { return io.Discard }
func (e *recordedEvents) find(prefix string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range e.evs {
		if strings.HasPrefix(v, prefix) {
			return v
		}
	}
	return ""
}

// KIN-71 on a real repository behind a slow remote: the assignment is acknowledged while git fetches, the fetch
// reports its phase, and a cancel kills git, fails the attempt as cancelled and leaves no worktree or record.
func TestCancelDuringASlowFetchOnARealRepository(t *testing.T) {
	t.Parallel()
	remotes, src, tools, data := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
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
	g(src, "commit", "-q", "--allow-empty", "-m", "init")
	g(remotes, "clone", "-q", "--bare", src, filepath.Join(remotes, "acme", "api.git"))
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	fifo, pidFile := filepath.Join(tools, "never"), filepath.Join(tools, "fetch.pid")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A fetch waits 60 s in a bash builtin (killing the wrapper leaves no child), then runs git.
	script := fmt.Sprintf("#!/bin/bash\nfor a in \"$@\"; do [ \"$a\" = fetch ] && { echo $$ > %s; read -r -t 60 _ <> %s; }; done\nexec %s \"$@\"\n",
		pidFile, fifo, real)
	prog := filepath.Join(tools, "git")
	if err := os.WriteFile(prog, []byte(script), 0o700); err != nil { //nolint:gosec // test helper program
		t.Fatal(err)
	}
	ws, err := newWorkspaces(data, "/bin/ai-runner", "file://"+remotes, 0)
	if err != nil {
		t.Fatal(err)
	}
	ws.m.Git.(*gitcli.Git).Program = prog
	ev := &recordedEvents{}
	j := journal.Dir{Path: filepath.Join(data, "attempts")}
	x := &sessionapp.Executor{Journal: j, Events: ev, Workspaces: ws}
	a := sessiondomain.Assignment{Step: "s1", Fence: 1, Execution: "e1", Task: "KIN-9", Project: "p1",
		Repository: "acme/api", Adapter: sessiondomain.AdapterMock, Base: "dev"}
	start := time.Now()
	if err := x.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the assignment took %s", d)
	}
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for pid == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {

			_, _ = fmt.Sscan(strings.TrimSpace(string(b)), &pid)
		}
		if time.Now().After(deadline) {
			t.Fatal("no fetch started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e := ev.find(protocol.EventWorkspacePreparing + ` {"phase":"fetching"`); e == "" {
		t.Fatalf("no fetching phase in %v", ev.evs)
	}
	x.Cancel("s1", 1, "user")
	deadline = time.Now().Add(10 * time.Second)
	for ev.find(protocol.EventFailed) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled preparation was not reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e := ev.find(protocol.EventFailed); !strings.Contains(e, "cancelled: user") {
		t.Fatal(e)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("fetch process %d still alive", pid)
	}
	if recs, err := j.List(); err != nil || len(recs) != 0 {
		t.Fatalf("journal %+v %v", recs, err)
	}
	if _, err := os.Stat(filepath.Join(data, "workspaces", "acme", "api", "KIN-9--s1")); err == nil {
		t.Fatal("worktree left")
	}
	x.KillAll()
}
