//go:build unix

package cliagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// back. Stores no attempt used for MaxAge are removed. A task's store (all its CLIs together) is bounded by Limit
// bytes and EntryLimit entries (KIN-96, KIN-115): before each attempt its oldest session files are removed until it
// is within 3/4 of both, and the attempt says so; during the attempt the runner stops it once it is over either
// (OverQuota, polled by the executor).
type SessionStore struct {
	Root   string
	MaxAge time.Duration // DefaultSessionMaxAge when zero
	Limit  int64         // bytes of session files per task; DefaultSessionLimit when zero
	// EntryLimit bounds the entries (files, directories, symlinks) of a task's store; DefaultSessionEntryLimit when
	// zero (KIN-115: an agent cannot use up the inodes either).
	EntryLimit int64
	Now        func() time.Time // time.Now when nil

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

// DefaultSessionEntryLimit bounds the entries of one task's store: a session is a handful of files, so this keeps
// many while an agent cannot use up the inodes of the runner's data directory through its store.
const DefaultSessionEntryLimit = 20000

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

func (s *SessionStore) entryLimit() int64 {
	if s.EntryLimit <= 0 {
		return DefaultSessionEntryLimit
	}
	return s.EntryLimit
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

// usage is what a task's store holds: bytes of regular files and entries (files, directories, symlinks, anything
// else) below the task's directory; files lists the entries that are not directories when asked for.
type usage struct {
	bytes   int64
	entries int64
	files   []sessionFile
}

// readBatch is how many directory entries measure reads at a time: a directory with millions of entries is never
// loaded whole.
const readBatch = 256

// errOver stops measure once the store is over its bounds.
var errOver = errors.New("over")

// measure walks the task's store at taskDir, one directory at a time and readBatch entries at a time, without
// following symlinks. A directory an agent left unreadable or read-only is made owner-accessible again before it is
// read (the store and everything in it belong to the runner's user). It stops early (over=true) once the store holds
// more than bytes or entries (when they are positive), so its cost is bounded by them however many entries an agent
// made; an error is returned when the store cannot be measured (callers fail closed).
func measure(taskDir string, list bool, bytes, entries int64) (u usage, over bool, err error) {
	stack := []string{taskDir}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		err := readDir(dir, dir == taskDir, func(p string, fi fs.FileInfo) error {
			u.entries++
			switch {
			case fi.IsDir():
				stack = append(stack, p)
			case fi.Mode().IsRegular():
				u.bytes += fi.Size()
				fallthrough
			default:
				if list {
					size := int64(0)
					if fi.Mode().IsRegular() {
						size = fi.Size()
					}
					u.files = append(u.files, sessionFile{p, size, fi.ModTime()})
				}
			}
			if (bytes > 0 && u.bytes > bytes) || (entries > 0 && u.entries > entries) {
				return errOver
			}
			return nil
		})
		if errors.Is(err, errOver) {
			return u, true, nil
		}
		if err != nil {
			return u, false, err
		}
	}
	return u, false, nil
}

// readDir calls fn with the lstat of every entry of dir, readBatch at a time. A directory that is gone meanwhile
// (but the store's own) has no entries; one an agent made inaccessible is made owner-accessible and opened again.
func readDir(dir string, root bool, fn func(string, fs.FileInfo) error) error {
	var f *os.File
	var err error
	for range 3 { // an agent may change the mode back between the chmod and the open: try again, then give up
		if f, err = os.Open(dir); err == nil || !errors.Is(err, fs.ErrPermission) {
			break
		}
		_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: directories need the x bit; 0700 is owner-only
	}
	if err != nil {
		if !root && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o700 != 0o700 { // readable, but not searchable
		_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: as above
	}
	for {
		batch, err := f.ReadDir(readBatch)
		for _, d := range batch {
			p := filepath.Join(dir, d.Name())
			fi, err := os.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed meanwhile
			}
			if err != nil {
				return err
			}
			if err := fn(p, fi); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// OverQuota measures the store of task of project while an attempt uses it (KIN-115) and says why it is over
// Limit or EntryLimit ("" while within them, and when the task has no store). A store that cannot be measured (an
// agent keeps a directory of it inaccessible) counts as over: the check never fails open. The walk stops as soon as
// the store is over, so its cost is bounded by EntryLimit however many entries an agent made.
func (s *SessionStore) OverQuota(project, task string) string {
	taskDir := filepath.Join(s.Root, storeName(project), storeName(task))
	limit, entries := s.limit(), s.entryLimit()
	u, over, err := measure(taskDir, false, limit, entries)
	switch {
	case errors.Is(err, fs.ErrNotExist) && u.entries == 0:
		return ""
	case err != nil:
		return "session store of the task cannot be measured during the attempt: " + relErr(taskDir, err)
	case !over:
		return ""
	case u.entries > entries:
		return fmt.Sprintf("session store of the task went over its quota during the attempt (> %d entries)", entries)
	}
	return fmt.Sprintf("session store of the task went over its quota during the attempt (> %s of session files)",
		byteSize(limit))
}

// relErr is err with the runner's data directory path left out (the attempt's failure shows it).
func relErr(taskDir string, err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		rel, rerr := filepath.Rel(taskDir, pe.Path)
		if rerr != nil {
			rel = filepath.Base(pe.Path)
		}
		return fmt.Sprintf("%s %s: %v", pe.Op, rel, pe.Err)
	}
	return err.Error()
}

// wipeAt is how far over EntryLimit Prepare still lists a store to evict its oldest files; beyond it every session
// file of the task is removed (listing it would cost too much).
const wipeAt = 4

// shrink brings the task's store at taskDir within 3/4 of Limit and EntryLimit (KIN-115: an attempt that goes over
// the limits themselves is stopped, so it starts with headroom): it removes the oldest entries that are not
// directories, then the directories that left empty, and says what it removed ("" when the store was within).
// Only regular files count towards the bytes; symlinks are neither followed nor counted as bytes.
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
	limit, entryLimit := s.limit(), s.entryLimit()
	lowBytes, lowEntries := limit/4*3, entryLimit/4*3
	u, huge, err := measure(taskDir, true, 0, wipeAt*entryLimit)
	if err != nil {
		return "", fmt.Errorf("session store: measuring %s: %s", filepath.Base(taskDir), relErr(taskDir, err))
	}
	if huge {
		return s.wipe(taskDir, entryLimit)
	}
	if u.bytes <= lowBytes && u.entries <= lowEntries {
		return "", nil
	}
	files := u.files
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
	total, entries := u.bytes, u.entries
	var n int
	var freed int64
	var lastErr error
	for _, f := range files {
		if total <= lowBytes && entries <= lowEntries {
			break
		}
		if err := remove(f.path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				lastErr = err
			}
			continue
		}
		total -= f.size
		entries--
		freed += f.size
		n++
	}
	pruneEmpty(taskDir)
	after, _, err := measure(taskDir, false, 0, 0)
	if err != nil {
		return "", fmt.Errorf("session store: measuring %s: %s", filepath.Base(taskDir), relErr(taskDir, err))
	}
	if after.bytes > limit || after.entries > entryLimit {
		return "", fmt.Errorf("session store of the task is over its limit (%s in %d entries > %s, %d entries) and "+
			"could not be shrunk: %w", byteSize(after.bytes), after.entries, byteSize(limit), entryLimit, lastErr)
	}
	return fmt.Sprintf("session store of the task was over its limit for a new attempt (%s in %d entries; an attempt "+
		"starts within 3/4 of %s and %d entries): removed %d oldest session files (%s); sessions in them can no longer "+
		"be resumed", byteSize(u.bytes), u.entries, byteSize(limit), entryLimit, n, byteSize(freed)), nil
}

// wipe removes everything in the CLIs' stores of the task at taskDir (the stores themselves stay: they are bound
// into the sandbox) when it holds too many entries to list.
func (s *SessionStore) wipe(taskDir string, entryLimit int64) (string, error) {
	stores, err := os.ReadDir(taskDir)
	if err != nil {
		return "", fmt.Errorf("session store: %w", err)
	}
	for _, st := range stores {
		dir := filepath.Join(taskDir, st.Name())
		if !st.IsDir() {
			_ = os.Remove(dir)
			continue
		}
		_ = os.Chmod(dir, 0o700) //nolint:gosec // G302: directories need the x bit; 0700 is owner-only
		children, err := os.ReadDir(dir)
		if err != nil {
			return "", fmt.Errorf("session store: %w", err)
		}
		for _, c := range children {
			p := filepath.Join(dir, c.Name())
			err := os.RemoveAll(p) // reads in batches and never follows a symlink inside
			if err != nil {        // an agent made a directory in it inaccessible
				unlock(p)
				err = os.RemoveAll(p)
			}
			if err != nil {
				return "", fmt.Errorf("session store of the task is over its limit and could not be emptied: %s",
					relErr(taskDir, err))
			}
		}
	}
	return fmt.Sprintf("session store of the task held more than %d entries (its limit is %d): removed all its "+
		"session files; no session of the task can be resumed", wipeAt*entryLimit, entryLimit), nil
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
