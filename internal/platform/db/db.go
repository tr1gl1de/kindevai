// Package db opens the Control Plane SQLite database and applies its versioned schema migrations.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver: static binary, no CGO toolchain on the VPS or in Docker
)

//go:embed migrations/*.sql
var embedded embed.FS

// Migrations returns the schema migrations compiled into the binary.
func Migrations() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // unreachable: the directory is embedded at compile time
	}
	return sub
}

// ErrSchemaMismatch means the applied migration history differs from the migrations in this binary.
var ErrSchemaMismatch = errors.New("database migration history does not match this ai-platform binary")

// ErrSchemaTooNew means the database was migrated by a newer binary; running an older one could corrupt it.
var ErrSchemaTooNew = errors.New("database schema is newer than this ai-platform binary")

// Open opens (creating if needed) the SQLite database at path with WAL, foreign keys and a busy timeout.
// The pool has one connection: SQLite allows one writer, and the Control Plane is the only process using it.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "foreign_keys(ON)", "busy_timeout(5000)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	// URI form: escape the path so '?', '#' and '%' in a directory name cannot inject parameters.
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	d.SetMaxOpenConns(1)
	var mode string
	if err := d.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	if mode != "wal" {
		_ = d.Close()
		return nil, fmt.Errorf("database %s: journal_mode is %q, want wal", path, mode)
	}
	return d, nil
}

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version int
	name    string
}

func list(migrations fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("listing migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !isSQL(e.Name()) {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %q: name must look like 0001_description.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		ms = append(ms, migration{version: v, name: e.Name()})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: expected version %04d (versions must be 1..n without gaps or duplicates)", m.name, i+1)
		}
	}
	return ms, nil
}

func isSQL(name string) bool { return len(name) > 4 && name[len(name)-4:] == ".sql" }

// Version returns the highest applied migration version (0 for a fresh database). It never writes.
func Version(ctx context.Context, d *sql.DB) (int, error) {
	hist, err := history(ctx, d)
	if err != nil {
		return 0, err
	}
	return len(hist), nil
}

// history returns applied migrations ordered by version, without creating anything.
func history(ctx context.Context, d *sql.DB) ([]migration, error) {
	var n int
	if err := d.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&n); err != nil {
		return nil, fmt.Errorf("reading schema version: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := d.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("reading schema version: %w", err)
	}
	defer rows.Close()
	var hist []migration
	for rows.Next() {
		var m migration
		if err := rows.Scan(&m.version, &m.name); err != nil {
			return nil, fmt.Errorf("reading schema version: %w", err)
		}
		hist = append(hist, m)
	}
	return hist, rows.Err()
}

// Check compares the applied history with the binary's migrations without writing and returns how many are
// pending (ErrSchemaTooNew, ErrSchemaMismatch otherwise). Import uses it before adopting a database (KIN-41).
func Check(ctx context.Context, d *sql.DB, migrations fs.FS) (int, error) {
	ms, hist, err := compare(ctx, d, migrations)
	if err != nil {
		return 0, err
	}
	return len(ms) - len(hist), nil
}

func compare(ctx context.Context, d *sql.DB, migrations fs.FS) (ms, hist []migration, err error) {
	ms, err = list(migrations)
	if err != nil {
		return nil, nil, err
	}
	hist, err = history(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	if len(hist) > len(ms) {
		return nil, nil, fmt.Errorf("database has %d migrations, binary knows %d: %w", len(hist), len(ms), ErrSchemaTooNew)
	}
	for i, h := range hist {
		if h != ms[i] {
			return nil, nil, fmt.Errorf("applied %04d %s, binary has %04d %s: %w", h.version, h.name, ms[i].version, ms[i].name,
				ErrSchemaMismatch)
		}
	}
	return ms, hist, nil
}

// Migrate checks the applied history against the binary's migrations, then applies pending ones, each in
// its own transaction, and returns how many ran.
func Migrate(ctx context.Context, d *sql.DB, migrations fs.FS) (int, error) {
	ms, hist, err := compare(ctx, d, migrations)
	if err != nil {
		return 0, err
	}
	if len(hist) == len(ms) {
		return 0, nil
	}
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`); err != nil {
		return 0, fmt.Errorf("preparing schema_migrations: %w", err)
	}
	applied := 0
	for _, m := range ms[len(hist):] {
		body, err := fs.ReadFile(migrations, m.name)
		if err != nil {
			return applied, fmt.Errorf("reading migration %s: %w", m.name, err)
		}
		if err := apply(ctx, d, m, string(body)); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// ForeignKeysOff, as the first line of a migration, runs it with foreign key enforcement off (SQLite's 12-step
// ALTER TABLE procedure: rebuilding a parent table must not cascade deletes into its children). The migration is
// still one transaction; PRAGMA foreign_key_check must find no violation before it commits, and enforcement is
// switched back on afterwards.
const ForeignKeysOff = "-- migrate: foreign-keys-off\n"

// foreignKeyCheck fails when PRAGMA foreign_key_check finds a violation.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key violations after the migration")
	}
	return rows.Err()
}

func apply(ctx context.Context, d *sql.DB, m migration, body string) (err error) {
	conn, err := d.Conn(ctx) // one connection: the pragma is per connection and must not change mid-transaction
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer conn.Close()
	fkOff := strings.HasPrefix(body, ForeignKeysOff)
	if fkOff {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		defer func() {
			if _, ferr := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON"); ferr != nil {
				err = errors.Join(err, fmt.Errorf("migration %s: enabling foreign keys again: %w", m.name, ferr))
				_ = conn.Raw(func(any) error { return driver.ErrBadConn }) // never reuse a connection without them
			}
		}()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if fkOff {
		if err := foreignKeyCheck(ctx, tx); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
		m.version, m.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("migration %s: recording version: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.name, err)
	}
	return nil
}
