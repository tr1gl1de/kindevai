package adapters_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/fsstore"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/gitcli"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/scan"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
)

// slowRemote is a real repository behind a git program that makes every fetch slow (a large first clone): the
// wrapper records the fetch's pid, notes overlapping fetches, and waits (a bash builtin, so killing the wrapper
// leaves no child) while the file "slow" exists, at most delay.
type slowRemote struct {
	remotes, data, dir string
	git                *gitcli.Git
}

func newSlowRemote(t *testing.T, delay string) *slowRemote {
	t.Helper()
	s := &slowRemote{remotes: t.TempDir(), data: t.TempDir(), dir: t.TempDir()}
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "dev")
	write(t, filepath.Join(src, "AGENTS.md"), "rules\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, s.remotes, "clone", "-q", "--bare", src, filepath.Join(s.remotes, "acme", "api.git"))
	fifo := filepath.Join(s.dir, "never")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/bash
for a in "$@"; do
  if [ "$a" = fetch ]; then
    mkdir %[1]s/inflight 2>/dev/null || echo overlap >> %[1]s/overlap
    echo $$ > %[1]s/fetch.pid
    if [ -e %[1]s/slow ]; then read -r -t %[2]s _ <> %[3]s; fi
    %[4]s "$@"; rc=$?
    rmdir %[1]s/inflight
    exit $rc
  fi
done
exec %[4]s "$@"
`, s.dir, delay, fifo, real)
	prog := filepath.Join(s.dir, "git")
	if err := os.WriteFile(prog, []byte(script), 0o700); err != nil { //nolint:gosec // test helper program
		t.Fatal(err)
	}
	write(t, filepath.Join(s.dir, "slow"), "")
	g, err := gitcli.New("file://"+s.remotes, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.Program = prog
	s.git = g
	return s
}

func (s *slowRemote) fast(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.dir, "slow")); err != nil {
		t.Fatal(err)
	}
}

func (s *slowRemote) fetchPID(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(s.dir, "fetch.pid")); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("no fetch started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *slowRemote) manager() *app.Manager {
	return &app.Manager{Git: s.git, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: s.data}}
}

// KIN-71: cancelling a preparation in the middle of a slow fetch kills git at once and leaves the workspace free:
// no worktree, no lock (the next preparation of the same workspace succeeds).
func TestCancelMidFetchKillsGitAndFreesTheWorkspace(t *testing.T) {
	t.Parallel()
	s := newSlowRemote(t, "60")
	m := s.manager()
	req := app.Request{Step: "s1", Fence: 1, Task: "KIN-7", Repository: "acme/api", Base: "dev"}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := m.Prepare(ctx, req); done <- err }()
	pid := s.fetchPID(t)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("prepared")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled fetch kept running")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cancel took %s", d)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("fetch process %d still alive: %v", pid, err)
	}
	dir := filepath.Join(s.data, "workspaces", "acme", "api", "KIN-7--s1")
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("worktree left: %v", err)
	}
	s.fast(t)
	req.Fence = 2
	if _, err := m.Prepare(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Finish(t.Context(), "s1", 2); err != nil {
		t.Fatal(err)
	}
}

// KIN-71: preparations of one repository run in parallel slots but never fetch into its mirror at the same time.
func TestSlowFetchesOfOneRepositoryNeverOverlap(t *testing.T) {
	t.Parallel()
	s := newSlowRemote(t, "0.2")
	m := s.manager()
	m.MaxPreparing = 3
	errs := make(chan error, 3)
	for i := range 3 {
		go func() {
			_, err := m.Prepare(t.Context(), app.Request{Step: fmt.Sprintf("s%d", i), Fence: 1, Task: "KIN-7",
				Repository: "acme/api", Base: "dev"})
			errs <- err
		}()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dir, "overlap")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("two fetches ran on the mirror at once")
	}
	for i := range 3 {
		if _, err := m.Finish(t.Context(), fmt.Sprintf("s%d", i), 1); err != nil {
			t.Fatal(err)
		}
	}
}
