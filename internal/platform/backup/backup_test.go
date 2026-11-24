package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// source is a running installation with one marker row and one artifact.
func sourceInstall(t *testing.T) (dir string, inst *bootstrap.Installation) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "src")
	inst, err := bootstrap.Run(t.Context(), bootstrap.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	if _, err := inst.DB.ExecContext(t.Context(), "INSERT INTO meta(key, value) VALUES ('marker', 'kept')"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts", "t1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifacts", "t1", "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, inst
}

func export(t *testing.T, dir string) (archive, key string) {
	t.Helper()
	out := t.TempDir()
	archive, key = filepath.Join(out, "kindevai.tar.gz"), filepath.Join(out, "kindevai.key")
	if _, err := Export(t.Context(), ExportOptions{DataDir: dir, Out: archive, KeyOut: key, AppVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	return archive, key
}

// AC: export takes a consistent archive of a running installation (DB snapshot + artifacts) and writes the key
// to a separate 0600 file; import into an empty data dir restores the same installation, which starts at once
// although the snapshot carries the source's live instance record.
func TestExportImportRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	src, inst := sourceInstall(t)
	archive, key := export(t, src) // the source keeps running
	if st, err := os.Stat(key); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v %v", st, err)
	}
	if st, err := os.Stat(archive); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("archive %v %v", st, err)
	}

	dst := filepath.Join(t.TempDir(), "dst")
	m, err := Import(ctx, ImportOptions{Archive: archive, Key: key, DataDir: dst})
	if err != nil {
		t.Fatal(err)
	}
	if m.InstallationID != inst.ID || !m.KeyExported || m.AppVersion != "test" || len(m.Files) != 2 {
		t.Fatalf("manifest %+v", m)
	}
	got, err := bootstrap.Run(ctx, bootstrap.Options{DataDir: dst})
	if err != nil {
		t.Fatalf("imported installation does not start: %v", err)
	}
	defer got.Close()
	var v string
	if err := got.DB.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'marker'").Scan(&v); err != nil || v != "kept" {
		t.Fatalf("marker %q %v", v, err)
	}
	if got.ID != inst.ID || got.Fresh {
		t.Fatalf("id %s fresh %v", got.ID, got.Fresh)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "artifacts", "t1", "report.txt")); err != nil || string(b) != "report" {
		t.Fatalf("artifact %q %v", b, err)
	}
}

// AC: the key never travels silently: the operator chooses --key-out or --without-key; existing files are never
// overwritten.
func TestExportNeedsAnExplicitKeyDecision(t *testing.T) {
	t.Parallel()
	src, _ := sourceInstall(t)
	out := t.TempDir()
	a := filepath.Join(out, "a.tar.gz")
	for _, o := range []ExportOptions{{DataDir: src, Out: a}, {DataDir: src, Out: a, KeyOut: filepath.Join(out, "k"), WithoutKey: true}} {
		if _, err := Export(t.Context(), o); !errors.Is(err, ErrKeyDecision) {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	m, err := Export(t.Context(), ExportOptions{DataDir: src, Out: a, WithoutKey: true})
	if err != nil || m.KeyExported {
		t.Fatalf("without key: %+v %v", m, err)
	}
	if _, err := Export(t.Context(), ExportOptions{DataDir: src, Out: a, WithoutKey: true}); !errors.Is(err, ErrTargetExists) {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := Export(t.Context(), ExportOptions{DataDir: filepath.Join(out, "none"), Out: filepath.Join(out, "b"), WithoutKey: true}); !errors.Is(err, ErrNoInstall) {
		t.Fatalf("no installation: %v", err)
	}
}

func isEmptyDataDir(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		if e.Name() != datadir.LockName {
			t.Fatalf("data dir not left empty: %s", e.Name())
		}
	}
}

// AC: a missing key is a clear error and no key is generated; a wrong key is refused; the data dir stays empty.
func TestImportRequiresTheMatchingKey(t *testing.T) {
	t.Parallel()
	src, _ := sourceInstall(t)
	archive, _ := export(t, src)
	other, _ := sourceInstall(t)
	_, otherKey := export(t, other)
	dst := filepath.Join(t.TempDir(), "dst")
	if _, err := Import(t.Context(), ImportOptions{Archive: archive, DataDir: dst}); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := Import(t.Context(), ImportOptions{Archive: archive, Key: filepath.Join(dst, "missing.key"), DataDir: dst}); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("missing key file: %v", err)
	}
	if _, err := Import(t.Context(), ImportOptions{Archive: archive, Key: otherKey, DataDir: dst}); !errors.Is(err, bootstrap.ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	isEmptyDataDir(t, dst)
	if _, err := os.Stat(bootstrap.KeyPath(dst)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key was created: %v", err)
	}
}

func TestImportRefusesANonEmptyDataDir(t *testing.T) {
	t.Parallel()
	src, _ := sourceInstall(t)
	archive, key := export(t, src)
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "something"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(t.Context(), ImportOptions{Archive: archive, Key: key, DataDir: dst}); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty: %v", err)
	}
}

// AC: schema check: a database migrated by a newer binary is refused before anything is written.
func TestImportRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	src, inst := sourceInstall(t)
	if _, err := inst.DB.ExecContext(t.Context(),
		"INSERT INTO schema_migrations(version, name, applied_at) VALUES (9999, '9999_future.sql', 'x')"); err != nil {
		t.Fatal(err)
	}
	archive, key := export(t, src)
	dst := filepath.Join(t.TempDir(), "dst")
	if _, err := Import(t.Context(), ImportOptions{Archive: archive, Key: key, DataDir: dst}); !errors.Is(err, db.ErrSchemaTooNew) {
		t.Fatalf("newer schema: %v", err)
	}
	isEmptyDataDir(t, dst)
}

// rewrite copies archive, letting edit change the manifest and file contents.
func rewrite(t *testing.T, archive string, edit func(m *Manifest, files map[string][]byte)) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var m Manifest
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == ManifestName {
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			continue
		}
		files[h.Name] = b
	}
	edit(&m, files)
	out := filepath.Join(t.TempDir(), "edited.tar.gz")
	o, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	gw := gzip.NewWriter(o)
	tw := tar.NewWriter(gw)
	mb, _ := json.Marshal(m)
	_ = tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(mb))})
	_, _ = tw.Write(mb)
	for name, b := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b))})
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	_ = gw.Close()
	return out
}

// Verify on import: a damaged file, an unknown format, a path outside the data dir or a missing database is refused.
func TestImportVerifiesTheArchive(t *testing.T) {
	t.Parallel()
	src, _ := sourceInstall(t)
	archive, key := export(t, src)
	cases := map[string]struct {
		edit func(m *Manifest, files map[string][]byte)
		want error
	}{
		"checksum": {func(_ *Manifest, f map[string][]byte) { f["artifacts/t1/report.txt"] = []byte("tampered") }, ErrCorrupt},
		"format":   {func(m *Manifest, _ map[string][]byte) { m.Format = 99 }, ErrFormat},
		"traversal": {func(m *Manifest, f map[string][]byte) {
			f["artifacts/../../evil"] = []byte("x")
			m.Files = append(m.Files, File{Path: "artifacts/../../evil", Size: 1})
		}, ErrCorrupt},
		"no database": {func(m *Manifest, f map[string][]byte) {
			delete(f, DBName)
			m.Files = m.Files[1:]
		}, ErrCorrupt},
		"missing file": {func(_ *Manifest, f map[string][]byte) { delete(f, "artifacts/t1/report.txt") }, ErrCorrupt},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			edited := rewrite(t, archive, tc.edit)
			dst := filepath.Join(t.TempDir(), "dst")
			if _, err := Import(t.Context(), ImportOptions{Archive: edited, Key: key, DataDir: dst}); !errors.Is(err, tc.want) {
				t.Fatalf("%s: %v", name, err)
			}
			isEmptyDataDir(t, dst)
		})
	}
}
