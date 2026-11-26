package spool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

func TestCursorFileRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "commands.json")
	f := CursorFile{Path: path}
	if c, err := f.Load(); err != nil || c != (protocol.Cursor{}) {
		t.Fatalf("missing file = %+v, %v", c, err)
	}
	if err := f.Save(protocol.Cursor{Epoch: "e1", Last: 42}); err != nil {
		t.Fatal(err)
	}
	if c, err := (CursorFile{Path: path}).Load(); err != nil || c != (protocol.Cursor{Epoch: "e1", Last: 42}) {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(); err == nil {
		t.Fatal("a corrupt cursor must not silently reset to 0")
	}
}
