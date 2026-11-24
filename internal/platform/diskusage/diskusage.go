// Package diskusage measures the filesystem holding a directory (KIN-20 storage metrics): total, available to
// an unprivileged writer, free including reserved blocks, and the mount point used as the volume id.
package diskusage

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Usage is one filesystem's size in bytes.
type Usage struct {
	MountPoint     string
	TotalBytes     int64
	AvailableBytes int64
	FreeBytes      int64
}

// Stat measures the filesystem holding path.
func Stat(path string) (Usage, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Usage{}, fmt.Errorf("measuring %s: %w", path, err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(abs, &st); err != nil {
		return Usage{}, fmt.Errorf("measuring %s: %w", abs, err)
	}
	u := usage(&st)
	if u.MountPoint, err = mountPoint(abs); err != nil {
		return Usage{}, fmt.Errorf("measuring %s: %w", abs, err)
	}
	return u, nil
}

// mountPoint walks up from dir while the device stays the same.
func mountPoint(dir string) (string, error) {
	dev := func(p string) (uint64, error) {
		fi, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, fmt.Errorf("no device for %s", p)
		}
		return uint64(st.Dev), nil //nolint:gosec,unconvert // G115: int32 on darwin, a device number is never negative
	}
	d, err := dev(dir)
	if err != nil {
		return "", err
	}
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir, nil
		}
		pd, err := dev(parent)
		if err != nil || pd != d {
			return dir, nil //nolint:nilerr // an unreadable parent is above this filesystem for our purpose
		}
		dir = parent
	}
}

func clamp(blocks uint64, size int64) int64 {
	if size <= 0 {
		return 0
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if blocks > uint64(maxInt64/size) { //nolint:gosec // G115: size > 0
		return maxInt64
	}
	return int64(blocks) * size //nolint:gosec // G115: bounded above
}
