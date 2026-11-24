package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndLoadKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secrets", "master.key")
	k, err := CreateKey(path)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, want 0600", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if dst.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key dir mode = %v, want private", dst.Mode().Perm())
	}
	loaded, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if loaded != k {
		t.Fatal("loaded key differs from created key")
	}
	if _, err := CreateKey(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second CreateKey err = %v, want ErrExist (never overwrite a key)", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp*")); len(leftovers) > 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestLoadKeyErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := bytes.Repeat([]byte{7}, KeySize)
	tests := []struct {
		name string
		path string
		want error
	}{
		{"missing", filepath.Join(dir, "nope.key"), ErrKeyMissing},
		{"short", write("short.key", []byte("abc"), 0o600), ErrKeyInvalid},
		{"group readable", write("open.key", good, 0o644), ErrKeyPermissions},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadKey(tt.path); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestBoxRoundTrip(t *testing.T) {
	t.Parallel()
	var k Key
	copy(k[:], bytes.Repeat([]byte{1}, KeySize))
	box, err := NewBox(k)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := box.Seal([]byte("token-123"), "youtrack.token")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("token-123")) {
		t.Fatal("ciphertext contains plaintext")
	}
	ct2, _ := box.Seal([]byte("token-123"), "youtrack.token")
	if bytes.Equal(ct, ct2) {
		t.Fatal("two seals of the same plaintext are identical (nonce reuse)")
	}
	pt, err := box.Open(ct, "youtrack.token")
	if err != nil || string(pt) != "token-123" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
}

func TestBoxRejectsTampering(t *testing.T) {
	t.Parallel()
	var k, other Key
	k[0], other[0] = 1, 2
	box, _ := NewBox(k)
	otherBox, _ := NewBox(other)
	ct, _ := box.Seal([]byte("secret"), "a")
	flipped := bytes.Clone(ct)
	flipped[len(flipped)-1] ^= 1
	tests := []struct {
		name string
		box  *Box
		ct   []byte
		aad  string
	}{
		{"wrong key", otherBox, ct, "a"},
		{"wrong aad", box, ct, "b"},
		{"flipped bit", box, flipped, "a"},
		{"truncated", box, ct[:5], "a"},
		{"empty", box, nil, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.box.Open(tt.ct, tt.aad); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("err = %v, want ErrDecrypt", err)
			}
		})
	}
}

func TestCreateKeyRemovesStaleTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, ".master.key.tmp123")
	if err := os.WriteFile(stale, []byte("old key bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateKey(filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temp key file left behind")
	}
}

func TestLoadKeyChecksTheFileItReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "world-readable.key")
	if err := os.WriteFile(target, bytes.Repeat([]byte{1}, KeySize), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(target, 0o644)
	link := filepath.Join(dir, "master.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(link); !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("err = %v, want ErrKeyPermissions for a symlink to a world-readable file", err)
	}
	if _, err := LoadKey(dir); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("directory as key: err = %v, want ErrKeyInvalid", err)
	}
}

func TestInstallKeyFallsBackWhenLinkUnsupported(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "master.key")
	var k Key
	k[0] = 9
	noLink := func(string, string) error {
		return &os.LinkError{Op: "link", Err: errors.New("operation not supported")}
	}
	if err := installKey(path, k, noLink); err != nil {
		t.Fatalf("installKey: %v", err)
	}
	got, err := LoadKey(path)
	if err != nil || got != k {
		t.Fatalf("LoadKey = %v, %v", got[0], err)
	}
	if err := installKey(path, k, noLink); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fallback overwrote an existing key: err = %v", err)
	}
}
