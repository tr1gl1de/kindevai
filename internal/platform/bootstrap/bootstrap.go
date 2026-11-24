// Package bootstrap brings a Control Plane installation up: data directory, encryption key, SQLite and
// schema. On a clean host it creates everything; on restart it reuses what exists and fails loudly
// instead of papering over a missing or wrong key.
package bootstrap

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

// ErrWrongKey means the key file does not match the key the database was created with.
var ErrWrongKey = errors.New("encryption key does not match this database")

// Options configures Run.
type Options struct {
	DataDir string
	KeyFile string // optional; defaults to KeyPath(DataDir). Lets operators keep the key off the data volume.
	Now     func() time.Time
}

// Installation is a running installation's storage. Close releases the database and the data dir lock.
type Installation struct {
	Dir   *datadir.Dir
	DB    *sql.DB
	Box   *secrets.Box
	ID    string
	Fresh bool // true when this run created the database
}

// DBPath is the SQLite location inside a data directory.
func DBPath(dataDir string) string { return filepath.Join(dataDir, "db", "platform.db") }

// KeyPath is the default key location inside a data directory, kept apart from the database files.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, "secrets", "master.key") }

const (
	metaInstallationID = "installation_id"
	metaCreatedAt      = "created_at"
	metaKeyCheck       = "key_check"
	keyCheckPlaintext  = "kindevai key check v1"
)

// Run opens or creates the installation in opts.DataDir.
func Run(ctx context.Context, opts Options) (inst *Installation, err error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	dir, err := datadir.Open(opts.DataDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = dir.Close()
		}
	}()
	keyFile := opts.KeyFile
	if keyFile == "" {
		keyFile = KeyPath(dir.Path())
	}
	dbFile := DBPath(dir.Path())

	if err := os.MkdirAll(filepath.Dir(dbFile), 0o700); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}
	sqldb, err := db.Open(ctx, dbFile)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = sqldb.Close()
		}
	}()
	// An installation is established once its key check is stored: from then on secrets may be encrypted
	// with the key, so it must never change. Before that (clean host or interrupted first run) nothing has
	// been encrypted and any key — existing or new — can be adopted.
	established, err := hasKeyCheck(ctx, sqldb)
	if err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(keyFile, established)
	if err != nil {
		return nil, err
	}
	box, err := secrets.NewBox(key)
	if err != nil {
		return nil, err
	}
	if _, err := db.Migrate(ctx, sqldb, db.Migrations()); err != nil {
		return nil, fmt.Errorf("migrating database: %w", err)
	}
	id, err := initMeta(ctx, sqldb, box, opts.Now())
	if err != nil {
		return nil, err
	}
	fresh := !established
	return &Installation{Dir: dir, DB: sqldb, Box: box, ID: id, Fresh: fresh}, nil
}

// Close closes the database and releases the data directory.
func (i *Installation) Close() error {
	return errors.Join(i.DB.Close(), i.Dir.Close())
}

// hasKeyCheck reports whether the database already records a key check. It only reads.
func hasKeyCheck(ctx context.Context, d *sql.DB) (bool, error) {
	var n int
	if err := d.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'").Scan(&n); err != nil {
		return false, fmt.Errorf("inspecting database: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := d.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key = ?", metaKeyCheck).Scan(&n); err != nil {
		return false, fmt.Errorf("inspecting database: %w", err)
	}
	return n > 0, nil
}

// loadOrCreateKey creates a key only for an installation that has not been established yet.
func loadOrCreateKey(path string, established bool) (secrets.Key, error) {
	k, err := secrets.LoadKey(path)
	switch {
	case err == nil:
		return k, nil
	case errors.Is(err, secrets.ErrKeyMissing) && established:
		return secrets.Key{}, fmt.Errorf("the database exists but %w; restore the key file from the original "+
			"installation (it is never regenerated, or stored secrets would become unreadable)", err)
	case errors.Is(err, secrets.ErrKeyMissing):
		k, err := secrets.CreateKey(path)
		if errors.Is(err, fs.ErrExist) { // created concurrently (shared --key-file): use that one
			return secrets.LoadKey(path)
		}
		if err != nil {
			return secrets.Key{}, fmt.Errorf("creating encryption key: %w", err)
		}
		return k, nil
	default:
		return secrets.Key{}, err
	}
}

// initMeta records installation metadata on first run and verifies the key against the stored key check.
// Missing rows (for example after an interrupted bootstrap) are filled in, so the operation is repeatable.
func initMeta(ctx context.Context, d *sql.DB, box *secrets.Box, now time.Time) (string, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("initialising metadata: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var check string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaKeyCheck).Scan(&check); {
	case errors.Is(err, sql.ErrNoRows):
		ct, err := box.Seal([]byte(keyCheckPlaintext), metaKeyCheck)
		if err != nil {
			return "", err
		}
		if err := setMeta(ctx, tx, metaKeyCheck, base64.StdEncoding.EncodeToString(ct)); err != nil {
			return "", err
		}
	case err != nil:
		return "", fmt.Errorf("reading key check: %w", err)
	default:
		ct, err := base64.StdEncoding.DecodeString(check)
		if err != nil {
			return "", fmt.Errorf("key check is corrupted: %w", err)
		}
		if pt, err := box.Open(ct, metaKeyCheck); err != nil || string(pt) != keyCheckPlaintext {
			return "", ErrWrongKey
		}
	}

	var id string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaInstallationID).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generating installation id: %w", err)
		}
		id = hex.EncodeToString(b[:])
		if err := setMeta(ctx, tx, metaInstallationID, id); err != nil {
			return "", err
		}
		if err := setMeta(ctx, tx, metaCreatedAt, now.UTC().Format(time.RFC3339)); err != nil {
			return "", err
		}
	case err != nil:
		return "", fmt.Errorf("reading installation id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("initialising metadata: %w", err)
	}
	return id, nil
}

func setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES (?, ?)", key, value); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}
