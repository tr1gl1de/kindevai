package nsbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// gitLayout builds a mirror and one worktree of it like KIN-29's workspace store does.
func gitLayout(t *testing.T) (data, mirror, admin, workdir string) {
	t.Helper()
	data, _ = filepath.EvalSymlinks(t.TempDir())
	mirror = filepath.Join(data, "repos", "o", "n.git")
	workdir = filepath.Join(data, "workspaces", "o", "n", "leaf")
	admin = filepath.Join(mirror, "worktrees", "leaf")
	write(t, filepath.Join(mirror, "HEAD"), "ref: refs/heads/main\n", 0o600)
	if err := os.MkdirAll(filepath.Join(mirror, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(admin, "gitdir"), filepath.Join(workdir, ".git")+"\n", 0o600)
	write(t, filepath.Join(workdir, ".git"), "gitdir: "+admin+"\n", 0o600)
	return data, mirror, admin, workdir
}

func TestGitDirs(t *testing.T) {
	t.Parallel()
	t.Run("a worktree of the mirror", func(t *testing.T) {
		t.Parallel()
		data, mirror, admin, workdir := gitLayout(t)
		m, a, ok := gitDirs(workdir, data)
		if !ok || m != mirror || a != admin {
			t.Fatalf("gitDirs = %q %q %v", m, a, ok)
		}
	})
	t.Run("another project's mirror", func(t *testing.T) {
		t.Parallel()
		data, _, _, workdir := gitLayout(t)
		// An earlier agent rewrote .git to reach another repository whose worktree of the same name points elsewhere.
		other := filepath.Join(data, "repos", "x", "secret.git")
		write(t, filepath.Join(other, "HEAD"), "x", 0o600)
		write(t, filepath.Join(other, "worktrees", "leaf", "gitdir"), filepath.Join(data, "workspaces", "x", "secret", "leaf", ".git"), 0o600)
		write(t, filepath.Join(workdir, ".git"), "gitdir: "+filepath.Join(other, "worktrees", "leaf"), 0o600)
		if _, _, ok := gitDirs(workdir, data); ok {
			t.Fatal("another repository was exposed")
		}
	})
	t.Run("outside the data directory", func(t *testing.T) {
		t.Parallel()
		data, _, _, workdir := gitLayout(t)
		outside := t.TempDir()
		write(t, filepath.Join(outside, "worktrees", "leaf", "gitdir"), filepath.Join(workdir, ".git"), 0o600)
		write(t, filepath.Join(workdir, ".git"), "gitdir: "+filepath.Join(outside, "worktrees", "leaf"), 0o600)
		if _, _, ok := gitDirs(workdir, data); ok {
			t.Fatal("a directory outside the data directory was exposed")
		}
	})
	t.Run("a .git symlink", func(t *testing.T) {
		t.Parallel()
		data, _, _, workdir := gitLayout(t)
		_ = os.Remove(filepath.Join(workdir, ".git"))
		if err := os.Symlink("/etc/hostname", filepath.Join(workdir, ".git")); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := gitDirs(workdir, data); ok {
			t.Fatal("a symlinked .git was followed")
		}
	})
	t.Run("not a worktree", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := gitDirs(t.TempDir(), t.TempDir()); ok {
			t.Fatal("a plain directory has no repository")
		}
	})
}

func TestExpose(t *testing.T) {
	t.Parallel()
	t.Run("a system program needs nothing", func(t *testing.T) {
		t.Parallel()
		sh, err := filepath.EvalSymlinks("/bin/sh")
		if err != nil {
			t.Skip("no /bin/sh")
		}
		got, err := expose(sh, "", "", nil, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("expose = %v, %v", got, err)
		}
	})
	t.Run("a native CLI in HOME", func(t *testing.T) {
		t.Parallel()
		home, _ := filepath.EvalSymlinks(t.TempDir())
		bin := filepath.Join(home, ".local", "share", "claude", "versions", "2.1")
		write(t, bin, "\x7fELF", 0o700)
		link := filepath.Join(home, ".local", "bin", "claude")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bin, link); err != nil {
			t.Fatal(err)
		}
		got, err := expose(link, "", filepath.Join(home, ".local", "share", "kindevai-runner"), []string{home}, nil)
		if err != nil || len(got) != 1 || got[0] != (exposure{link: link, target: bin, unit: bin}) {
			t.Fatalf("expose = %+v, %v", got, err)
		}
	})
	t.Run("an npm CLI run by env node", func(t *testing.T) {
		t.Parallel()
		home, _ := filepath.EvalSymlinks(t.TempDir())
		prefix := filepath.Join(home, ".nvm", "versions", "node", "v24")
		pkg := filepath.Join(prefix, "lib", "node_modules", "@openai", "codex")
		script := filepath.Join(pkg, "bin", "codex.js")
		write(t, script, "#!/usr/bin/env node\nrequire('../vendor')\n", 0o700)
		node := filepath.Join(prefix, "bin", "node")
		write(t, node, "\x7fELF", 0o700)
		codex := filepath.Join(prefix, "bin", "codex")
		if err := os.Symlink("../lib/node_modules/@openai/codex/bin/codex.js", codex); err != nil {
			t.Fatal(err)
		}
		got, err := expose(codex, "/usr/bin:"+filepath.Dir(codex), "", []string{home}, nil)
		want := []exposure{{link: codex, target: script, unit: pkg}, {target: node, unit: node}}
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("expose = %+v, %v; want %+v", got, err, want)
		}
	})
	t.Run("an installation holding the data directory", func(t *testing.T) {
		t.Parallel()
		dir, _ := filepath.EvalSymlinks(t.TempDir())
		pkg := filepath.Join(dir, "node_modules", "cli")
		write(t, filepath.Join(pkg, "cli"), "\x7fELF", 0o700)
		if _, err := expose(filepath.Join(pkg, "cli"), "", filepath.Join(pkg, "data"), nil, nil); err == nil {
			t.Fatal("an installation around the data directory was exposed")
		}
		if _, err := expose(filepath.Join(pkg, "cli"), "", dir, nil, nil); err == nil {
			t.Fatal("a program inside the data directory was exposed")
		}
	})
}

func TestProbes(t *testing.T) {
	t.Parallel()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for _, p := range []string{".ssh/id_ed25519", ".gitconfig", ".claude/.credentials.json", ".claude/history.jsonl",
		".local/share/kindevai-runner/keys/ed25519.pem", ".local/share/kindevai-runner/workspaces/w/f",
		".local/bin/claude"} {
		write(t, filepath.Join(home, p), "x", 0o600)
	}
	units := []string{filepath.Join(home, ".local/share/kindevai-runner/workspaces/w")}
	points := []string{filepath.Join(home, ".claude/.credentials.json"), filepath.Join(home, ".local/bin/claude")}
	got := probes(home, units, points, 100)
	want := []string{filepath.Join(home, ".claude/history.jsonl"), filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".local/share/kindevai-runner/keys"), filepath.Join(home, ".ssh")}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("probes = %v\nwant %v", got, want)
	}
	if got := probes(home, units, points, 2); len(got) != 2 {
		t.Fatalf("budget not kept: %v", got)
	}
}
