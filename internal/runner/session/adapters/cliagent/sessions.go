//go:build unix

package cliagent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

// SessionStore keeps the CLIs' sessions of sandboxed attempts between attempts (KIN-86): the agent's HOME in the
// strict sandbox is a fresh tmpfs per attempt, so without it `--resume` of the next attempt finds nothing. Each
// project × task × CLI gets its own directory under Root (in the runner's data directory), bound read-write at the
// CLI's SessionDir below the agent's HOME: an attempt sees its own task's sessions only, never another task's or the
// host's. The store holds transcripts only: the CLI's login stays a copy (or the runner's token) and is never written
// back. Stores no attempt used for MaxAge are removed.
type SessionStore struct {
	Root   string
	MaxAge time.Duration    // DefaultSessionMaxAge when zero
	Now    func() time.Time // time.Now when nil

	mu        sync.Mutex
	lastSweep time.Time
}

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

// Prepare creates (or reuses) the store of agent for task of project and returns it with the path below HOME it is
// bound at. It marks the task's store as used now.
func (s *SessionStore) Prepare(project, task, agent string) (source, rel string, err error) {
	d, err := DialectFor(agent)
	if err != nil {
		return "", "", err
	}
	for _, n := range []string{project, task} {
		if !storeNameRe.MatchString(n) {
			return "", "", fmt.Errorf("session store: %q is not a plain name", n)
		}
	}
	s.maybeSweep()
	taskDir := filepath.Join(s.Root, project, task)
	source = filepath.Join(taskDir, agent)
	// Every level owner-only and no symlink (EnsureDir refuses one): the runner binds exactly this directory.
	for _, dir := range []string{s.Root, filepath.Join(s.Root, project), taskDir, source} {
		if err := secfile.EnsureDir(dir); err != nil {
			return "", "", fmt.Errorf("session store: %w", err)
		}
	}
	now := s.now()
	if err := os.Chtimes(taskDir, now, now); err != nil {
		return "", "", fmt.Errorf("session store: %w", err)
	}
	return source, d.SessionDir(), nil
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
			if err != nil || !fi.ModTime().Before(cutoff) {
				continue
			}
			td := filepath.Join(pd, t.Name())
			if os.RemoveAll(td) == nil { // RemoveAll never follows a symlink inside
				removed = append(removed, td)
			}
		}
		_ = os.Remove(pd) // only when empty
	}
	return removed
}
