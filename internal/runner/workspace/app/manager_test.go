package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

type fakeGit struct {
	mu       sync.Mutex
	calls    []string
	tokens   []string
	fetchErr error
	changes  []domain.Change
	removed  []string
}

func (g *fakeGit) log(s string) { g.mu.Lock(); g.calls = append(g.calls, s); g.mu.Unlock() }

func (g *fakeGit) FetchBase(_ context.Context, mirror, repository, base string, tok Token) (string, error) {
	g.log("fetch " + repository + " " + base)
	g.mu.Lock()
	g.tokens = append(g.tokens, tok.Reveal())
	g.mu.Unlock()
	return "base-sha", g.fetchErr
}

func (g *fakeGit) AddWorktree(_ context.Context, mirror, dir, branch, baseSHA string) (string, string, error) {
	g.log("add " + branch + " " + baseSHA)
	return baseSHA, dir + ".admin", nil
}

func (g *fakeGit) Snapshot(_ context.Context, admin, dir, msg string) (string, error) {
	g.log("snapshot")
	return "head-sha", nil
}

func (g *fakeGit) Diff(_ context.Context, admin, from, to string) ([]domain.Change, error) {
	g.log("diff " + from + " " + to)
	return g.changes, nil
}

func (g *fakeGit) Patch(_ context.Context, admin, from, to string, limit int) (string, bool, error) {
	g.log("patch " + from + " " + to)
	return "diff --git a/x b/x", false, nil
}

func (g *fakeGit) Changes(_ context.Context, admin, dir, base string) ([]domain.Change, error) {
	g.log("changes " + base)
	return g.changes, nil
}

func (g *fakeGit) RemoveWorktree(_ context.Context, mirror, dir string) error {
	g.mu.Lock()
	g.removed = append(g.removed, dir)
	g.mu.Unlock()
	return nil
}

type fakeScanner struct {
	mu   sync.Mutex
	scan [][]domain.Instruction
}

func (s *fakeScanner) Scan(string) ([]domain.Instruction, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.scan) == 0 {
		return nil, false, nil
	}
	out := s.scan[0]
	if len(s.scan) > 1 {
		s.scan = s.scan[1:]
	}
	return out, false, nil
}

type fakeStore struct {
	mu    sync.Mutex
	meta  map[string]Meta
	locks map[string]bool
	free  uint64
}

func newStore() *fakeStore {
	return &fakeStore{meta: map[string]Meta{}, locks: map[string]bool{}, free: 10 << 30}
}

func (s *fakeStore) MirrorPath(owner, name string) string { return "/r/" + owner + "/" + name + ".git" }
func (s *fakeStore) WorktreePath(owner, name, leaf string) string {
	return "/w/" + owner + "/" + name + "/" + leaf
}

func (s *fakeStore) Lock(path string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[path] {
		return nil, ErrBusy
	}
	s.locks[path] = true
	return func() { s.mu.Lock(); delete(s.locks, path); s.mu.Unlock() }, nil
}

func k(step string, fence int64) string { return fmt.Sprintf("%s/%d", step, fence) }

func (s *fakeStore) SaveMeta(m Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta[k(m.Step, m.Fence)] = m
	return nil
}

func (s *fakeStore) LoadMeta(step string, fence int64) (Meta, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.meta[k(step, fence)]
	return m, ok, nil
}

func (s *fakeStore) DeleteMeta(step string, fence int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.meta, k(step, fence))
	return nil
}

func (s *fakeStore) Free() (uint64, error) { return s.free, nil }

func newManager() (*Manager, *fakeGit, *fakeStore, *fakeScanner) {
	g, st, sc := &fakeGit{}, newStore(), &fakeScanner{}
	return &Manager{Git: g, Store: st, Scanner: sc, MinFree: 1 << 30}, g, st, sc
}

func req() Request {
	return Request{Step: "st1", Fence: 1, Task: "KIN-29", Repository: "acme/api", Base: "dev", Token: NewToken("ghs_x")}
}

func TestPrepareCreatesTheStepBranchFromTheBase(t *testing.T) {
	t.Parallel()
	m, g, st, sc := newManager()
	sc.scan = [][]domain.Instruction{{{Path: "AGENTS.md", SHA256: "h"}}}
	p, err := m.Prepare(t.Context(), req())
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "feature/KIN-29--st1" || p.BaseSHA != "base-sha" || p.Dir != "/w/acme/api/KIN-29--st1" ||
		len(p.Instructions) != 1 {
		t.Fatalf("%+v", p)
	}
	if strings.Join(g.calls, ";") != "fetch acme/api dev;add feature/KIN-29--st1 base-sha" || g.tokens[0] != "ghs_x" {
		t.Fatalf("%v", g.calls)
	}
	if _, ok, _ := st.LoadMeta("st1", 1); !ok {
		t.Fatal("no metadata recorded")
	}
}

func TestPrepareRefusesBadInputBeforeTouchingGit(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*Request){
		"repository": func(r *Request) { r.Repository = "../etc" },
		"base":       func(r *Request) { r.Base = "--upload-pack=x" },
		"task":       func(r *Request) { r.Task = "a/b" },
	} {
		m, g, _, _ := newManager()
		r := req()
		mod(&r)
		if _, err := m.Prepare(t.Context(), r); err == nil {
			t.Errorf("%s accepted", name)
		}
		if len(g.calls) != 0 {
			t.Errorf("%s: git called: %v", name, g.calls)
		}
	}
}

func TestPrepareNeedsFreeDisk(t *testing.T) {
	t.Parallel()
	m, g, st, _ := newManager()
	st.free = 100 << 20
	if _, err := m.Prepare(t.Context(), req()); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("%v", err)
	}
	if len(g.calls) != 0 {
		t.Fatal("fetched without disk space")
	}
}

func TestOneWriterPerWorkspace(t *testing.T) {
	t.Parallel()
	m, _, _, _ := newManager()
	if _, err := m.Prepare(t.Context(), req()); err != nil {
		t.Fatal(err)
	}
	r := req()
	r.Fence = 2 // the same step's worktree while the first attempt holds it
	if _, err := m.Prepare(t.Context(), r); !errors.Is(err, ErrBusy) {
		t.Fatalf("second writer: %v", err)
	}
	if _, err := m.Finish(t.Context(), "st1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prepare(t.Context(), r); err != nil {
		t.Fatalf("after the first finished: %v", err)
	}
}

func TestFailedFetchReleasesTheWorkspace(t *testing.T) {
	t.Parallel()
	m, g, st, _ := newManager()
	g.fetchErr = errors.New("unreachable")
	if _, err := m.Prepare(t.Context(), req()); err == nil {
		t.Fatal("no error")
	}
	if len(st.locks) != 0 || len(st.meta) != 0 {
		t.Fatalf("left behind: %v %v", st.locks, st.meta)
	}
}

func TestFinishReportsChangesAndRemovesTheWorktree(t *testing.T) {
	t.Parallel()
	m, g, st, sc := newManager()
	sc.scan = [][]domain.Instruction{{{Path: "CLAUDE.md", SHA256: "a"}}, {{Path: "CLAUDE.md", SHA256: "b"}}}
	g.changes = []domain.Change{{Path: "a.go", Status: "M"}}
	if _, err := m.Prepare(t.Context(), req()); err != nil {
		t.Fatal(err)
	}
	res, err := m.Finish(t.Context(), "st1", 1)
	if err != nil || res == nil {
		t.Fatalf("%v %v", res, err)
	}
	if res.BaseSHA != "base-sha" || res.HeadSHA != "head-sha" || res.Total != 1 || res.Files[0].Path != "a.go" ||
		len(res.InstructionsChanged) != 1 || res.InstructionsChanged[0].After != "b" {
		t.Fatalf("%+v", res)
	}
	if len(g.removed) != 1 || len(st.meta) != 0 || len(st.locks) != 0 {
		t.Fatalf("not cleaned: %v %v %v", g.removed, st.meta, st.locks)
	}
	if res, err := m.Finish(t.Context(), "st1", 1); res != nil || err != nil {
		t.Fatalf("second finish: %v %v", res, err)
	}
}

func TestFinishTruncatesLongChangeLists(t *testing.T) {
	t.Parallel()
	m, g, _, _ := newManager()
	for i := range 5000 {
		g.changes = append(g.changes, domain.Change{Path: fmt.Sprintf("dir/%0100d.go", i), Status: "A"})
	}
	if _, err := m.Prepare(t.Context(), req()); err != nil {
		t.Fatal(err)
	}
	res, err := m.Finish(t.Context(), "st1", 1)
	if err != nil || !res.Truncated || res.Total != 5000 || len(res.Files) == 0 || len(res.Files) >= 5000 {
		t.Fatalf("%v %d %d", err, res.Total, len(res.Files))
	}
}

func TestTokenIsRedacted(t *testing.T) {
	t.Parallel()
	tok := NewToken("ghs_secret")
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%+v %#v", req(), tok)} {
		if strings.Contains(s, "ghs_") {
			t.Fatal(s)
		}
	}
}

// KIN-51: while the attempt runs, Changes lists the worktree against the base without committing or removing
// anything; an attempt this process holds no workspace for reports nothing.
func TestChangesReportTheRunningAttempt(t *testing.T) {
	t.Parallel()
	m, g, st, _ := newManager()
	for i := range 5000 {
		g.changes = append(g.changes, domain.Change{Path: fmt.Sprintf("dir/%0100d.go", i), Status: "A"})
	}
	if res, err := m.Changes(t.Context(), "st1", 1); res != nil || err != nil {
		t.Fatalf("no workspace: %v %v", res, err)
	}
	if _, err := m.Prepare(t.Context(), req()); err != nil {
		t.Fatal(err)
	}
	res, err := m.Changes(t.Context(), "st1", 1)
	if err != nil || res == nil || res.BaseSHA != "base-sha" || res.HeadSHA != "" || res.Total != 5000 || !res.Truncated ||
		len(res.Files) == 0 || len(res.Files) >= 5000 {
		t.Fatalf("%+v %v", res, err)
	}
	if slices.Contains(g.calls, "snapshot") || len(g.removed) != 0 || len(st.meta) != 1 || len(st.locks) != 1 {
		t.Fatalf("interim report changed the workspace: %v %v", g.calls, g.removed)
	}
	if res, err := m.Changes(t.Context(), "st1", 2); res != nil || err != nil {
		t.Fatalf("other attempt: %v %v", res, err)
	}
}
