// Package scan discovers a worktree's repository-native instruction files (KIN-29, KB KIN-A-15): AGENTS.md,
// CLAUDE.md, .claude/{agents,skills,rules,commands,settings*}, .codex/, .mcp.json. They are untrusted data: only
// path, size and SHA-256 are taken, nothing is parsed. The walk is confined to the worktree with os.Root, never
// follows a symlink (a link is reported with the hash of its target text) and is bounded.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

// Defaults of Scanner.
const (
	DefaultMaxFiles   = 256
	DefaultMaxEntries = 50000
	DefaultMaxDepth   = 16
)

// Scanner walks a worktree.
type Scanner struct {
	MaxFiles   int // instruction files reported (DefaultMaxFiles)
	MaxEntries int // directory entries looked at (DefaultMaxEntries)
	MaxDepth   int // directory depth (DefaultMaxDepth)
}

var _ app.Scanner = Scanner{}

var errStop = errors.New("limit reached")

type walk struct {
	root      *os.Root
	s         Scanner
	entries   int
	files     []domain.Instruction
	truncated bool
}

// Scan implements app.Scanner.
func (s Scanner) Scan(dir string) ([]domain.Instruction, bool, error) {
	if s.MaxFiles <= 0 {
		s.MaxFiles = DefaultMaxFiles
	}
	if s.MaxEntries <= 0 {
		s.MaxEntries = DefaultMaxEntries
	}
	if s.MaxDepth <= 0 {
		s.MaxDepth = DefaultMaxDepth
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false, fmt.Errorf("opening worktree: %w", err)
	}
	defer func() { _ = root.Close() }()
	w := &walk{root: root, s: s}
	if err := w.dir(".", 0); err != nil && !errors.Is(err, errStop) {
		return nil, false, err
	}
	slices.SortFunc(w.files, func(a, b domain.Instruction) int { return strings.Compare(a.Path, b.Path) })
	return w.files, w.truncated, nil
}

func (w *walk) dir(rel string, depth int) error {
	if depth > w.s.MaxDepth {
		w.truncated = true
		return nil
	}
	f, err := w.root.Open(rel)
	if err != nil {
		return err
	}
	ents, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return err
	}
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range ents {
		if w.entries++; w.entries > w.s.MaxEntries {
			w.truncated = true
			return errStop
		}
		p := path.Join(rel, e.Name())
		switch t := e.Type(); {
		case t.IsDir():
			if domain.Descend(p) {
				if err := w.dir(p, depth+1); err != nil {
					return err
				}
			}
		case domain.IsInstruction(p) && (t.IsRegular() || t&fs.ModeSymlink != 0):
			if len(w.files) >= w.s.MaxFiles {
				w.truncated = true
				return errStop
			}
			in, err := w.file(p, t&fs.ModeSymlink != 0)
			if err != nil {
				return fmt.Errorf("hashing %s: %w", p, err)
			}
			w.files = append(w.files, in)
		}
	}
	return nil
}

func (w *walk) file(p string, link bool) (domain.Instruction, error) {
	h := sha256.New()
	if link {
		target, err := w.root.Readlink(p)
		if err != nil {
			return domain.Instruction{}, err
		}
		h.Write([]byte(target))
		return domain.Instruction{Path: p, SHA256: hex.EncodeToString(h.Sum(nil)), Size: int64(len(target)), Symlink: true}, nil
	}
	// A file swapped for a symlink after the listing is still confined to the worktree by the root.
	f, err := w.root.Open(p)
	if err != nil {
		return domain.Instruction{}, err
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(h, f)
	if err != nil {
		return domain.Instruction{}, err
	}
	return domain.Instruction{Path: p, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}
