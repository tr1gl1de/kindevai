package fsstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

func TestLayout(t *testing.T) {
	t.Parallel()
	d := t.TempDir()
	s := Store{DataDir: d}
	if got := s.MirrorPath("acme", "api"); got != filepath.Join(d, "repos", "acme", "api.git") {
		t.Fatal(got)
	}
	if got := s.WorktreePath("acme", "api", "KIN-1-s1"); got != filepath.Join(d, "workspaces", "acme", "api", "KIN-1-s1") {
		t.Fatal(got)
	}
	if free, err := s.Free(); err != nil || free == 0 {
		t.Fatalf("%d %v", free, err)
	}
}

func TestLockIsExclusive(t *testing.T) {
	t.Parallel()
	s := Store{DataDir: t.TempDir()}
	dir := s.WorktreePath("acme", "api", "KIN-1-s1")
	unlock, err := s.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(dir); !errors.Is(err, app.ErrBusy) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock2, err := s.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}

func TestMetaRoundTrip(t *testing.T) {
	t.Parallel()
	s := Store{DataDir: t.TempDir()}
	m := app.Meta{Step: "s1", Fence: 2, Repository: "acme/api", Branch: "feature/KIN-1-s1", Dir: "/x",
		Instructions: []domain.Instruction{{Path: "AGENTS.md", SHA256: "h"}}}
	if err := s.SaveMeta(m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadMeta("s1", 2)
	if err != nil || !ok || got.Branch != m.Branch || got.Instructions[0].Path != "AGENTS.md" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	st, err := os.Stat(filepath.Join(s.DataDir, "workspaces", ".meta", "s1-2.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if err := s.DeleteMeta("s1", 2); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LoadMeta("s1", 2); ok || err != nil {
		t.Fatalf("after delete: %v %v", ok, err)
	}
	if _, _, err := s.LoadMeta("../x", 1); err == nil {
		t.Fatal("path traversal in step accepted")
	}
}
