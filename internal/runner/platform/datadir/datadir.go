// Package datadir prepares the runner's data directory (runner-data, KB KIN-A-16) with owner-only permissions and
// holds an exclusive lock on it, so one runner process owns one identity.
package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

// ErrLocked means another ai-runner process already uses the data directory.
var ErrLocked = errors.New("data directory is in use by another ai-runner process")

const (
	name     = "kindevai-runner"
	lockName = "runner.lock"
)

// Subdirectories of runner-data.
var subdirs = []string{"repos", "workspaces", "sessions", "events", "proxy", "keys"}

// Default is /var/lib/kindevai-runner for a system service (root) and $XDG_DATA_HOME/kindevai-runner or
// ~/.local/share/kindevai-runner for a user.
func Default(euid int, home, xdgDataHome string) string {
	if euid == 0 {
		return filepath.Join("/var/lib", name)
	}
	if filepath.IsAbs(xdgDataHome) {
		return filepath.Join(xdgDataHome, name)
	}
	return filepath.Join(home, ".local", "share", name)
}

// Dir is an opened, locked runner data directory. Close releases the lock.
type Dir struct {
	path string
	lock *os.File
}

// Open creates the directory tree with mode 0700 (tightening existing directories) and locks it.
func Open(dir string) (*Dir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving data dir %q: %w", dir, err)
	}
	if err := secfile.EnsureDir(abs); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(abs, lockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: our data dir
	if err != nil {
		return nil, fmt.Errorf("data dir %s is not writable: %w", abs, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: fd fits int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", abs, ErrLocked)
		}
		return nil, fmt.Errorf("locking data dir %s: %w", abs, err)
	}
	for _, sub := range subdirs {
		if err := secfile.EnsureDir(filepath.Join(abs, sub)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("data dir: %w", err)
		}
	}
	return &Dir{path: abs, lock: f}, nil
}

// Inspect returns the layout of dir without creating, tightening or locking anything, for read-only commands
// that must work while the daemon holds the lock.
func Inspect(dir string) (*Dir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving data dir %q: %w", dir, err)
	}
	return &Dir{path: abs}, nil
}

// Path is the absolute data directory.
func (d *Dir) Path() string { return d.path }

// KeyPath is the Ed25519 private key file.
func (d *Dir) KeyPath() string { return filepath.Join(d.path, "keys", "ed25519.pem") }

// ConfigPath is the registration/config file.
func (d *Dir) ConfigPath() string { return filepath.Join(d.path, "config.json") }

// Close releases the lock.
func (d *Dir) Close() error {
	if d.lock == nil {
		return nil
	}
	if err := d.lock.Close(); err != nil {
		return fmt.Errorf("releasing data dir lock: %w", err)
	}
	return nil
}
