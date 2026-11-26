package diskusage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/diskusage"
)

func TestStatReportsConsistentMetricsAndMountPoint(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	u, err := diskusage.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.TotalBytes <= 0 || u.AvailableBytes < 0 || u.AvailableBytes > u.TotalBytes || u.FreeBytes < u.AvailableBytes ||
		u.FreeBytes > u.TotalBytes {
		t.Fatalf("usage = %+v", u)
	}
	if !filepath.IsAbs(u.MountPoint) {
		t.Fatalf("mount point %q", u.MountPoint)
	}
	if rel, err := filepath.Rel(u.MountPoint, dir); err != nil || filepath.IsAbs(rel) || rel == ".." || len(rel) >= 2 && rel[:2] == ".." {
		t.Fatalf("mount point %q does not contain %q", u.MountPoint, dir)
	}
	if _, err := diskusage.Stat(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing path measured")
	}
}
