package tokenfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishWritesPrivateFileAndClearRemovesIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secrets", "setup-token")
	ch := New(path)
	if err := ch.Publish("tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := ch.Publish("tok-2"); err != nil { // rotation overwrites
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "tok-2\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	if err := ch.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("token file not removed")
	}
	if err := ch.Clear(); err != nil {
		t.Fatalf("Clear on missing file: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*")); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
}
