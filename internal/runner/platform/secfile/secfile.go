// Package secfile reads and writes the runner's private files (key, config, token file): owner-only modes,
// no symlinks, no silent replacement of a key. Shared kernel of the runner; no business rules.
package secfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrInsecure means a private file is accessible by other users or is not a regular file of this user.
var ErrInsecure = errors.New("file is accessible by other users")

// ErrTooLarge means a private file exceeds the expected size.
var ErrTooLarge = errors.New("file is larger than expected")

// ReadPrivate reads a regular file that only the current user can access (mode 0600 or stricter, owned by the
// current user), refusing symlinks. The checks run on the opened descriptor, so the file cannot be swapped between
// check and read.
func ReadPrivate(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink: %w", path, ErrInsecure)
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("checking %s: %w", path, err)
	}
	if err := CheckPrivate(path, st); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: %w (limit %d bytes)", path, ErrTooLarge, limit)
	}
	return b, nil
}

// CheckPrivate verifies that st describes a regular file only its owner, the current user, can access.
func CheckPrivate(path string, st fs.FileInfo) error {
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file: %w", path, ErrInsecure)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o, %w; run `chmod 600 %s`", path, perm, ErrInsecure, path)
	}
	if !ownedByMe(st) {
		return fmt.Errorf("%s is owned by another user: %w", path, ErrInsecure)
	}
	return nil
}

// WriteNew atomically creates path with mode 0600 and data. It fails with fs.ErrExist when path exists, so an
// existing file (a key) is never replaced, even by a concurrent writer.
func WriteNew(path string, data []byte) error {
	tmp, err := writeTemp(path, data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, path); err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

// WriteReplace atomically writes path with mode 0600, replacing it if it exists.
func WriteReplace(path string, data []byte) error {
	tmp, err := writeTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return syncDir(filepath.Dir(path))
}

func writeTemp(path string, data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*") // CreateTemp uses mode 0600
	if err != nil {
		return "", fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	name := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return name, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: the runner's own data directory
	if err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

// EnsureDir creates dir and its missing parents with mode 0700 and restricts an existing dir to 0700.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByMe(st) {
		return fmt.Errorf("%s is owned by another user: %w", dir, ErrInsecure)
	}
	if st.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: directories need the x bit; 0700 is owner-only
			return fmt.Errorf("restricting %s to mode 0700: %w", dir, err)
		}
	}
	return nil
}
