package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
)

var _ app.CommandCursor = CursorFile{}

// CursorFile stores the runner's position in the Control Plane's command stream (JSON, 0600, replaced atomically
// with fsync). A missing file is the start of the stream; a corrupt one is an error, never a silent reset (the
// command's Base would move it forward, but a reset epoch could re-apply commands).
type CursorFile struct{ Path string }

type cursorJSON struct {
	Epoch string `json:"epoch"`
	Last  int64  `json:"last"`
}

// Load reads the cursor.
func (f CursorFile) Load() (protocol.Cursor, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Cursor{}, nil
	}
	if err != nil {
		return protocol.Cursor{}, fmt.Errorf("reading command cursor: %w", err)
	}
	var c cursorJSON
	if err := json.Unmarshal(b, &c); err != nil || c.Last < 0 {
		return protocol.Cursor{}, fmt.Errorf("command cursor %s is corrupt", f.Path)
	}
	return protocol.Cursor{Epoch: c.Epoch, Last: c.Last}, nil
}

// Save replaces the cursor durably.
func (f CursorFile) Save(c protocol.Cursor) error {
	b, _ := json.Marshal(cursorJSON{Epoch: c.Epoch, Last: c.Last})
	return writeAtomic(f.Path, b)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if _, err := fh.Write(b); err != nil {
		_ = fh.Close()
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return fmt.Errorf("syncing %s: %w", filepath.Base(path), err)
	}
	if err := fh.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing %s: %w", filepath.Base(path), err)
	}
	return syncDir(filepath.Dir(path))
}
