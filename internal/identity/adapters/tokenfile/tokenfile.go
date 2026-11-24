// Package tokenfile hands the setup token to the operator through a 0600 file in the data directory.
// Reading it needs shell access to the host or container, which is the trusted channel; the token never
// appears in HTTP responses or logs.
package tokenfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Channel writes the current token to Path.
type Channel struct{ Path string }

// New returns a Channel writing to path.
func New(path string) *Channel { return &Channel{Path: path} }

// Publish atomically replaces the file with the token.
func (c *Channel) Publish(plain string) error {
	dir := filepath.Dir(c.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".setup-token.tmp*")
	if err != nil {
		return fmt.Errorf("writing setup token: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(plain + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing setup token: %w", err)
	}
	if err := tmp.Close(); err != nil { // CreateTemp files are already 0600
		return fmt.Errorf("writing setup token: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.Path); err != nil {
		return fmt.Errorf("installing setup token: %w", err)
	}
	return nil
}

// Clear removes the file; a missing file is fine.
func (c *Channel) Clear() error {
	if err := os.Remove(c.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing setup token: %w", err)
	}
	return nil
}
