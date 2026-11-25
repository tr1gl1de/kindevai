//go:build linux

package nsbox

import (
	"errors"
	"fmt"
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

// devNodes are the host devices the sandbox's /dev holds.
var devNodes = []string{"null", "zero", "full", "random", "urandom", "tty"}

// view is the sandbox's filesystem view once built: the preflight's probes.
type view struct {
	home, data []string // host paths that must not exist in the view
	readonly   []string // paths a write below must fail
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
}

// readHomeFiles reads the CLI's login files (domain.CLIHomeFiles) from home; a file that resolves outside home
// (a symlink elsewhere) or into the runner's data directory, or is not a regular file, is skipped.
func readHomeFiles(home string, dataDirs []string) []homeFile {
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil
	}
	var out []homeFile
	for _, rel := range domain.CLIHomeFiles {
		r, err := filepath.EvalSymlinks(filepath.Join(home, rel))
		if err != nil || !within(r, realHome) || withinAny(r, dataDirs) {
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
		out = append(out, homeFile{rel: rel, data: b, mode: fi.Mode().Perm() | 0o600})
	}
	return out
}

// both is p and, when it differs, the path it resolves to.
func both(p string) []string {
	if p == "" {
		return nil
	}
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}
	return out
}

// buildView builds the sandbox's filesystem view and pivots into it (see fsview.go). It runs in the helper's
// fresh mount namespace, before the agent exists.
func buildView(s Spec) (*view, error) {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return nil, fmt.Errorf("private mounts: %w", err)
	}
	for _, k := range s.Keep {
		if !filepath.IsAbs(k) || filepath.Clean(k) != k || k == "/" {
			return nil, fmt.Errorf("kept directory %q is not a clean absolute path", k)
		}
	}
	system := visibleSystem()
	home := envValue(s.Env, "HOME")
	if home != "" && (!filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" ||
		withinAny(home, system) || withinAny(home, []string{"/etc", "/dev", "/proc"}) || withinAny(home, s.Keep)) {
		return nil, errors.New("the agent's HOME is not a separate absolute directory")
	}
	// The runner's HOME and data directory as given and as resolved: a symlink must not let a hidden directory
	// show up under its real path (inside a visible one) while the probes look at the link.
	homes, datas := both(s.Home), both(s.DataDir)
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
	var mirrors []string // directories holding read-only entries only: a read-only tmpfs in the view
	units := append([]string{}, s.Keep...)
	for _, k := range s.Keep {
		if err := add(k, false); err != nil {
			return nil, err
		}
	}
	readonly := []string{"/", "/etc"}
	for _, k := range s.Keep {
		mirror, admin, ok := "", "", false
		for _, wk := range both(k) { // git may have recorded the worktree's path as given or as resolved
			for _, d := range datas {
				if mirror, admin, ok = gitDirs(wk, d); ok {
					break
				}
			}
			if ok {
				break
			}
		}
		if !ok {
			continue
		}
		// Of the shared mirror only what git needs, read-only (an agent can neither plant hooks nor rewrite other
		// branches); this worktree's index, HEAD and logs writable.
		for _, e := range mirrorEntries {
			p := filepath.Join(mirror, e)
			if _, err := os.Lstat(p); err != nil {
				continue
			}
			if err := add(p, true); err != nil {
				return nil, err
			}
			units = append(units, p)
		}
		if err := add(admin, false); err != nil {
			return nil, err
		}
		units = append(units, admin)
		mirrors = append(mirrors, mirror, filepath.Dir(admin))
		readonly = append(readonly, mirror, filepath.Dir(admin), filepath.Join(mirror, "objects"), filepath.Join(mirror, "refs"))
	}
	var exps []exposure
	if len(s.Argv) > 0 {
		var err error
		hidden := append(append([]string{home}, homes...), datas...)
		visible := append([]string{}, units...)
		for _, k := range s.Keep {
			visible = append(visible, both(k)...)
		}
		if exps, err = expose(s.Argv[0], envValue(s.Env, "PATH"), s.DataDir, hidden, visible); err != nil {
			return nil, err
		}
	}
	var points []string
	for _, e := range exps {
		if err := add(e.unit, true); err != nil {
			return nil, err
		}
		units = append(units, e.unit)
		readonly = append(readonly, e.unit)
		if e.link != "" {
			points = append(points, e.link)
		}
	}
	var files []homeFile
	if home != "" {
		points = append(points, home)
		files = readHomeFiles(home, datas)
		for _, f := range files {
			points = append(points, filepath.Join(home, f.rel))
		}
	}
	v := &view{}
	all := append(append([]string{}, units...), system...)
	for _, h := range homes {
		v.home = append(v.home, rootProbes(h, all, points)...)
	}
	for _, d := range datas {
		v.data = append(v.data, rootProbes(d, all, points)...)
	}
	if err := assemble(home, srcs, files, exps, mirrors); err != nil {
		return nil, err
	}
	for _, d := range systemDirs {
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
			v.readonly = append(v.readonly, d)
		}
	}
	v.readonly = append(v.readonly, readonly...)
	return v, nil
}

// assemble mounts the view on newRoot and pivots into it.
func assemble(home string, srcs []source, files []homeFile, exps []exposure, roDirs []string) error {
	r := newRoot
	if err := unix.Mount("tmpfs", r, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=16m"); err != nil {
		return fmt.Errorf("the sandbox root: %w", err)
	}
	for _, d := range systemDirs {
		if err := mirrorHost(r, d); err != nil {
			return err
		}
	}
	if err := etc(r); err != nil {
		return err
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
	for _, d := range roDirs {
		if err := mkdirIn(r, d); err != nil {
			return err
		}
		if err := unix.Mount("tmpfs", r+d, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=64k"); err != nil {
			return fmt.Errorf("placing %s: %w", d, err)
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
	// The root, /etc, /dev and the mirror's directories themselves read-only: the agent writes only to the worktree, its state directory,
	// /tmp and its private HOME.
	ro := []string{r + "/etc", r + "/dev", r}
	for i := len(roDirs) - 1; i >= 0; i-- {
		ro = append([]string{r + roDirs[i]}, ro...)
	}
	for _, d := range ro {
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

// mirrorHost places the host path p (absolute) read-only in the view below r: a symlink is recreated (/bin ->
// usr/bin), a directory or file bound; a missing one is skipped.
func mirrorHost(r, p string) error {
	fi, err := os.Lstat(p)
	switch {
	case err != nil:
		return nil
	case fi.Mode()&os.ModeSymlink != 0:
		t, err := os.Readlink(p)
		if err != nil {
			return err
		}
		if err := mkdirIn(r, filepath.Dir(p)); err != nil {
			return err
		}
		if err := os.Symlink(t, r+p); err != nil {
			return fmt.Errorf("the sandbox root: %w", err)
		}
		return nil
	case fi.IsDir():
		if err := mkdirIn(r, p); err != nil {
			return err
		}
	case fi.Mode().IsRegular():
		if err := mkdirIn(r, filepath.Dir(p)); err != nil {
			return err
		}
		if err := os.WriteFile(r+p, nil, 0o600); err != nil {
			return fmt.Errorf("the sandbox root: %w", err)
		}
	default:
		return nil
	}
	return bind(p, r+p, true)
}

// etc builds the sandbox's /etc from etcEntries: a tmpfs, so nothing else of the host's /etc shows.
func etc(r string) error {
	if err := os.Mkdir(r+"/etc", 0o750); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", r+"/etc", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("the sandbox's /etc: %w", err)
	}
	for _, e := range etcEntries {
		if err := mirrorHost(r, "/etc/"+e); err != nil {
			return err
		}
	}
	return nil
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
	// Every mount below dst too (Linux 5.12+; an older kernel fails here and strict egress is unsupported).
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV}
	if err := unix.MountSetattr(-1, dst, unix.AT_RECURSIVE, attr); err != nil {
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
