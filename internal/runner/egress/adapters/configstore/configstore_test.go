package configstore_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/configstore"
)

func TestConfigFileIsPrivate(t *testing.T) {
	t.Parallel()
	f := configstore.File{Path: filepath.Join(t.TempDir(), "network", "config.json")}
	if _, ok, err := f.Load(); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	cu := protocol.ConfigUpdate{Revision: 3, Network: protocol.NetworkConfig{Default: protocol.NetworkProfile{
		Proxy: &protocol.NetworkProxy{Scheme: "http", Host: "p", Port: 1, Password: "pw"}}}}
	if err := f.Save(cu); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(f.Path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	got, ok, err := f.Load()
	if !ok || err != nil || got.Revision != 3 || got.Network.Default.Proxy.Password != "pw" {
		t.Fatalf("load %+v %v %v", got, ok, err)
	}
}
