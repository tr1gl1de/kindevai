// Package configstore keeps the runner's network configuration in an owner-only file (it holds proxy passwords).
package configstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

// maxBytes bounds the file (a command body is at most protocol.MaxBodyBytes).
const maxBytes = 1 << 20

// File stores the configuration at Path (mode 0600 in a 0700 directory).
type File struct{ Path string }

// Load reads the configuration; ok=false when none was stored yet.
func (f File) Load() (protocol.ConfigUpdate, bool, error) {
	b, err := secfile.ReadPrivate(f.Path, maxBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return protocol.ConfigUpdate{}, false, nil
	}
	if err != nil {
		return protocol.ConfigUpdate{}, false, fmt.Errorf("reading network configuration: %w", err)
	}
	var cu protocol.ConfigUpdate
	if err := json.Unmarshal(b, &cu); err != nil {
		return protocol.ConfigUpdate{}, false, fmt.Errorf("network configuration %s is corrupt", f.Path)
	}
	return cu, true, nil
}

// Save replaces the configuration atomically.
func (f File) Save(cu protocol.ConfigUpdate) error {
	if err := secfile.EnsureDir(filepath.Dir(f.Path)); err != nil {
		return err
	}
	b, err := json.Marshal(cu)
	if err != nil {
		return err
	}
	return secfile.WriteReplace(f.Path, b)
}
