// Package datadir prepares the Control Plane data directory and holds an exclusive lock on it, so only one
// Control Plane instance ever writes to a given SQLite database.
package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another process already holds the data directory.
var ErrLocked = errors.New("data directory is in use by another ai-platform process")

// LockName is the lock file inside the data directory.
const LockName = "ai-platform.lock"

// Dir is an opened, locked data directory. Close releases the lock.
type Dir struct {
	path string
	lock *os.File
}

// Open creates dir (mode 0700) if needed, checks that it is writable and takes an exclusive lock.
func Open(dir string) (*Dir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving data dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir %s: %w", abs, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("checking data dir %s: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("data dir %s is not a directory", abs)
	}
	if st.Mode().Perm()&0o077 != 0 { // pre-existing dirs (docker volumes, ./data) may be world-readable
		if err := os.Chmod(abs, 0o700); err != nil { //nolint:gosec // G302: directories need the x bit; 0700 is owner-only
			return nil, fmt.Errorf("restricting data dir %s to mode 0700: %w", abs, err)
		}
	}
	f, err := os.OpenFile(filepath.Join(abs, LockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("data dir %s is not writable: %w", abs, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", abs, ErrLocked)
		}
		return nil, fmt.Errorf("locking data dir %s: %w", abs, err)
	}
	return &Dir{path: abs, lock: f}, nil
}

// Path is the absolute data directory path.
func (d *Dir) Path() string { return d.path }

// Join returns a path inside the data directory.
func (d *Dir) Join(elem ...string) string { return filepath.Join(append([]string{d.path}, elem...)...) }

// Close releases the lock. The flock belongs to the open file description, which a child forked by any goroutine
// of this process shares until its exec closes the inherited copy; merely closing our descriptor would leave the
// lock held for that moment, so the next Open of the directory (import starting the imported installation, serve
// after stop) could fail with ErrLocked. Unlocking explicitly releases it for every copy at once.
func (d *Dir) Close() error {
	unlockErr := syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
	if err := d.lock.Close(); err != nil {
		return fmt.Errorf("releasing data dir lock: %w", err)
	}
	if unlockErr != nil {
		return fmt.Errorf("releasing data dir lock: %w", unlockErr)
	}
	return nil
}
