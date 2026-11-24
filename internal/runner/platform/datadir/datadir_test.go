package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		euid      int
		home, xdg string
		want      string
	}{
		{0, "/root", "", "/var/lib/kindevai-runner"},
		{1000, "/home/u", "", "/home/u/.local/share/kindevai-runner"},
		{1000, "/home/u", "/data/xdg", "/data/xdg/kindevai-runner"},
		{1000, "/home/u", "relative", "/home/u/.local/share/kindevai-runner"}, // XDG spec: ignore relative paths
	}
	for _, tc := range cases {
		if got := Default(tc.euid, tc.home, tc.xdg); got != tc.want {
			t.Errorf("Default(%d, %q, %q) = %q, want %q", tc.euid, tc.home, tc.xdg, got, tc.want)
		}
	}
}

// AC: runner-data with repos/ workspaces/ sessions/ events/ proxy/ is created owner-only.
func TestOpenCreatesLayoutOwnerOnly(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runner-data")
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, sub := range []string{"", "repos", "workspaces", "sessions", "events", "proxy", "keys"} {
		st, err := os.Stat(filepath.Join(root, sub))
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", sub, st.Mode(), err)
		}
	}
	if d.KeyPath() != filepath.Join(root, "keys", "ed25519.pem") || d.ConfigPath() != filepath.Join(root, "config.json") {
		t.Fatalf("paths %s %s", d.KeyPath(), d.ConfigPath())
	}
}

func TestOpenTightensExistingDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for _, p := range []string{root, filepath.Join(root, "repos")} {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v", p, st.Mode())
		}
	}
}

func TestOpenIsExclusive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open err = %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := Open(root)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	_ = d2.Close()
}

// Read-only commands (fingerprint) work while the daemon holds the lock and create nothing.
func TestInspectNeitherLocksNorCreates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	in, err := Inspect(root)
	if err != nil || in.KeyPath() != d.KeyPath() {
		t.Fatalf("Inspect = %v", err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "nope")
	if _, err := Inspect(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Inspect created the directory")
	}
}
