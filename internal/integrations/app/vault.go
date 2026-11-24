package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// Credentials is the port through which integration adapters (YouTrack KIN-27, GitHub App KIN-28, network profiles
// KIN-30, Telegram KIN-39) read a credential at the moment they call the external system. The value is a
// domain.Secret, which prints as "[redacted]" everywhere; adapters call Reveal only to put it on the wire and never
// copy it into payloads, errors or logs. *Vault implements it.
type Credentials interface {
	Credential(ctx context.Context, name domain.SecretName) (domain.Secret, error) // domain.ErrSecretNotSet
}

// Cipher seals values with the installation master key (secrets.Box).
type Cipher interface {
	Seal(plaintext []byte, aad string) ([]byte, error)
	Open(ciphertext []byte, aad string) ([]byte, error)
}

// SecretStore keeps encrypted credentials. Every method runs on the caller's transaction (db.Conn).
type SecretStore interface {
	PutSecret(ctx context.Context, name domain.SecretName, ciphertext []byte, by string, at time.Time) error
	SecretCiphertext(ctx context.Context, name domain.SecretName) ([]byte, error)      // domain.ErrSecretNotSet
	SecretMeta(ctx context.Context, name domain.SecretName) (domain.SecretMeta, error) // Set=false when missing
	DeleteSecret(ctx context.Context, name domain.SecretName) error
}

// Vault encrypts each credential separately with the installation key (AES-256-GCM); the credential's name is the
// associated data, so a ciphertext copied to another slot does not decrypt. There is no API that returns a stored
// value to a client: only Credential, for adapters, and Meta, for "set, updated <date>".
type Vault struct {
	Store  SecretStore
	Cipher Cipher
	Now    func() time.Time
}

var errVaultNotConfigured = errors.New("integrations: vault not configured")

func aad(name domain.SecretName) string { return "integration_secret:" + string(name) }

// Put stores (or replaces) a credential.
func (v *Vault) Put(ctx context.Context, name domain.SecretName, s domain.Secret, by string) error {
	if v == nil || v.Cipher == nil || v.Store == nil {
		return errVaultNotConfigured
	}
	if s.IsZero() {
		return &domain.ValidationError{Field: "secret", Code: "required"}
	}
	ct, err := v.Cipher.Seal([]byte(s.Reveal()), aad(name))
	if err != nil {
		return fmt.Errorf("encrypting credential %s: %w", name, err)
	}
	if err := v.Store.PutSecret(ctx, name, ct, by, v.Now()); err != nil {
		return fmt.Errorf("storing credential %s: %w", name, err)
	}
	return nil
}

// Credential decrypts a stored credential.
func (v *Vault) Credential(ctx context.Context, name domain.SecretName) (domain.Secret, error) {
	if v == nil || v.Cipher == nil || v.Store == nil {
		return domain.Secret{}, errVaultNotConfigured
	}
	ct, err := v.Store.SecretCiphertext(ctx, name)
	if err != nil {
		return domain.Secret{}, err
	}
	p, err := v.Cipher.Open(ct, aad(name))
	if err != nil {
		return domain.Secret{}, fmt.Errorf("decrypting credential %s: %w", name, err)
	}
	return domain.NewSecret(string(p))
}

// Meta reports whether a credential is set and when it changed.
func (v *Vault) Meta(ctx context.Context, name domain.SecretName) (domain.SecretMeta, error) {
	if v == nil || v.Store == nil {
		return domain.SecretMeta{}, errVaultNotConfigured
	}
	return v.Store.SecretMeta(ctx, name)
}

// Delete removes a credential (no error if it is not set).
func (v *Vault) Delete(ctx context.Context, name domain.SecretName) error {
	if v == nil || v.Store == nil {
		return errVaultNotConfigured
	}
	return v.Store.DeleteSecret(ctx, name)
}
