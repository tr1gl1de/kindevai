//go:build unix

package cliagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// The session store of a task (KIN-86) is one owner-only directory per project, task and CLI, bound where the CLI
// keeps its sessions; other tasks get other directories.
func TestSessionStorePrepare(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	s := &SessionStore{Root: root}
	src, rel, err := s.Prepare("proj-1", "task-1", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	if src != filepath.Join(root, "proj-1", "task-1", "claude") || rel != ".claude/projects" {
		t.Fatalf("store %s at %s", src, rel)
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("store %v %v", fi, err)
	}
	if _, rel, err := s.Prepare("proj-1", "task-1", domain.AgentCodex); err != nil || rel != ".codex/sessions" {
		t.Fatalf("codex: %s %v", rel, err)
	}
	other, _, err := s.Prepare("proj-1", "task-2", domain.AgentClaude)
	if err != nil || other == src {
		t.Fatalf("another task's store %s %v", other, err)
	}
	for _, bad := range [][2]string{{"", "t"}, {"p", ""}} {
		if _, _, err := s.Prepare(bad[0], bad[1], domain.AgentClaude); err == nil {
			t.Errorf("project %q task %q accepted", bad[0], bad[1])
		}
	}
	// Ids that are not plain names become hashed directory names, never paths.
	for _, odd := range [][2]string{{"..", "t"}, {"p", "../x"}, {"p/q", "t"}, {".", "t"}} {
		src, _, err := s.Prepare(odd[0], odd[1], domain.AgentClaude)
		if err != nil || !strings.HasPrefix(src, root+"/") || strings.Count(strings.TrimPrefix(src, root+"/"), "/") != 2 {
			t.Errorf("project %q task %q: %s %v", odd[0], odd[1], src, err)
		}
	}
	if _, _, err := s.Prepare("p", "t", "zcode"); err == nil {
		t.Error("unknown agent accepted")
	}
	// A planted symlink in place of a task's store is refused, not followed.
	if err := os.MkdirAll(filepath.Join(root, "proj-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "proj-1", "task-3")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Prepare("proj-1", "task-3", domain.AgentClaude); err == nil {
		t.Error("a symlinked store was used")
	}
}

// Stores of tasks no attempt used for MaxAge are removed when another store is prepared.
func TestSessionStoreSweepsOldTasks(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, MaxAge: 30 * 24 * time.Hour, Now: func() time.Time { return now }}
	old, _, err := s.Prepare("p", "old", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "s.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An agent made a directory of its store read-only: the sweep still removes it.
	locked := filepath.Join(old, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	recent, _, err := s.Prepare("p", "recent", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	past := now.Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "p", "old"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "p", "recent"), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Sweep()
	if _, err := os.Stat(filepath.Join(root, "p", "old")); !os.IsNotExist(err) {
		t.Fatalf("old store kept: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent store removed: %v", err)
	}
}
