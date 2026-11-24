package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesPrivateDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nested", "data")
	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("data dir mode = %v, want private dir", st.Mode())
	}
	if d.Path() != dir {
		t.Fatalf("Path() = %q", d.Path())
	}
	if got := d.Join("platform.db"); got != filepath.Join(dir, "platform.db") {
		t.Fatalf("Join = %q", got)
	}
}

func TestSecondOpenIsLocked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open err = %v, want ErrLocked", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	d2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	_ = d2.Close()
}

func TestOpenUnwritableDirFails(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	if _, err := Open(filepath.Join(parent, "data")); err == nil {
		t.Fatal("want error for unwritable parent")
	}
	if _, err := Open(parent); err == nil {
		t.Fatal("want error for read-only data dir")
	}
}

func TestOpenRejectsFile(t *testing.T) {
	t.Parallel()
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Fatal("want error when data dir is a file")
	}
}

func TestOpenTightensExistingDirMode(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // defeat umask
		t.Fatal(err)
	}
	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st, _ := os.Stat(dir)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", st.Mode().Perm())
	}
}
