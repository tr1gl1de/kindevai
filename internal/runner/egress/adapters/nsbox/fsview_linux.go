//go:build linux

package nsbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// newRoot is where the view is assembled before the helper pivots into it: a fresh tmpfs over /tmp of the
// helper's own mount namespace (every host source is opened before it covers anything).
const newRoot = "/tmp"

// maxHomeFile bounds a CLI file copied into the agent's HOME (Claude Code's ~/.claude.json grows with use).
const maxHomeFile = 16 << 20

// maxProbes bounds the paths each filesystem leak test checks.
const maxProbes = 512

// devNodes are the host devices the sandbox's /dev holds.
var devNodes = []string{"null", "zero", "full", "random", "urandom", "tty"}

// view is the sandbox's filesystem view once built: the preflight's probes and the CLI logins to write back.
type view struct {
	home, data []string // host paths that must not exist in the view
	readonly   []string // directories a write to must fail
	creds      []cred
}

// cred is a CLI credential file copied into the agent's HOME.
type cred struct {
	inView string // its path in the view
	dirfd  int    // the host directory holding it (O_PATH)
	name   string
	orig   []byte
}

// source is a host path to bind into the view, opened before the view covers anything.
type source struct {
	path string
	fd   int
	dir  bool
	ro   bool
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // the last one wins, as for exec
		}
	}
	return v
}

func openSource(p string, ro bool) (source, error) {
	fd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return source{}, fmt.Errorf("opening %s: %w", p, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return source{}, fmt.Errorf("opening %s: %w", p, err)
	}
	return source{path: p, fd: fd, dir: st.Mode&unix.S_IFMT == unix.S_IFDIR, ro: ro}, nil
}

// homeFile is a CLI file read from the CLI's HOME on the host.
type homeFile struct {
	rel  string
	data []byte
	mode os.FileMode
	cred bool
}

// readHomeFiles reads the CLI's login files (domain.CLIHomeFiles) from home; a file that resolves into the
// runner's data directory or is not a regular file is skipped.
func readHomeFiles(home, dataDir string) []homeFile {
	var out []homeFile
	for _, f := range domain.CLIHomeFiles {
		p := filepath.Join(home, f.Path)
		r, err := filepath.EvalSymlinks(p)
		if err != nil || dataDir != "" && within(r, dataDir) {
			continue
		}
		fi, err := os.Stat(r)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxHomeFile {
			continue
		}
		b, err := os.ReadFile(r) //nolint:gosec // G304: the CLI's own login file
		if err != nil {
			continue
		}
		out = append(out, homeFile{rel: f.Path, data: b, mode: fi.Mode().Perm() | 0o600, cred: f.Credential})
	}
	return out
}

// buildView builds the sandbox's filesystem view and pivots into it (see fsview.go). It runs in the helper's
// fresh mount namespace, before the agent exists.
func buildView(s Spec) (*view, error) {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return nil, fmt.Errorf("private mounts: %w", err)
	}
	home := envValue(s.Env, "HOME")
	if home != "" && (!filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" ||
		withinAny(home, systemDirs) || withinAny(home, s.Keep)) {
		return nil, errors.New("the agent's HOME is not a separate absolute directory")
	}
	var srcs []source
	defer func() {
		for _, src := range srcs {
			_ = unix.Close(src.fd)
		}
	}()
	add := func(p string, ro bool) error {
		src, err := openSource(p, ro)
		if err != nil {
			return err
		}
		srcs = append(srcs, src)
		return nil
	}
	units := append([]string{}, s.Keep...)
	for _, k := range s.Keep {
		if err := add(k, false); err != nil {
			return nil, err
		}
	}
	var readonly []string
	for _, k := range s.Keep {
		if mirror, admin, ok := gitDirs(k, s.DataDir); ok {
			// The shared mirror read-only (an agent cannot plant hooks or rewrite other branches); this worktree's
			// index, HEAD and logs writable.
			if err := add(mirror, true); err != nil {
				return nil, err
			}
			if err := add(admin, false); err != nil {
				return nil, err
			}
			units = append(units, mirror)
			readonly = append(readonly, filepath.Join(mirror, "objects"))
		}
	}
	var exps []exposure
	if len(s.Argv) > 0 {
		var err error
		if exps, err = expose(s.Argv[0], envValue(s.Env, "PATH"), s.DataDir, []string{s.Home, home}, units); err != nil {
			return nil, err
		}
	}
	var points []string
	for _, e := range exps {
		if err := add(e.unit, true); err != nil {
			return nil, err
		}
		units = append(units, e.unit)
		if e.link != "" {
			points = append(points, e.link)
		}
	}
	var files []homeFile
	var creds []cred
	if home != "" {
		points = append(points, home)
		files = readHomeFiles(home, s.DataDir)
		for _, f := range files {
			points = append(points, filepath.Join(home, f.rel))
			if !f.cred {
				continue
			}
			dir := filepath.Join(home, filepath.Dir(f.rel))
			fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			creds = append(creds, cred{inView: filepath.Join(home, f.rel), dirfd: fd, name: filepath.Base(f.rel), orig: f.data})
		}
	}
	v := &view{creds: creds}
	all := append(append([]string{}, units...), systemDirs...)
	v.home = rootProbes(s.Home, all, points)
	v.data = rootProbes(s.DataDir, all, points)
	if err := assemble(home, srcs, files, exps); err != nil {
		v.close()
		return nil, err
	}
	v.readonly = append([]string{"/"}, readonly...)
	for _, d := range systemDirs {
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
			v.readonly = append(v.readonly, d)
		}
	}
	return v, nil
}

// rootProbes are the probes of a hidden root: the root itself when nothing visible lies inside it, its other
// entries when something does, and the root (a certain leak) when it lies inside something visible.
func rootProbes(root string, units, points []string) []string {
	switch {
	case root == "":
		return nil
	case withinAny(root, units):
		return []string{root}
	case ancestor(root, units) || ancestor(root, points) || contains(points, root):
		return probes(root, units, points, maxProbes)
	}
	return []string{root}
}

// assemble mounts the view on newRoot and pivots into it.
func assemble(home string, srcs []source, files []homeFile, exps []exposure) error {
	r := newRoot
	if err := unix.Mount("tmpfs", r, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=16m"); err != nil {
		return fmt.Errorf("the sandbox root: %w", err)
	}
	for _, d := range systemDirs {
		fi, err := os.Lstat(d)
		switch {
		case err != nil:
			continue
		case fi.Mode()&os.ModeSymlink != 0: // /bin -> usr/bin
			t, err := os.Readlink(d)
			if err != nil {
				return err
			}
			if err := os.Symlink(t, r+d); err != nil {
				return fmt.Errorf("the sandbox root: %w", err)
			}
		case fi.IsDir():
			if err := os.Mkdir(r+d, 0o750); err != nil {
				return fmt.Errorf("the sandbox root: %w", err)
			}
			if err := bind(d, r+d, true); err != nil {
				return err
			}
		}
	}
	if err := devices(r); err != nil {
		return err
	}
	if err := os.Mkdir(r+"/proc", 0o550); err != nil {
		return err
	}
	if err := unix.Mount("proc", r+"/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mounting /proc: %w", err)
	}
	for _, d := range []string{"/tmp", "/var", "/run"} {
		if err := os.Mkdir(r+d, 0o750); err != nil {
			return err
		}
	}
	if err := unix.Mount("tmpfs", r+"/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=512m"); err != nil {
		return fmt.Errorf("the sandbox's /tmp: %w", err)
	}
	if err := os.Symlink("../tmp", r+"/var/tmp"); err != nil {
		return err
	}
	if home != "" {
		if err := privateHome(r, home, files); err != nil {
			return err
		}
	}
	for _, src := range srcs {
		if err := place(r, src); err != nil {
			return err
		}
	}
	for _, e := range exps {
		if e.link == "" {
			continue
		}
		if err := mkdirIn(r, filepath.Dir(e.link)); err != nil {
			return err
		}
		if err := os.Symlink(e.target, r+e.link); err != nil {
			return fmt.Errorf("linking %s: %w", filepath.Base(e.link), err)
		}
	}
	// The root and /dev themselves read-only: the agent writes only to the worktree, its state directory, /tmp
	// and its private HOME.
	for _, d := range []string{r + "/dev", r} {
		if err := unix.Mount("", d, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return fmt.Errorf("the sandbox root read-only: %w", err)
		}
	}
	if err := unix.Chdir(r); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// The host's root is stacked under the new one: detach it, nothing of it stays reachable.
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detaching the host root: %w", err)
	}
	return unix.Chdir("/")
}

// devices builds /dev: a few host device nodes, the usual links, a private /dev/shm.
func devices(r string) error {
	d := r + "/dev"
	if err := os.Mkdir(d, 0o750); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", d, "tmpfs", unix.MS_NOSUID, "mode=0755,size=64k"); err != nil {
		return fmt.Errorf("the sandbox's /dev: %w", err)
	}
	for _, n := range devNodes {
		if _, err := os.Stat("/dev/" + n); err != nil {
			continue
		}
		if err := os.WriteFile(d+"/"+n, nil, 0o600); err != nil {
			return err
		}
		if err := unix.Mount("/dev/"+n, d+"/"+n, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding /dev/%s: %w", n, err)
		}
	}
	for name, target := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		if err := os.Symlink(target, d+"/"+name); err != nil {
			return err
		}
	}
	if err := os.Mkdir(d+"/shm", 0o750); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", d+"/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=64m"); err != nil {
		return fmt.Errorf("the sandbox's /dev/shm: %w", err)
	}
	return nil
}

// privateHome mounts an empty tmpfs as the agent's HOME and writes the CLI's login files into it.
func privateHome(r, home string, files []homeFile) error {
	if err := mkdirIn(r, home); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", r+home, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700,size=1g"); err != nil {
		return fmt.Errorf("the agent's HOME: %w", err)
	}
	for _, f := range files {
		p := r + filepath.Join(home, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("the agent's HOME: %w", err)
		}
		if err := os.WriteFile(p, f.data, f.mode); err != nil {
			return fmt.Errorf("the agent's HOME: %w", err)
		}
	}
	return nil
}

// place binds an opened host source at its own path in the view.
func place(r string, src source) error {
	parent := filepath.Dir(src.path)
	if src.dir {
		parent = src.path
	}
	if err := mkdirIn(r, parent); err != nil {
		return err
	}
	if !src.dir {
		f, err := os.OpenFile(r+src.path, os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a mount point in the view
		if err != nil {
			return fmt.Errorf("placing %s: %w", src.path, err)
		}
		_ = f.Close()
	}
	return bind("/proc/self/fd/"+strconv.Itoa(src.fd), r+src.path, src.ro)
}

// bind binds src at dst, with every mount below it; ro makes them all read-only.
func bind(src, dst string, ro bool) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("binding %s: %w", dst, err)
	}
	if !ro {
		return nil
	}
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV}
	err := unix.MountSetattr(-1, dst, unix.AT_RECURSIVE, attr)
	if errors.Is(err, unix.ENOSYS) { // before Linux 5.12: the top mount only, keeping its locked flags
		var st unix.Statfs_t
		if err = unix.Statfs(dst, &st); err == nil {
			flags := uintptr(unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
			flags |= uintptr(st.Flags) & (unix.MS_NOEXEC | unix.MS_NOATIME | unix.MS_NODIRATIME | unix.MS_RELATIME) //nolint:gosec // G115: mount flags
			err = unix.Mount("", dst, "", flags, "")
		}
	}
	if err != nil {
		return fmt.Errorf("making %s read-only: %w", dst, err)
	}
	return nil
}

// mkdirIn creates p (absolute) below the view's root r, never through a symlink (which would lead out of the view
// onto the host).
func mkdirIn(r, p string) error {
	cur := r
	for _, c := range strings.Split(strings.Trim(p, "/"), "/") {
		if c == "" {
			continue
		}
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o750); err != nil {
				return fmt.Errorf("placing %s: %w", p, err)
			}
		case err != nil:
			return fmt.Errorf("placing %s: %w", p, err)
		case !fi.IsDir():
			return fmt.Errorf("placing %s: %s is not a directory", p, strings.TrimPrefix(cur, r))
		}
	}
	return nil
}

// leaks runs the filesystem leak tests in the view: each probe must be absent, each write must fail.
func (v *view) leaks() []domain.Leak {
	absent := func(test string, paths []string) domain.Leak {
		for _, p := range paths {
			if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
				return domain.Leak{Test: test, Target: p}
			}
		}
		return domain.Leak{Test: test, Blocked: true}
	}
	out := []domain.Leak{absent(domain.LeakFSHome, v.home), absent(domain.LeakFSData, v.data)}
	write := domain.Leak{Test: domain.LeakFSWrite, Blocked: true}
	for _, d := range v.readonly {
		p := filepath.Join(d, ".kindev-write-probe")
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the probe
		if err == nil {
			_ = f.Close()
			_ = os.Remove(p)
			write = domain.Leak{Test: domain.LeakFSWrite, Target: d}
			break
		}
	}
	return append(out, write)
}

// writeBack returns the CLI logins the agent's CLI refreshed to the host (domain.CredentialUpdate decides).
func (v *view) writeBack() {
	for _, c := range v.creds {
		updated, err := readNoFollow(unix.AT_FDCWD, c.inView, domain.MaxCredentialSize+1)
		if err != nil {
			continue
		}
		current, err := readNoFollow(c.dirfd, c.name, domain.MaxCredentialSize+1)
		if err != nil || !domain.CredentialUpdate(c.orig, current, updated) {
			continue
		}
		tmp := "." + c.name + ".kindev-" + strconv.Itoa(os.Getpid())
		fd, err := unix.Openat(c.dirfd, tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(fd), tmp)
		_, werr := f.Write(updated)
		if serr := f.Sync(); werr == nil {
			werr = serr
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = unix.Renameat(c.dirfd, tmp, c.dirfd, c.name)
		}
		if werr != nil {
			_ = unix.Unlinkat(c.dirfd, tmp, 0)
		}
	}
}

// readNoFollow reads a regular file (never through a final symlink), at most limit bytes.
func readNoFollow(dirfd int, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

func (v *view) close() {
	for _, c := range v.creds {
		_ = unix.Close(c.dirfd)
	}
	v.creds = nil
}
