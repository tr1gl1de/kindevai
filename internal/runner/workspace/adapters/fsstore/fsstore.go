// Package fsstore is the workspace layout in the runner data dir (KIN-29, KB KIN-A-16): bare repository caches in
// repos/<owner>/<name>.git, worktrees in workspaces/<owner>/<name>/<task>-<step> (both counted by the KIN-55 space
// meter), writer locks and workspace records in workspaces/.locks and workspaces/.meta. Every name comes from
// validated ids, so no path leaves the data dir.
package fsstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/tr1gl1de/kindevai/internal/platform/diskusage"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
)

// Store is the layout under DataDir.
type Store struct{ DataDir string }

var _ app.Store = Store{}

var stepRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s Store) workspaces() string { return filepath.Join(s.DataDir, "workspaces") }

// MirrorPath implements app.Store.
func (s Store) MirrorPath(owner, name string) string {
	return filepath.Join(s.DataDir, "repos", owner, name+".git")
}

// WorktreePath implements app.Store.
func (s Store) WorktreePath(owner, name, leaf string) string {
	return filepath.Join(s.workspaces(), owner, name, leaf)
}

// Lock implements app.Store with a non-blocking flock on workspaces/.locks/<owner>/<name>/<leaf>.lock.
func (s Store) Lock(dir string) (func(), error) {
	rel, err := filepath.Rel(s.workspaces(), dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("workspace %s is outside the data dir", dir)
	}
	p := filepath.Join(s.workspaces(), ".locks", rel+".lock")
	if err := secfile.EnsureDir(filepath.Dir(p)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: validated layout
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: fd fits int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, app.ErrBusy
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: fd fits int
		_ = f.Close()
	}, nil
}

func (s Store) metaPath(step string, fence int64) (string, error) {
	if !stepRe.MatchString(step) || fence < 1 {
		return "", fmt.Errorf("invalid attempt %q/%d", step, fence)
	}
	return filepath.Join(s.workspaces(), ".meta", step+"-"+strconv.FormatInt(fence, 10)+".json"), nil
}

// SaveMeta implements app.Store (atomic, 0600).
func (s Store) SaveMeta(m app.Meta) error {
	p, err := s.metaPath(m.Step, m.Fence)
	if err != nil {
		return err
	}
	if err := secfile.EnsureDir(filepath.Dir(p)); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return secfile.WriteReplace(p, b)
}

// LoadMeta implements app.Store.
func (s Store) LoadMeta(step string, fence int64) (app.Meta, bool, error) {
	p, err := s.metaPath(step, fence)
	if err != nil {
		return app.Meta{}, false, err
	}
	b, err := secfile.ReadPrivate(p, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return app.Meta{}, false, nil
	}
	if err != nil {
		return app.Meta{}, false, err
	}
	var m app.Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return app.Meta{}, false, fmt.Errorf("workspace record %s: %w", p, err)
	}
	return m, true, nil
}

// DeleteMeta implements app.Store.
func (s Store) DeleteMeta(step string, fence int64) error {
	p, err := s.metaPath(step, fence)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Free implements app.Store: the bytes available to an unprivileged writer on the data dir's filesystem.
func (s Store) Free() (uint64, error) {
	u, err := diskusage.Stat(s.DataDir)
	if err != nil {
		return 0, err
	}
	return uint64(max(u.AvailableBytes, 0)), nil
}
