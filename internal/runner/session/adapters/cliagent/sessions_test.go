//go:build unix

package cliagent

import (
	"fmt"
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
	st, err := s.Prepare("proj-1", "task-1", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	src, rel := st.Source, st.Rel
	if src != filepath.Join(root, "proj-1", "task-1", "claude") || rel != ".claude/projects" {
		t.Fatalf("store %s at %s", src, rel)
	}
	fi, err := os.Stat(src)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("store %v %v", fi, err)
	}
	if st, err := s.Prepare("proj-1", "task-1", domain.AgentCodex); err != nil || st.Rel != ".codex/sessions" {
		t.Fatalf("codex: %s %v", st.Rel, err)
	}
	other, err := s.Prepare("proj-1", "task-2", domain.AgentClaude)
	if err != nil || other.Source == src {
		t.Fatalf("another task's store %s %v", other, err)
	}
	for _, bad := range [][2]string{{"", "t"}, {"p", ""}} {
		if _, err := s.Prepare(bad[0], bad[1], domain.AgentClaude); err == nil {
			t.Errorf("project %q task %q accepted", bad[0], bad[1])
		}
	}
	// Ids that are not plain names become hashed directory names, never paths.
	for _, odd := range [][2]string{{"..", "t"}, {"p", "../x"}, {"p/q", "t"}, {".", "t"}} {
		st, err := s.Prepare(odd[0], odd[1], domain.AgentClaude)
		if err != nil || !strings.HasPrefix(st.Source, root+"/") || strings.Count(strings.TrimPrefix(st.Source, root+"/"), "/") != 2 {
			t.Errorf("project %q task %q: %s %v", odd[0], odd[1], st.Source, err)
		}
	}
	if _, err := s.Prepare("p", "t", "zcode"); err == nil {
		t.Error("unknown agent accepted")
	}
	// A planted symlink in place of a task's store is refused, not followed.
	if err := os.MkdirAll(filepath.Join(root, "proj-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "proj-1", "task-3")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare("proj-1", "task-3", domain.AgentClaude); err == nil {
		t.Error("a symlinked store was used")
	}
}

// Stores of tasks no attempt used for MaxAge are removed when another store is prepared.
func TestSessionStoreSweepsOldTasks(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, MaxAge: 30 * 24 * time.Hour, Now: func() time.Time { return now }}
	oldSt, err := s.Prepare("p", "old", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	old := oldSt.Source
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
	recentSt, err := s.Prepare("p", "recent", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	recent := recentSt.Source
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

// writeAged writes a file of size bytes in dir, last modified at t.
func writeAged(t *testing.T, dir, name string, size int, at time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

// AC (KIN-96, KIN-115): a task's store is limited in size. Over 3/4 of the limit (headroom for the attempt, which
// is stopped over the limit itself), Prepare removes the task's oldest session files (of all its CLIs) until the
// store fits, keeps the newest ones and says so; within it nothing is touched.
func TestSessionStorePrepareEnforcesLimit(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, Limit: 700, Now: func() time.Time { return now }}
	st, err := s.Prepare("p", "t", domain.AgentClaude)
	if err != nil || st.Notice != "" {
		t.Fatalf("fresh store %+v %v", st, err)
	}
	codex, err := s.Prepare("p", "t", domain.AgentCodex)
	if err != nil {
		t.Fatal(err)
	}
	oldest := writeAged(t, codex.Source, "2026/11/01/a.jsonl", 400, now.Add(-72*time.Hour))
	older := writeAged(t, st.Source, "proj/b.jsonl", 400, now.Add(-48*time.Hour))
	newest := writeAged(t, st.Source, "proj/c.jsonl", 400, now.Add(-time.Hour))
	// Another task's store never counts towards this one, nor is it touched.
	other, err := s.Prepare("p", "u", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	foreign := writeAged(t, other.Source, "big.jsonl", 900, now.Add(-100*time.Hour))
	// A symlink in the store is neither followed nor counted.
	if err := os.Symlink(foreign, filepath.Join(st.Source, "link")); err != nil {
		t.Fatal(err)
	}

	st, err = s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{oldest, older} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s kept: %v", gone, err)
		}
	}
	for _, kept := range []string{newest, foreign, filepath.Join(st.Source, "link")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s removed: %v", kept, err)
		}
	}
	for _, want := range []string{"session store", "limit", "removed 2"} {
		if !strings.Contains(st.Notice, want) {
			t.Errorf("notice %q lacks %q", st.Notice, want)
		}
	}
	if st, err := s.Prepare("p", "t", domain.AgentClaude); err != nil || st.Notice != "" {
		t.Fatalf("within the limit: %+v %v", st, err)
	}
	if (&SessionStore{}).limit() != DefaultSessionLimit || DefaultSessionLimit != 256<<20 {
		t.Errorf("default limit %d", DefaultSessionLimit)
	}
}

// An agent made a directory of its store read-only: eviction still removes the files in it; a store whose files
// cannot be removed refuses the attempt with the reason instead of running over the limit.
func TestSessionStoreLimitLockedAndRefused(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, Limit: 100, Now: func() time.Time { return now }}
	st, err := s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	f := writeAged(t, st.Source, "locked/a.jsonl", 500, now.Add(-time.Hour))
	if err := os.Chmod(filepath.Dir(f), 0o500); err != nil {
		t.Fatal(err)
	}
	st, err = s.Prepare("p", "t", domain.AgentClaude)
	if err != nil || st.Notice == "" {
		t.Fatalf("locked: %+v %v", st, err)
	}
	if _, err := os.Lstat(f); !os.IsNotExist(err) {
		t.Fatalf("locked file kept: %v", err)
	}

	s.remove = func(string) error { return os.ErrPermission }
	writeAged(t, st.Source, "b.jsonl", 500, now.Add(-time.Hour))
	if _, err := s.Prepare("p", "t", domain.AgentClaude); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an over-limit store that cannot shrink: %v", err)
	}
}

// An agent left a directory of its store unreadable: the next attempt still measures (and runs) instead of being
// locked out; the sweep never shrinks a store (an attempt may be using it), only Prepare does and says so.
func TestSessionStoreUnreadableDirAndSweep(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, Limit: 500, Now: func() time.Time { return now }}
	st, err := s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(st.Source, "sub")
	writeAged(t, sub, "a.jsonl", 10, now)
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Prepare("p", "t", domain.AgentClaude); err != nil || st.Notice != "" {
		t.Fatalf("unreadable dir under the limit: %+v %v", st, err)
	}
	old := writeAged(t, st.Source, "b.jsonl", 600, now.Add(-time.Hour))
	s.Sweep()
	if _, err := os.Lstat(old); err != nil {
		t.Fatalf("the sweep shrank a store: %v", err)
	}
	if st, err := s.Prepare("p", "t", domain.AgentClaude); err != nil || !strings.Contains(st.Notice, "removed 1") {
		t.Fatalf("over the limit: %+v %v", st, err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("old file kept: %v", err)
	}
}

// AC (KIN-115): entries count too (files, directories, symlinks: an agent could exhaust the inodes with empty
// files). Over 3/4 of the entry limit, Prepare removes the oldest entries that are not directories and the
// directories that leaves empty, and says so.
func TestSessionStorePrepareEnforcesEntryLimit(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, EntryLimit: 8, Now: func() time.Time { return now }}
	st, err := s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	var files, links []string
	for i := range 4 { // the claude directory, d/, 4 old files and 4 new symlinks: 10 entries
		files = append(files, writeAged(t, st.Source, fmt.Sprintf("d/f%d", i), 1, now.Add(-time.Duration(10-i)*time.Hour)))
		l := filepath.Join(st.Source, fmt.Sprintf("l%d", i))
		if err := os.Symlink("/nowhere", l); err != nil { // its mtime is the wall clock's: newer than the files
			t.Fatal(err)
		}
		links = append(links, l)
	}
	st, err = s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"entries", "removed 4"} {
		if !strings.Contains(st.Notice, want) {
			t.Errorf("notice %q lacks %q", st.Notice, want)
		}
	}
	for _, gone := range append(files, filepath.Join(st.Source, "d")) {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s kept: %v", gone, err)
		}
	}
	for _, kept := range links {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s removed: %v", kept, err)
		}
	}
	if (&SessionStore{}).entryLimit() != DefaultSessionEntryLimit || DefaultSessionEntryLimit != 20000 {
		t.Errorf("default entry limit %d", DefaultSessionEntryLimit)
	}
}

// AC (KIN-115): while an attempt runs the store is measured against the limits themselves: within them OverQuota
// says nothing; over the bytes or the entries it says which, also when the agent hid a directory (mode 0).
func TestSessionStoreOverQuota(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, Limit: 1000, EntryLimit: 20, Now: func() time.Time { return now }}
	if why := s.OverQuota("p", "t"); why != "" {
		t.Fatalf("no store yet: %q", why)
	}
	st, err := s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	writeAged(t, st.Source, "a.jsonl", 900, now)
	if why := s.OverQuota("p", "t"); why != "" {
		t.Fatalf("within the quota: %q", why)
	}
	hidden := filepath.Join(st.Source, "hidden")
	writeAged(t, hidden, "b.jsonl", 200, now)
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	if why := s.OverQuota("p", "t"); !strings.Contains(why, "over its quota") || !strings.Contains(why, "1000 B") {
		t.Fatalf("over the bytes: %q", why)
	}

	u, err := s.Prepare("p", "u", domain.AgentCodex)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		writeAged(t, u.Source, fmt.Sprintf("e%d", i), 0, now)
	}
	if why := s.OverQuota("p", "u"); !strings.Contains(why, "entries") || !strings.Contains(why, "> 20") {
		t.Fatalf("over the entries: %q", why)
	}
}

// AC (KIN-115): a store with far more entries than its limit is not listed whole before an attempt: every session
// file of the task is removed (the CLIs' store directories stay, they are bound into the sandbox), and it says so.
func TestSessionStoreWipesAHugeStore(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cli-sessions")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	s := &SessionStore{Root: root, EntryLimit: 2, Now: func() time.Time { return now }}
	st, err := s.Prepare("p", "t", domain.AgentClaude)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		writeAged(t, st.Source, fmt.Sprintf("d%d/f", i%3), 1, now)
		writeAged(t, st.Source, fmt.Sprintf("f%d", i), 1, now)
	}
	if err := os.Chmod(filepath.Join(st.Source, "d1"), 0); err != nil {
		t.Fatal(err)
	}
	st, err = s.Prepare("p", "t", domain.AgentClaude)
	if err != nil || !strings.Contains(st.Notice, "removed all") {
		t.Fatalf("%+v %v", st, err)
	}
	if left, err := os.ReadDir(st.Source); err != nil || len(left) != 0 {
		t.Fatalf("left %v %v", left, err)
	}
}
