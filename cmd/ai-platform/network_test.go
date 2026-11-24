package main

import (
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	networkapp "github.com/tr1gl1de/kindevai/internal/network/app"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

// Proxy passwords live in the integrations vault under "network_profile.<id>.password" (KIN-26 slot format).
func TestNetworkSecretsInTheVault(t *testing.T) {
	t.Parallel()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	var k secrets.Key
	_, _ = rand.Read(k[:])
	box, _ := secrets.NewBox(k)
	s := networkSecrets{&integrationsapp.Vault{Store: integrationsdb.New(d), Cipher: box, Now: time.Now}}
	name := networkapp.PasswordSecret(strings.ToLower(rand.Text()))
	if _, ok, err := s.Get(t.Context(), name); ok || err != nil {
		t.Fatalf("unset: %v %v", ok, err)
	}
	if err := s.Put(t.Context(), name, "p@ss w0rd", ""); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.Get(t.Context(), name); v != "p@ss w0rd" || !ok || err != nil {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	if set, err := s.IsSet(t.Context(), name); !set || err != nil {
		t.Fatalf("set: %v %v", set, err)
	}
	if err := s.Delete(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	if set, _ := s.IsSet(t.Context(), name); set {
		t.Fatal("still set after delete")
	}
}
