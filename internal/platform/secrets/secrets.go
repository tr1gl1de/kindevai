// Package secrets manages the installation master key and encrypts stored secrets with AES-256-GCM.
//
// The key lives in its own file, separate from SQLite. It is created exactly once and never regenerated
// silently: a missing key for an existing database is an error the operator must resolve.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeySize is the master key length in bytes (AES-256).
const KeySize = 32

// Key is the installation master key.
type Key [KeySize]byte

var (
	ErrKeyMissing     = errors.New("encryption key file not found")
	ErrKeyInvalid     = errors.New("encryption key file is malformed")
	ErrKeyPermissions = errors.New("encryption key file is accessible to other users (want mode 0600)")
	ErrDecrypt        = errors.New("decrypting secret failed: wrong key or corrupted data")
)

// CreateKey generates a random key and writes it to path with mode 0600. It never overwrites an existing
// file (returns an error wrapping os.ErrExist) and leaves no partial key behind on failure.
func CreateKey(path string) (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("generating encryption key: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Key{}, fmt.Errorf("creating key directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".master.key.tmp*")
	if err != nil {
		return Key{}, fmt.Errorf("writing encryption key: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful link+remove
	if err := writeAndSync(tmp, k[:]); err != nil {
		return Key{}, fmt.Errorf("writing encryption key: %w", err)
	}
	// Link is atomic and, unlike rename, fails when the target exists.
	if err := os.Link(tmp.Name(), path); err != nil {
		return Key{}, fmt.Errorf("installing encryption key: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return Key{}, fmt.Errorf("syncing key directory: %w", err)
	}
	return k, nil
}

func writeAndSync(f *os.File, b []byte) error {
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// LoadKey reads the key at path. It refuses files readable or writable by group/others.
func LoadKey(path string) (Key, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Key{}, fmt.Errorf("%s: %w", path, ErrKeyMissing)
	}
	if err != nil {
		return Key{}, fmt.Errorf("reading encryption key: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return Key{}, fmt.Errorf("%s has mode %v: %w", path, st.Mode().Perm(), ErrKeyPermissions)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Key{}, fmt.Errorf("reading encryption key: %w", err)
	}
	if len(b) != KeySize {
		return Key{}, fmt.Errorf("%s: %w", path, ErrKeyInvalid)
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// Box encrypts and decrypts values with the master key. The associated data (for example the setting
// name) binds a ciphertext to its slot, so it cannot be moved to another field.
type Box struct{ aead cipher.AEAD }

// version prefixes every ciphertext to allow future algorithm or key rotation.
const version byte = 1

// NewBox returns a Box for key k.
func NewBox(k Key) (*Box, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, fmt.Errorf("initialising cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialising cipher: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext; output is version || nonce || ciphertext+tag.
func (b *Box) Seal(plaintext []byte, aad string) ([]byte, error) {
	out := make([]byte, 1+b.aead.NonceSize(), 1+b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	out[0] = version
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return b.aead.Seal(out, out[1:], plaintext, []byte(aad)), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (b *Box) Open(ciphertext []byte, aad string) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < 1+ns+b.aead.Overhead() || ciphertext[0] != version {
		return nil, ErrDecrypt
	}
	pt, err := b.aead.Open(nil, ciphertext[1:1+ns], ciphertext[1+ns:], []byte(aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
