//go:build unix

package cliagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

// SessionStore keeps the CLIs' sessions of sandboxed attempts between attempts (KIN-86): the agent's HOME in the
// strict sandbox is a fresh tmpfs per attempt, so without it `--resume` of the next attempt finds nothing. Each
// project × task × CLI gets its own directory under Root (in the runner's data directory), bound read-write at the
// CLI's SessionDir below the agent's HOME: an attempt sees its own task's sessions only, never another task's or the
// host's. The store holds transcripts only: the CLI's login stays a copy (or the runner's token) and is never written
// back. Stores no attempt used for MaxAge are removed. A task's store (all its CLIs together) is kept within Limit
// (KIN-96): before each attempt its oldest session files are removed until it fits again, and the attempt says so.
type SessionStore struct {
	Root   string
	MaxAge time.Duration    // DefaultSessionMaxAge when zero
	Limit  int64            // bytes of session files per task; DefaultSessionLimit when zero
	Now    func() time.Time // time.Now when nil

	remove func(string) error // os.Remove when nil (tests make it fail)

	mu        sync.Mutex
	lastSweep time.Time
	shrinking map[string]*sync.Mutex // per task directory: concurrent attempts of a task shrink one at a time
}

// Store is a task's session store prepared for an attempt.
type Store struct {
	Source string // the directory in the runner's data directory
	Rel    string // where it is bound below the agent's HOME
	Notice string // why session files were removed before the attempt ("" when none were): for its output
}

// DefaultSessionLimit bounds the session files of one task: a transcript is a few MiB at most, so this keeps many
// sessions while an agent cannot fill the runner's data directory through its store.
const DefaultSessionLimit = 256 << 20

// DefaultSessionMaxAge is how long a task's sessions are kept after its last attempt.
const DefaultSessionMaxAge = 30 * 24 * time.Hour

// sweepEvery bounds how often Prepare sweeps.
const sweepEvery = time.Hour

var storeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (s *SessionStore) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *SessionStore) maxAge() time.Duration {
	if s.MaxAge <= 0 {
		return DefaultSessionMaxAge
	}
	return s.MaxAge
}

func (s *SessionStore) limit() int64 {
	if s.Limit <= 0 {
		return DefaultSessionLimit
	}
	return s.Limit
}

// Prepare creates (or reuses) the store of agent for task of project and returns it with the path below HOME it is
// bound at. It marks the task's store as used now and brings it within Limit; a store that cannot be brought within
// it is an error (the attempt does not run over the limit).
func (s *SessionStore) Prepare(project, task, agent string) (Store, error) {
	d, err := DialectFor(agent)
	if err != nil {
		return Store{}, err
	}
	if project == "" || task == "" {
		return Store{}, errors.New("session store: no project or task")
	}
	s.maybeSweep()
	taskDir := filepath.Join(s.Root, storeName(project), storeName(task))
	source := filepath.Join(taskDir, agent)
	// Every level owner-only and no symlink (EnsureDir refuses one): the runner binds exactly this directory.
	for _, dir := range []string{s.Root, filepath.Dir(taskDir), taskDir, source} {
		if err := secfile.EnsureDir(dir); err != nil {
			return Store{}, fmt.Errorf("session store: %w", err)
		}
	}
	now := s.now()
	if err := os.Chtimes(taskDir, now, now); err != nil {
		return Store{}, fmt.Errorf("session store: %w", err)
	}
	notice, err := s.shrink(taskDir)
	if err != nil {
		return Store{}, err
	}
	return Store{Source: source, Rel: d.SessionDir(), Notice: notice}, nil
}

type sessionFile struct {
	path string
	size int64
	mod  time.Time
}

// shrink removes the oldest session files of the task's store at taskDir until it is within Limit and says what it
// removed ("" when the store was within it). Only regular files count; symlinks are neither followed nor counted.
func (s *SessionStore) shrink(taskDir string) (string, error) {
	s.mu.Lock()
	if s.shrinking == nil {
		s.shrinking = map[string]*sync.Mutex{}
	}
	m := s.shrinking[taskDir]
	if m == nil {
		m = &sync.Mutex{}
		s.shrinking[taskDir] = m
	}
	s.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	unlock(taskDir) // an agent may have made a directory unreadable or read-only
	var files []sessionFile
	var total int64
	err := filepath.WalkDir(taskDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info() // lstat
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		files = append(files, sessionFile{p, fi.Size(), fi.ModTime()})
		total += fi.Size()
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("session store: measuring %s: %w", filepath.Base(taskDir), err)
	}
	limit := s.limit()
	if total <= limit {
		return "", nil
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.Before(files[j].mod)
		}
		return files[i].path < files[j].path
	})
	remove := s.remove
	if remove == nil {
		remove = os.Remove
	}
	was := total
	var n int
	var freed int64
	var lastErr error
	for _, f := range files {
		if total <= limit {
			break
		}
		if err := remove(f.path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				lastErr = err
			}
			continue
		}
		total -= f.size
		freed += f.size
		n++
	}
	pruneEmpty(taskDir)
	if total > limit {
		return "", fmt.Errorf("session store of the task is over its limit (%s > %s) and could not be shrunk: %w",
			byteSize(total), byteSize(limit), lastErr)
	}
	return fmt.Sprintf("session store of the task was over its limit (%s > %s): removed %d oldest session files "+
		"(%s); sessions in them can no longer be resumed", byteSize(was), byteSize(limit), n, byteSize(freed)), nil
}

// pruneEmpty removes the directories below the CLIs' stores in taskDir that eviction left empty (the stores
// themselves stay: they are bound into the sandbox).
func pruneEmpty(taskDir string) {
	var dirs []string
	_ = filepath.WalkDir(taskDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && strings.Count(strings.TrimPrefix(p, taskDir), string(filepath.Separator)) > 1 {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- { // deepest first
		_ = os.Remove(dirs[i]) // only when empty
	}
}

// byteSize formats n bytes for a person.
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// storeName is an id as a directory name: itself when it is a plain name, else a hash of it (never a path).
func storeName(id string) string {
	if storeNameRe.MatchString(id) && id != "." && id != ".." {
		return id
	}
	h := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(h[:16])
}

func (s *SessionStore) maybeSweep() {
	s.mu.Lock()
	due := s.now().Sub(s.lastSweep) >= sweepEvery
	if due {
		s.lastSweep = s.now()
	}
	s.mu.Unlock()
	if due {
		s.Sweep()
	}
}

// Sweep removes the stores of tasks no attempt used for MaxAge and returns them.
func (s *SessionStore) Sweep() []string {
	cutoff := s.now().Add(-s.maxAge())
	var removed []string
	projects, _ := os.ReadDir(s.Root)
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		pd := filepath.Join(s.Root, p.Name())
		tasks, _ := os.ReadDir(pd)
		for _, t := range tasks {
			fi, err := t.Info()
			if err != nil || !fi.IsDir() {
				continue
			}
			td := filepath.Join(pd, t.Name())
			if !fi.ModTime().Before(cutoff) {
				continue // shrinking is Prepare's: it reports it, and an attempt may be using the store now
			}
			unlock(td)
			if os.RemoveAll(td) == nil { // RemoveAll never follows a symlink inside
				removed = append(removed, td)
			}
		}
		_ = os.Remove(pd) // only when empty
	}
	return removed
}

// unlock makes every directory below dir owner-writable again (an agent may have made one read-only), so the sweep
// can remove it; symlinks are not followed (WalkDir reports them as such).
func unlock(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // G302: directories need the x bit; 0700 is owner-only
		}
		return nil
	})
}
