package nsbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The sandbox's filesystem view (KIN-80, KB KIN-A-22). The helper builds a new root on an empty tmpfs and pivots
// into it, so nothing of the host is visible unless placed there:
//
//   - systemDirs, bound read-only (symlinks such as /bin -> usr/bin are recreated), and of /etc only etcEntries
//     (no service configuration of the host: /etc/<service> may hold tokens the runner's uid can read);
//   - its own /dev (null, zero, full, random, urandom, tty), /proc, /tmp, /var/tmp and /dev/shm;
//   - the agent's HOME: a private tmpfs with copies of the CLI's login files (domain.CLIHomeFiles), never copied
//     back;
//   - the attempt's worktree and state directory, read-write;
//   - of the worktree's repository mirror only mirrorEntries, read-only, and the worktree's own administrative
//     directory (index, HEAD) read-write;
//   - the agent's binary (and its script interpreter) when it lives outside the system directories, read-only.
//
// This file holds the host-side path logic (no namespaces needed); fsview_linux.go builds the view.

// systemDirs are bound read-only into every sandbox (those that exist).
var systemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/nix/store"}

// etcEntries are the parts of /etc the sandbox's /etc holds, read-only (those that exist): users, name resolution,
// the dynamic linker, TLS roots, time zone, alternatives.
var etcEntries = []string{"passwd", "group", "nsswitch.conf", "hosts", "host.conf", "gai.conf", "resolv.conf",
	"protocols", "services", "ld.so.cache", "ld.so.conf", "ld.so.conf.d", "ssl", "ca-certificates",
	"ca-certificates.conf", "pki", "localtime", "timezone", "alternatives", "os-release", "lsb-release",
	"debian_version", "mime.types", "gitconfig", "inputrc", "shells", "environment", "locale.alias", "locale.conf"}

// visibleSystem are the host paths the view shows read-only, for the preflight's probes.
func visibleSystem() []string {
	out := append([]string{}, systemDirs...)
	for _, e := range etcEntries {
		out = append(out, "/etc/"+e)
	}
	return out
}

// mirrorEntries are what git in a worktree needs of its mirror (read-only); the other worktrees' administrative
// directories, FETCH_HEAD and the like stay hidden.
var mirrorEntries = []string{"HEAD", "config", "objects", "refs", "packed-refs", "shallow", "info"}

// within reports p == dir or p below dir.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func withinAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if within(p, d) {
			return true
		}
	}
	return false
}

// maxGitFile bounds the worktree's .git file and the gitdir file it names.
const maxGitFile = 4 << 10

func readSmall(p string, limit int64) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := os.Open(p) //nolint:gosec // G304: a path the sandbox owns, checked above
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// gitDirs finds the repository mirror and the administrative directory of a worktree made by `git worktree add`
// (KIN-29). The worktree's .git file may have been rewritten by an earlier attempt's agent, so its claim is only
// trusted when the administrative directory lies in <mirror>/worktrees/<worktree's name>, inside the runner's
// data directory, and points back at this worktree; anything else exposes no repository (ok false).
func gitDirs(workdir, dataDir string) (mirror, admin string, ok bool) {
	if workdir == "" || dataDir == "" {
		return "", "", false
	}
	b, err := readSmall(filepath.Join(workdir, ".git"), maxGitFile)
	if err != nil {
		return "", "", false
	}
	gd, found := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !found || !filepath.IsAbs(gd) || filepath.Clean(gd) != gd {
		return "", "", false
	}
	admin = gd
	mirror = filepath.Dir(filepath.Dir(admin))
	if filepath.Base(filepath.Dir(admin)) != "worktrees" || filepath.Base(admin) != filepath.Base(workdir) ||
		!within(mirror, dataDir) || mirror == dataDir || withinAny(workdir, []string{mirror}) {
		return "", "", false
	}
	// No symlink anywhere: the checked path is the one bound.
	if r, err := filepath.EvalSymlinks(admin); err != nil || r != admin {
		return "", "", false
	}
	back, err := readSmall(filepath.Join(admin, "gitdir"), maxGitFile)
	if err != nil || strings.TrimSpace(string(back)) != filepath.Join(workdir, ".git") {
		return "", "", false
	}
	for _, p := range []string{"HEAD", "objects"} {
		if _, err := os.Lstat(filepath.Join(mirror, p)); err != nil {
			return "", "", false
		}
	}
	return mirror, admin, true
}

// exposure places a program that lives outside the system directories into the sandbox: unit (the file, or the
// npm package holding it) is bound read-only at its own path and link, when set, is a symlink to target.
type exposure struct {
	link, target, unit string
}

// lookPath finds name on a PATH value (absolute entries only).
func lookPath(name, path string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s: not an absolute path", name)
	}
	for _, d := range filepath.SplitList(path) {
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found", name)
}

// packageRoot is the npm package directory holding p (…/node_modules/<name> or …/node_modules/@scope/<name>), or
// p itself.
func packageRoot(p string) string {
	i := strings.LastIndex(p, "/node_modules/")
	if i < 0 {
		return p
	}
	rest := strings.Split(p[i+len("/node_modules/"):], "/")
	n := 1
	if strings.HasPrefix(rest[0], "@") {
		n = 2
	}
	if len(rest) <= n {
		return p
	}
	return p[:i] + "/node_modules/" + strings.Join(rest[:n], "/")
}

// interpreter returns the program a script's #! line runs (`#!/usr/bin/env node` -> node), "" for a binary.
func interpreter(p string) string {
	f, err := os.Open(p) //nolint:gosec // G304: the agent's own program
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 256)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if !bytes.HasPrefix(head, []byte("#!")) {
		return ""
	}
	line, _, _ := bytes.Cut(head[2:], []byte("\n"))
	fs := strings.Fields(string(line))
	if len(fs) == 0 {
		return ""
	}
	if filepath.Base(fs[0]) != "env" {
		return fs[0]
	}
	for _, a := range fs[1:] {
		if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			return a
		}
	}
	return ""
}

// expose returns what makes program (and its #! interpreter, found on path) runnable inside the sandbox. A
// program inside the system directories or a visible directory (the worktree, the state directory) needs nothing. An installation inside the runner's data directory, or one
// whose unit would hold a hidden directory (the data directory, a whole HOME), is refused (the sandbox then does
// not verify).
func expose(program, path string, dataDirs, hidden, visible []string) ([]exposure, error) {
	var out []exposure
	for depth := 0; program != "" && depth < 2; depth++ {
		p, err := lookPath(program, path)
		if err != nil {
			return nil, err
		}
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", filepath.Base(p), err)
		}
		if !withinAny(target, systemDirs) && !withinAny(target, visible) {
			unit := packageRoot(target)
			bad := withinAny(unit, dataDirs)
			for _, h := range append(append([]string{}, dataDirs...), hidden...) {
				bad = bad || h != "" && within(h, unit)
			}
			if bad {
				return nil, fmt.Errorf("%s is installed in a directory the sandbox hides", filepath.Base(p))
			}
			e := exposure{target: target, unit: unit}
			if p != target && !withinAny(p, systemDirs) {
				e.link = p
			}
			out = append(out, e)
		}
		program = interpreter(target)
	}
	return out, nil
}

// probes lists the host paths below root that must not exist in the sandbox: every entry except those inside a
// visible unit (bound with its contents) or equal to a visible point (a copied file, a link); an ancestor of
// something visible is searched further. At most budget paths are listed.
func probes(root string, units, points []string, budget int) []string {
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if len(out) >= budget {
				return
			}
			p := filepath.Join(dir, e.Name())
			if withinAny(p, units) || contains(points, p) {
				continue
			}
			if ancestor(p, units) || ancestor(p, points) {
				if depth < 16 {
					walk(p, depth+1)
				}
				continue
			}
			out = append(out, p)
		}
	}
	walk(root, 0)
	return out
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

// maxProbes bounds the paths each filesystem leak test checks.
const maxProbes = 512

func contains(list []string, p string) bool {
	for _, l := range list {
		if l == p {
			return true
		}
	}
	return false
}

// ancestor reports p strictly above one of paths.
func ancestor(p string, paths []string) bool {
	for _, q := range paths {
		if q != p && within(q, p) {
			return true
		}
	}
	return false
}
