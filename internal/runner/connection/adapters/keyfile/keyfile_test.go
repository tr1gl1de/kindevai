package keyfile

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Path: filepath.Join(t.TempDir(), "keys", "ed25519.pem")}
}

func TestLoadOrCreateGeneratesOwnerOnlyKeyOnce(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, err := s.Load(t.Context()); !errors.Is(err, app.ErrNoKey) {
		t.Fatalf("Load before create: %v", err)
	}
	id, created, err := s.LoadOrCreate(t.Context())
	if err != nil || !created {
		t.Fatalf("created = %v, %v", created, err)
	}
	st, err := os.Stat(s.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, %v", st.Mode(), err)
	}
	if dst, _ := os.Stat(filepath.Dir(s.Path)); dst.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode = %v", dst.Mode())
	}
	before, _ := os.ReadFile(s.Path)
	again, created, err := s.LoadOrCreate(t.Context())
	if err != nil || created || again.Fingerprint() != id.Fingerprint() {
		t.Fatalf("second LoadOrCreate created=%v err=%v", created, err)
	}
	after, _ := os.ReadFile(s.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("key file rewritten")
	}
	loaded, err := s.Load(t.Context())
	if err != nil || !loaded.PrivateKey().Equal(id.PrivateKey()) {
		t.Fatalf("Load = %v", err)
	}
	if blk, _ := pem.Decode(after); blk == nil || blk.Type != "PRIVATE KEY" {
		t.Fatal("key is not a PKCS#8 PEM")
	}
}

// AC: a key readable by other users is refused, and never replaced by a new one.
func TestKeyAccessibleByOthersIsRefused(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, _, err := s.LoadOrCreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path)
	if err := os.Chmod(s.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context()); !errors.Is(err, secfile.ErrInsecure) {
		t.Fatalf("Load err = %v", err)
	}
	if _, _, err := s.LoadOrCreate(t.Context()); !errors.Is(err, secfile.ErrInsecure) {
		t.Fatalf("LoadOrCreate err = %v", err)
	}
	if after, _ := os.ReadFile(s.Path); !bytes.Equal(before, after) {
		t.Fatal("insecure key was replaced")
	}
}

// A corrupt or foreign key is an error, never a reason to generate a new identity silently.
func TestCorruptOrForeignKeyIsNotRegenerated(t *testing.T) {
	t.Parallel()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(ec)
	for name, content := range map[string][]byte{
		"garbage": []byte("not a key"),
		"ecdsa":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		"empty":   {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			if err := secfile.EnsureDir(filepath.Dir(s.Path)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.Path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.LoadOrCreate(t.Context()); err == nil || errors.Is(err, app.ErrNoKey) {
				t.Fatalf("err = %v", err)
			}
			if after, _ := os.ReadFile(s.Path); !bytes.Equal(after, content) {
				t.Fatal("key replaced")
			}
		})
	}
}
