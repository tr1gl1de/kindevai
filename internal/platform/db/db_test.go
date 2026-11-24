package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func openTemp(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestOpenUsesWALAndForeignKeys(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	var mode string
	if err := d.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := d.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

func TestOpenFailsOnBadPath(t *testing.T) {
	t.Parallel()
	_, err := Open(t.Context(), filepath.Join(t.TempDir(), "missing-dir", "x.db"))
	if err == nil {
		t.Fatal("want error for a database in a missing directory")
	}
}

var testMigrations = fstest.MapFS{
	"0001_init.sql": {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
	"0002_more.sql": {Data: []byte("CREATE TABLE b (id INTEGER PRIMARY KEY); INSERT INTO a(id) VALUES (1);")},
	"README.md":     {Data: []byte("ignored")},
}

func TestMigrateAppliesInOrderOnce(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	n, err := Migrate(t.Context(), d, testMigrations)
	if err != nil || n != 2 {
		t.Fatalf("first Migrate = %d, %v; want 2, nil", n, err)
	}
	n, err = Migrate(t.Context(), d, testMigrations)
	if err != nil || n != 0 {
		t.Fatalf("second Migrate = %d, %v; want 0, nil", n, err)
	}
	if v, err := Version(t.Context(), d); err != nil || v != 2 {
		t.Fatalf("Version = %d, %v", v, err)
	}
	var count int
	if err := d.QueryRowContext(t.Context(), "SELECT count(*) FROM a").Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows in a = %d, %v (migration ran twice?)", count, err)
	}
}

func TestMigrateFailureRollsBack(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	bad := fstest.MapFS{
		"0001_init.sql":   {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
		"0002_broken.sql": {Data: []byte("CREATE TABLE c (id INTEGER); THIS IS NOT SQL;")},
	}
	_, err := Migrate(t.Context(), d, bad)
	if err == nil || !strings.Contains(err.Error(), "0002_broken.sql") {
		t.Fatalf("err = %v, want failure naming 0002_broken.sql", err)
	}
	if v, _ := Version(t.Context(), d); v != 1 {
		t.Fatalf("Version = %d, want 1", v)
	}
	var name string
	err = d.QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE name = 'c'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("table c exists after failed migration (err=%v)", err)
	}
}

func TestMigrateRejectsNewerDatabase(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if _, err := Migrate(t.Context(), d, testMigrations); err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{"0001_init.sql": testMigrations["0001_init.sql"]}
	if _, err := Migrate(t.Context(), d, older); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
}

func TestMigrateRejectsBadSets(t *testing.T) {
	t.Parallel()
	tests := map[string]fstest.MapFS{
		"bad name":  {"init.sql": {Data: []byte("SELECT 1;")}},
		"gap":       {"0001_a.sql": {Data: []byte("SELECT 1;")}, "0003_c.sql": {Data: []byte("SELECT 1;")}},
		"duplicate": {"0001_a.sql": {Data: []byte("SELECT 1;")}, "0001_b.sql": {Data: []byte("SELECT 1;")}},
	}
	for name, set := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Migrate(t.Context(), openTemp(t), set); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestEmbeddedMigrationsApply(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if _, err := Migrate(t.Context(), d, Migrations()); err != nil {
		t.Fatalf("embedded migrations: %v", err)
	}
	if _, err := d.ExecContext(context.WithoutCancel(t.Context()), "INSERT INTO meta(key, value) VALUES ('k', 'v')"); err != nil {
		t.Fatalf("meta table missing: %v", err)
	}
}

func TestOpenPathWithURLCharacters(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a?b#c%d e")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x.db")
	d, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.ExecContext(t.Context(), "CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the requested path: %v", err)
	}
}

func TestNewerDatabaseIsNotWritten(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if _, err := Migrate(t.Context(), d, testMigrations); err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{"0001_init.sql": testMigrations["0001_init.sql"]}
	if _, err := Migrate(t.Context(), d, older); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("err = %v", err)
	}
	if v, _ := Version(t.Context(), d); v != 2 {
		t.Fatalf("version changed to %d", v)
	}
}

func TestMigrateRejectsMismatchedHistory(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if _, err := Migrate(t.Context(), d, testMigrations); err != nil {
		t.Fatal(err)
	}
	renamed := fstest.MapFS{
		"0001_init.sql":  testMigrations["0001_init.sql"],
		"0002_other.sql": testMigrations["0002_more.sql"],
	}
	if _, err := Migrate(t.Context(), d, renamed); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
	}
}

func TestVersionOnFreshDatabaseDoesNotWrite(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	if v, err := Version(t.Context(), d); err != nil || v != 0 {
		t.Fatalf("Version = %d, %v", v, err)
	}
	var n int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master").Scan(&n)
	if n != 0 {
		t.Fatalf("Version created %d schema objects", n)
	}
}
