package secfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteNewCreatesOwnerOnlyFileOnce(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "key")
	if err := WriteNew(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode(), err)
	}
	if err := WriteNew(p, []byte("two")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second WriteNew err = %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Fatalf("file replaced: %q", b)
	}
	if left, _ := filepath.Glob(p + ".tmp*"); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// Concurrent creators: exactly one wins, nobody overwrites.
func TestWriteNewRace(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "key")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 8 {
		wg.Go(func() {
			if WriteNew(p, []byte{byte('a' + i)}) == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins = %d", wins)
	}
}

func TestWriteReplace(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.json")
	for _, v := range []string{"a", "b"} {
		if err := WriteReplace(p, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := ReadPrivate(p, 10)
	if err != nil || string(b) != "b" {
		t.Fatalf("read = %q, %v", b, err)
	}
}

func TestReadPrivateRejectsFilesOthersCanAccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, mode := range []fs.FileMode{0o640, 0o604, 0o660, 0o644, 0o666} {
		p := filepath.Join(dir, mode.String())
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPrivate(p, 10); !errors.Is(err, ErrInsecure) {
			t.Errorf("mode %v: err = %v", mode, err)
		}
	}
}

func TestReadPrivateRejectsSymlinksAndDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(link, 10); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := ReadPrivate(dir, 10); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestReadPrivateLimitAndMissing(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(p, make([]byte, 11), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(p, 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ReadPrivate(p+"-missing", 10); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p := filepath.Join(root, "a", "b")
	if err := EnsureDir(p); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v", st.Mode())
	}
	// A pre-existing, too open directory is tightened.
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(p); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode after tighten = %v", st.Mode())
	}
	f := filepath.Join(root, "file")
	_ = os.WriteFile(f, nil, 0o600)
	if err := EnsureDir(f); err == nil {
		t.Fatal("file accepted as directory")
	}
}
