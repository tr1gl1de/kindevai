// Package keyfile stores the runner's Ed25519 private key as a PKCS#8 PEM file with mode 0600. The key is
// generated once; an existing key is never replaced, and a key other users can access is refused.
package keyfile

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

const maxKeyFile = 4 << 10

// Store is the key file at Path. Rand defaults to crypto/rand.
type Store struct {
	Path string
	Rand io.Reader
}

var _ app.KeyStore = (*Store)(nil)

// Load reads the key, or returns app.ErrNoKey when the file does not exist.
func (s *Store) Load(_ context.Context) (domain.Identity, error) {
	b, err := secfile.ReadPrivate(s.Path, maxKeyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return domain.Identity{}, app.ErrNoKey
	}
	if err != nil {
		return domain.Identity{}, fmt.Errorf("runner key: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return domain.Identity{}, fmt.Errorf("runner key %s is not a PKCS#8 PEM file; it is kept as is: %w", s.Path, domain.ErrInvalidKey)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return domain.Identity{}, fmt.Errorf("runner key %s: %w", s.Path, domain.ErrInvalidKey)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return domain.Identity{}, fmt.Errorf("runner key %s is not Ed25519: %w", s.Path, domain.ErrInvalidKey)
	}
	return domain.NewIdentity(priv)
}

// LoadOrCreate returns the stored key or generates one when the file does not exist.
func (s *Store) LoadOrCreate(ctx context.Context) (domain.Identity, bool, error) {
	id, err := s.Load(ctx)
	if !errors.Is(err, app.ErrNoKey) {
		return id, false, err
	}
	r := s.Rand
	if r == nil {
		r = rand.Reader
	}
	_, priv, err := ed25519.GenerateKey(r)
	if err != nil {
		return domain.Identity{}, false, fmt.Errorf("generating runner key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return domain.Identity{}, false, fmt.Errorf("encoding runner key: %w", err)
	}
	if err := secfile.EnsureDir(filepath.Dir(s.Path)); err != nil {
		return domain.Identity{}, false, err
	}
	err = secfile.WriteNew(s.Path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if errors.Is(err, fs.ErrExist) { // another process created it first: use that one
		id, err := s.Load(ctx)
		return id, false, err
	}
	if err != nil {
		return domain.Identity{}, false, err
	}
	id, err = domain.NewIdentity(priv)
	return id, true, err
}
