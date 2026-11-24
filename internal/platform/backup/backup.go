// Package backup exports a Control Plane installation into a consistent archive and imports it into an empty data
// directory (KIN-41, manual move; automatic backups are not in the MVP).
//
// The archive is a gzip-compressed tar: manifest.json first (format, binary version, schema version, installation
// id, size and SHA-256 of every file), then db/platform.db — an online, transactionally consistent snapshot taken
// with VACUUM INTO on a read-only connection, so a running Control Plane keeps serving — and the artifacts/ tree.
//
// The encryption key is never inside the archive. Export writes it to a separate file only when the operator asks
// (KeyOut), and the operator must decide explicitly (KeyOut or WithoutKey): data and key are moved separately,
// and whoever holds both can read every stored secret. Import requires the key, checks it against the database's
// key check and never generates a new one (stored secrets would become unreadable).
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/diskusage"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

// Format is the archive format version this binary writes and reads.
const Format = 1

// Archive paths.
const (
	ManifestName = "manifest.json"
	DBName       = "db/platform.db"
	ArtifactsDir = "artifacts"
)

const (
	maxManifestBytes = 1 << 20
	maxFiles         = 100_000
	spaceReserve     = 64 << 20 // headroom for migrations and the WAL after import
)

// Errors.
var (
	ErrKeyDecision  = errors.New("decide how the encryption key travels: --key-out <file> writes it to a separate file, --without-key leaves it out")
	ErrKeyRequired  = errors.New("the archive's encryption key file is required (--key); a new key is never generated, or the stored secrets could not be decrypted")
	ErrNotEmpty     = errors.New("import needs an empty data directory")
	ErrCorrupt      = errors.New("archive is damaged or not an ai-platform export")
	ErrFormat       = errors.New("archive format is not supported by this ai-platform binary")
	ErrNoInstall    = errors.New("no installation in the data directory")
	ErrTargetExists = errors.New("output file already exists")
	ErrNoSpace      = errors.New("not enough free space in the data directory for the archive's contents")
)

// File is one archived file.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes an archive.
type Manifest struct {
	Format         int       `json:"format"`
	AppVersion     string    `json:"appVersion"`
	SchemaVersion  int       `json:"schemaVersion"`
	InstallationID string    `json:"installationId"`
	CreatedAt      time.Time `json:"createdAt"`
	KeyExported    bool      `json:"keyExported"` // the key was written to a separate file next to this export
	Files          []File    `json:"files"`
}

// ExportOptions configures Export.
type ExportOptions struct {
	DataDir    string
	KeyFile    string // the installation's key (default bootstrap.KeyPath(DataDir))
	Out        string // archive path; must not exist
	KeyOut     string // write the key to this separate file (must not exist), or
	WithoutKey bool   // leave the key out (the operator moves it separately)
	AppVersion string
	Now        func() time.Time
}

// Export writes a consistent archive of the installation in DataDir. It works while the Control Plane runs.
func Export(ctx context.Context, o ExportOptions) (m Manifest, err error) {
	if (o.KeyOut == "") == !o.WithoutKey {
		return Manifest{}, ErrKeyDecision
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.KeyFile == "" {
		o.KeyFile = bootstrap.KeyPath(o.DataDir)
	}
	for _, p := range []string{o.Out, o.KeyOut} {
		if p == "" {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			return Manifest{}, fmt.Errorf("%s: %w", p, ErrTargetExists)
		}
	}
	dbFile := bootstrap.DBPath(o.DataDir)
	if _, err := os.Stat(dbFile); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", o.DataDir, ErrNoInstall)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(o.Out), ".ai-platform-export-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("creating a temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	snap := filepath.Join(tmp, "platform.db")
	if err := snapshot(ctx, dbFile, snap); err != nil {
		return Manifest{}, err
	}
	m, err = inspect(ctx, snap, o.KeyFile, !o.WithoutKey)
	if err != nil {
		return Manifest{}, err
	}
	m.Format, m.AppVersion, m.CreatedAt, m.KeyExported = Format, o.AppVersion, o.Now().UTC(), o.KeyOut != ""

	files := []source{{name: DBName, path: snap}}
	art, err := artifacts(filepath.Join(o.DataDir, ArtifactsDir))
	if err != nil {
		return Manifest{}, err
	}
	files = append(files, art...)
	for _, f := range files {
		sum, size, err := hashFile(f.path)
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, File{Path: f.name, Size: size, SHA256: sum})
	}
	if err := writeArchive(o.Out, m, files); err != nil {
		return Manifest{}, err
	}
	if o.KeyOut != "" {
		if err := copyKey(o.KeyFile, o.KeyOut); err != nil {
			_ = os.Remove(o.Out)
			return Manifest{}, err
		}
	}
	return m, nil
}

// snapshot copies the database consistently with VACUUM INTO on a read-only connection.
func snapshot(ctx context.Context, src, dst string) error {
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: src}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("taking a consistent snapshot: %w", err)
	}
	return nil
}

// inspect reads a database file's identity and, if checkKey, verifies the key against it.
func inspect(ctx context.Context, dbFile, keyFile string, checkKey bool) (Manifest, error) {
	d, err := sql.Open("sqlite", "file:"+(&url.URL{Path: dbFile}).EscapedPath()+"?mode=ro")
	if err != nil {
		return Manifest{}, fmt.Errorf("opening snapshot: %w", err)
	}
	defer d.Close()
	var res string
	if err := d.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res); err != nil || res != "ok" {
		return Manifest{}, fmt.Errorf("%w: database integrity check failed (%s)", ErrCorrupt, res)
	}
	v, err := db.Version(ctx, d)
	if err != nil {
		return Manifest{}, err
	}
	id, err := bootstrap.InstallationID(ctx, d)
	if err != nil {
		return Manifest{}, err
	}
	if id == "" {
		return Manifest{}, bootstrap.ErrNotInstallation
	}
	if checkKey {
		if err := verifyKey(ctx, d, keyFile); err != nil {
			return Manifest{}, err
		}
	}
	return Manifest{SchemaVersion: v, InstallationID: id}, nil
}

func verifyKey(ctx context.Context, d *sql.DB, keyFile string) error {
	k, err := secrets.LoadKey(keyFile)
	if err != nil {
		return err
	}
	box, err := secrets.NewBox(k)
	if err != nil {
		return err
	}
	return bootstrap.VerifyKey(ctx, d, box)
}

type source struct{ name, path string }

// artifacts lists the regular files under dir (missing dir: none). Symlinks and special files are skipped.
func artifacts(dir string) ([]source, error) {
	var out []source
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return filepath.SkipAll
		}
		if err != nil {
			return err
		}
		if !e.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, source{name: path.Join(ArtifactsDir, filepath.ToSlash(rel)), path: p})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	return out, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("reading %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func writeArchive(out string, m Manifest, files []source) (err error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", out, err)
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(out)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(mb)), ModTime: m.CreatedAt}); err != nil {
		return err
	}
	if _, err := tw.Write(mb); err != nil {
		return err
	}
	for i, s := range files {
		if err := addFile(tw, s, m.Files[i], m.CreatedAt); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Sync()
}

func addFile(tw *tar.Writer, s source, want File, at time.Time) error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{Name: s.name, Mode: 0o600, Size: want.Size, ModTime: at}); err != nil {
		return err
	}
	// The file may change between hashing and copying (artifacts): copy exactly the hashed size, then compare.
	h := sha256.New()
	if n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, want.Size)); err != nil || n != want.Size {
		return fmt.Errorf("%s changed during export; retry: %w", s.name, errors.Join(err, io.ErrUnexpectedEOF))
	}
	if hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return fmt.Errorf("%s changed during export; retry", s.name)
	}
	return nil
}

// copyKey copies the key file byte for byte to a new 0600 file (never overwriting one).
func copyKey(src, dst string) error {
	if _, err := secrets.LoadKey(src); err != nil { // a valid key, not some other file
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading the key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("creating the key directory: %w", err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("writing the key: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("writing the key: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("writing the key: %w", err)
	}
	return f.Close()
}

// ImportOptions configures Import.
type ImportOptions struct {
	Archive    string // the export archive
	Key        string // the key file exported with it (required)
	DataDir    string // empty or missing
	KeyFile    string // where the installation keeps its key (default bootstrap.KeyPath(DataDir)); must not exist
	AppVersion string
}

// Import restores an archive into an empty data directory: it verifies the manifest, every file's checksum, the
// database's integrity, its schema against this binary (newer or different history: refused before anything is
// written) and the key against the database's key check. Then it places the files and the key, clears the source
// instance's record and applies pending migrations. On any error the data directory is left empty.
func Import(ctx context.Context, o ImportOptions) (m Manifest, err error) {
	if o.Key == "" {
		return Manifest{}, ErrKeyRequired
	}
	if _, err := secrets.LoadKey(o.Key); err != nil {
		if errors.Is(err, secrets.ErrKeyMissing) {
			return Manifest{}, fmt.Errorf("%w: %w", ErrKeyRequired, err)
		}
		return Manifest{}, err
	}
	if o.KeyFile == "" {
		o.KeyFile = bootstrap.KeyPath(o.DataDir)
	}
	if _, err := os.Lstat(o.KeyFile); err == nil {
		return Manifest{}, fmt.Errorf("%s: %w (an installation key already exists)", o.KeyFile, ErrNotEmpty)
	}
	dir, err := datadir.Open(o.DataDir)
	if err != nil {
		return Manifest{}, err
	}
	locked := true
	defer func() {
		if locked {
			_ = dir.Close()
		}
	}()
	if err := checkEmpty(dir.Path()); err != nil {
		return Manifest{}, err
	}
	staging, err := os.MkdirTemp(dir.Path(), ".import-*")
	if err != nil {
		return Manifest{}, fmt.Errorf("creating a staging directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(staging)
		if err != nil { // leave the data directory as empty as it was
			_ = os.RemoveAll(filepath.Join(dir.Path(), "db"))
			_ = os.RemoveAll(filepath.Join(dir.Path(), ArtifactsDir))
		}
	}()
	m, err = extract(o.Archive, staging, func(total int64) error { return checkSpace(dir.Path(), total) })
	if err != nil {
		return Manifest{}, err
	}
	snap := filepath.Join(staging, filepath.FromSlash(DBName))
	got, err := inspect(ctx, snap, o.Key, true)
	if err != nil {
		return Manifest{}, err
	}
	if got.InstallationID != m.InstallationID || got.SchemaVersion != m.SchemaVersion {
		return Manifest{}, fmt.Errorf("%w: manifest does not describe the database", ErrCorrupt)
	}
	if err := prepare(ctx, snap); err != nil {
		return Manifest{}, err
	}
	for _, f := range m.Files {
		dst := filepath.Join(dir.Path(), filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return Manifest{}, err
		}
		if err := os.Rename(filepath.Join(staging, filepath.FromSlash(f.Path)), dst); err != nil {
			return Manifest{}, fmt.Errorf("placing %s: %w", f.Path, err)
		}
	}
	if err := copyKey(o.Key, o.KeyFile); err != nil {
		return Manifest{}, err
	}
	// Release the lock and bring the installation up once: migrations run and the key is checked as on serve. In
	// the short gap another process could take the directory; it then finds a complete installation (files and key
	// are in place), and our Run fails with the lock error instead of corrupting anything.
	locked = false
	if err := dir.Close(); err != nil {
		return Manifest{}, err
	}
	inst, err := bootstrap.Run(ctx, bootstrap.Options{DataDir: o.DataDir, KeyFile: o.KeyFile})
	if err != nil {
		_ = os.Remove(o.KeyFile)
		return Manifest{}, fmt.Errorf("starting the imported installation: %w", err)
	}
	return m, inst.Close()
}

// checkEmpty accepts a data directory holding nothing but the lock file.
func checkEmpty(dir string) error {
	es, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Name() != datadir.LockName {
			return fmt.Errorf("%s contains %s: %w", dir, e.Name(), ErrNotEmpty)
		}
	}
	return nil
}

// prepare checks the schema against this binary without writing, then clears the source instance's record.
func prepare(ctx context.Context, dbFile string) error {
	d, err := db.Open(ctx, dbFile)
	if err != nil {
		return err
	}
	defer d.Close()
	if _, err := db.Check(ctx, d, db.Migrations()); err != nil {
		return err
	}
	return bootstrap.ClearInstanceRecord(ctx, d)
}

// checkSpace refuses an archive whose declared contents do not fit the data directory's free space, before
// anything is extracted (a small, highly compressed archive must not fill the disk).
func checkSpace(dir string, total int64) error {
	u, err := diskusage.Stat(dir)
	if err != nil {
		return fmt.Errorf("checking free space: %w", err)
	}
	if total > u.AvailableBytes-spaceReserve {
		return fmt.Errorf("%w: needs %d bytes, %d available", ErrNoSpace, total+spaceReserve, u.AvailableBytes)
	}
	return nil
}

// extract reads the archive into dir, checking the manifest and every file against it. fits is called with the
// total declared size before any file is written.
func extract(archive, dir string, fits func(total int64) error) (Manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return Manifest{}, fmt.Errorf("opening archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != ManifestName || h.Typeflag != tar.TypeReg || h.Size > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest.json must come first", ErrCorrupt)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, maxManifestBytes)).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %w", ErrCorrupt, err)
	}
	if m.Format != Format {
		return Manifest{}, fmt.Errorf("%w: format %d, this binary reads %d", ErrFormat, m.Format, Format)
	}
	if len(m.Files) > maxFiles {
		return Manifest{}, fmt.Errorf("%w: more than %d files", ErrCorrupt, maxFiles)
	}
	want := map[string]File{}
	var total int64
	for _, fe := range m.Files {
		if !validPath(fe.Path) || fe.Size < 0 || fe.Size > 1<<50 {
			return Manifest{}, fmt.Errorf("%w: invalid path %q", ErrCorrupt, fe.Path)
		}
		if _, dup := want[fe.Path]; dup {
			return Manifest{}, fmt.Errorf("%w: duplicate %q", ErrCorrupt, fe.Path)
		}
		want[fe.Path] = fe
		total += fe.Size
	}
	if err := fits(total); err != nil {
		return Manifest{}, err
	}
	if _, ok := want[DBName]; !ok {
		return Manifest{}, fmt.Errorf("%w: no database", ErrCorrupt)
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		fe, ok := want[h.Name]
		if !ok || h.Typeflag != tar.TypeReg || h.Size != fe.Size {
			return Manifest{}, fmt.Errorf("%w: unexpected entry %q", ErrCorrupt, h.Name)
		}
		if err := writeEntry(tr, filepath.Join(dir, filepath.FromSlash(h.Name)), fe); err != nil {
			return Manifest{}, err
		}
		delete(want, h.Name)
	}
	for p := range want {
		return Manifest{}, fmt.Errorf("%w: %s is missing", ErrCorrupt, p)
	}
	return m, nil
}

// validPath allows only db/platform.db and local paths under artifacts/.
func validPath(p string) bool {
	if p == DBName {
		return true
	}
	rest, ok := strings.CutPrefix(p, ArtifactsDir+"/")
	return ok && rest != "" && path.Clean(p) == p && filepath.IsLocal(filepath.FromSlash(rest)) && !strings.Contains(p, "\\")
}

func writeEntry(r io.Reader, dst string, fe File) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, fe.Size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrCorrupt, fe.Path, err)
	}
	if n != fe.Size || hex.EncodeToString(h.Sum(nil)) != fe.SHA256 {
		return fmt.Errorf("%w: %s checksum mismatch", ErrCorrupt, fe.Path)
	}
	return nil
}
