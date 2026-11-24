package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

func put(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestScanFindsInstructionFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	put(t, root, "AGENTS.md", "agents")
	put(t, root, "CLAUDE.md", "claude")
	put(t, root, "web/CLAUDE.md", "web")
	put(t, root, ".claude/skills/task/SKILL.md", "skill")
	put(t, root, ".claude/settings.json", `{"permissions":{"allow":["Bash(*)"]}}`)
	put(t, root, ".codex/config.toml", "x")
	put(t, root, "README.md", "no")
	put(t, root, "node_modules/pkg/CLAUDE.md", "skipped")
	put(t, root, ".git/CLAUDE.md", "skipped")
	files, truncated, err := Scanner{}.Scan(root)
	if err != nil || truncated {
		t.Fatalf("%v %v", err, truncated)
	}
	want := map[string]string{"AGENTS.md": sum("agents"), "CLAUDE.md": sum("claude"), "web/CLAUDE.md": sum("web"),
		".claude/skills/task/SKILL.md": sum("skill"), ".claude/settings.json": sum(`{"permissions":{"allow":["Bash(*)"]}}`),
		".codex/config.toml": sum("x")}
	if len(files) != len(want) {
		t.Fatalf("%+v", files)
	}
	for _, f := range files {
		if want[f.Path] != f.SHA256 || f.Symlink {
			t.Errorf("%+v", f)
		}
	}
	for i := 1; i < len(files); i++ {
		if files[i-1].Path >= files[i].Path {
			t.Fatal("not sorted")
		}
	}
}

// A symlinked instruction file is reported as a link (hash of its target text) and never read, so a repository
// cannot make the runner read or report files outside the worktree; a symlinked directory is not entered.
func TestScanDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	put(t, outside, "secret", "TOP SECRET")
	put(t, outside, "dir/CLAUDE.md", "outside")
	target := filepath.Join(outside, "secret")
	if err := os.Symlink(target, filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	files, _, err := Scanner{}.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != (domain.Instruction{Path: "CLAUDE.md", SHA256: sum(target), Size: int64(len(target)), Symlink: true}) {
		t.Fatalf("%+v", files)
	}
}

func TestScanIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for i := range 20 {
		put(t, root, filepath.Join("d", string(rune('a'+i)), "AGENTS.md"), "x")
	}
	files, truncated, err := Scanner{MaxFiles: 5}.Scan(root)
	if err != nil || !truncated || len(files) != 5 {
		t.Fatalf("%d %v %v", len(files), truncated, err)
	}
	if _, truncated, _ := (Scanner{MaxEntries: 10}).Scan(root); !truncated {
		t.Fatal("entry limit not applied")
	}
}
