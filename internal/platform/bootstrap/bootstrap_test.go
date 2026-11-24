package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

func run(t *testing.T, dir string) (*Installation, error) {
	t.Helper()
	inst, err := Run(t.Context(), Options{DataDir: dir})
	if err == nil {
		t.Cleanup(func() { _ = inst.Close() })
	}
	return inst, err
}

func mustRun(t *testing.T, dir string) *Installation {
	t.Helper()
	inst, err := run(t, dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return inst
}

func TestFreshInstall(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "data")
	inst := mustRun(t, dir)
	if !inst.Fresh {
		t.Fatal("Fresh = false on a clean data dir")
	}
	if inst.ID == "" {
		t.Fatal("installation id is empty")
	}
	for _, p := range []string{DBPath(dir), KeyPath(dir)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s not created: %v", p, err)
		}
	}
	if filepath.Dir(KeyPath(dir)) == filepath.Dir(DBPath(dir)) {
		t.Fatal("key must live outside the database directory")
	}
	if v, err := db.Version(t.Context(), inst.DB); err != nil || v < 1 {
		t.Fatalf("schema version = %d, %v", v, err)
	}
	ct, err := inst.Box.Seal([]byte("x"), "t")
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := inst.Box.Open(ct, "t"); err != nil || string(pt) != "x" {
		t.Fatalf("box round trip = %q, %v", pt, err)
	}
}

func TestRestartKeepsInstallation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := mustRun(t, dir)
	id := first.ID
	key, _ := os.ReadFile(KeyPath(dir))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := mustRun(t, dir)
	if second.Fresh || second.ID != id {
		t.Fatalf("restart: Fresh=%v ID=%q, want false %q", second.Fresh, second.ID, id)
	}
	if key2, _ := os.ReadFile(KeyPath(dir)); string(key2) != string(key) {
		t.Fatal("key changed on restart")
	}
}

func TestExistingDatabaseWithoutKeyFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := mustRun(t, dir)
	_ = first.Close()
	if err := os.Remove(KeyPath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, dir); !errors.Is(err, secrets.ErrKeyMissing) {
		t.Fatalf("err = %v, want ErrKeyMissing", err)
	}
	if _, err := os.Stat(KeyPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was generated")
	}
}

func TestWrongKeyFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := mustRun(t, dir)
	_ = first.Close()
	if err := os.Remove(KeyPath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.CreateKey(KeyPath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, dir); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("err = %v, want ErrWrongKey", err)
	}
}

func TestKeyWithoutDatabaseIsReused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	k, err := secrets.CreateKey(KeyPath(dir)) // e.g. bootstrap interrupted after the key was written
	if err != nil {
		t.Fatal(err)
	}
	inst := mustRun(t, dir)
	if !inst.Fresh {
		t.Fatal("want fresh install")
	}
	if got, _ := secrets.LoadKey(KeyPath(dir)); got != k {
		t.Fatal("existing key was replaced")
	}
}

func TestSecondInstanceIsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustRun(t, dir)
	if _, err := run(t, dir); !errors.Is(err, datadir.ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
}

func TestNewerSchemaFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := mustRun(t, dir)
	if _, err := first.DB.ExecContext(t.Context(),
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES (999, 'future', 'now')"); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if _, err := run(t, dir); !errors.Is(err, db.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
}

func TestFailedRunReleasesLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := mustRun(t, dir)
	_ = first.Close()
	_ = os.Remove(KeyPath(dir))
	if _, err := run(t, dir); err == nil {
		t.Fatal("want error")
	}
	d, err := datadir.Open(dir)
	if err != nil {
		t.Fatalf("lock still held after failed Run: %v", err)
	}
	_ = d.Close()
}
