// Package app holds the runner's workspace use cases (KIN-29): prepare an attempt's git worktree (bare repository
// cache, step branch from the base, instruction manifest) and finish it (commit leftover work to the branch,
// report the changes against the base, remove the worktree but keep the branch).
package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

var (
	// ErrBusy: another attempt holds the workspace (one writer per workspace).
	ErrBusy = errors.New("workspace is in use by another attempt")
	// ErrDiskFull: less free space than the configured minimum on the data dir's filesystem.
	ErrDiskFull = errors.New("not enough free disk space for a workspace")
)

// DefaultMinFree is the free space a workspace needs to be prepared: the "critical" threshold of KB KIN-A-26.
const DefaultMinFree = 2 << 30

// Message budgets: a workspace event body stays well below protocol.MaxBodyBytes.
const (
	instructionBudget = 40 << 10
	changesBudget     = 40 << 10
	entryOverhead     = 96 // JSON field names, hash, size, status
)

// Token is a short-lived git credential. It never formats in clear (logs, errors); Reveal hands it to the
// credential helper only.
type Token struct{ v string }

// NewToken wraps a token ("" for none: public repositories, local tests).
func NewToken(s string) Token { return Token{s} }

// Reveal returns the token.
func (t Token) Reveal() string { return t.v }

// Empty reports no token.
func (t Token) Empty() bool { return t.v == "" }

// String redacts the token.
func (Token) String() string { return "[redacted]" }

// GoString redacts the token.
func (Token) GoString() string { return "[redacted]" }

// Git is the git CLI (adapters/gitcli). Commands on a worktree name its administrative directory (admin) so they
// never follow a .git file the agent could have rewritten.
type Git interface {
	// FetchBase fetches branch base of repository into the bare cache mirror (created if missing) and returns the
	// commit it points to.
	FetchBase(ctx context.Context, mirror, repository, base string, tok Token) (string, error)
	// AddWorktree checks branch out in dir, creating it at baseSHA when it does not exist yet. It returns the
	// branch's fork point from the base (baseSHA for a new branch) and the worktree's administrative directory.
	AddWorktree(ctx context.Context, mirror, dir, branch, baseSHA string) (fork, admin string, err error)
	// Snapshot commits uncommitted work in the worktree (hooks disabled) and returns HEAD.
	Snapshot(ctx context.Context, admin, dir, message string) (string, error)
	// Diff lists the paths changed between two commits.
	Diff(ctx context.Context, admin, from, to string) ([]domain.Change, error)
	// RemoveWorktree removes the worktree; the branch stays in the mirror.
	RemoveWorktree(ctx context.Context, mirror, dir string) error
}

// Scanner discovers instruction files below a worktree without following symlinks out of it.
type Scanner interface {
	Scan(dir string) (files []domain.Instruction, truncated bool, err error)
}

// Meta is the on-disk record of a prepared workspace; it outlives a runner restart, so the attempt's end is
// reported and the worktree removed by the next runner process.
type Meta struct {
	Step         string               `json:"step"`
	Fence        int64                `json:"fence"`
	Repository   string               `json:"repository"`
	Branch       string               `json:"branch"`
	Base         string               `json:"base"`
	BaseSHA      string               `json:"baseSha"`
	Mirror       string               `json:"mirror"`
	Dir          string               `json:"dir"`
	Admin        string               `json:"admin"`
	Instructions []domain.Instruction `json:"instructions"`
}

// Store is the workspace layout under the runner data dir (adapters/fsstore).
type Store interface {
	MirrorPath(owner, name string) string
	WorktreePath(owner, name, leaf string) string
	// Lock takes the exclusive writer lock of a workspace (ErrBusy when it is held).
	Lock(dir string) (unlock func(), err error)
	SaveMeta(m Meta) error
	LoadMeta(step string, fence int64) (Meta, bool, error)
	DeleteMeta(step string, fence int64) error
	// Free is the space available to the runner on the data dir's filesystem.
	Free() (uint64, error)
}

// Request is an attempt's workspace.
type Request struct {
	Step       string
	Fence      int64
	Task       string
	Repository string
	Base       string
	Token      Token
}

// Prepared is a ready worktree.
type Prepared struct {
	Dir                   string
	Repository            string
	Branch                string
	Base                  string
	BaseSHA               string
	Instructions          []domain.Instruction
	InstructionsTruncated bool
}

// Result is what an attempt changed.
type Result struct {
	Branch              string
	BaseSHA             string
	HeadSHA             string
	Files               []domain.Change
	Total               int
	Truncated           bool
	InstructionsChanged []domain.InstructionChange
}

// Manager prepares and finishes workspaces.
type Manager struct {
	Git     Git
	Scanner Scanner
	Store   Store
	MinFree uint64 // free space needed to prepare one (0: none)

	mu   sync.Mutex
	held map[string]func() // step/fence → unlock
}

func key(step string, fence int64) string { return fmt.Sprintf("%s/%d", step, fence) }

// Prepare creates the attempt's worktree on feature/<task>-<step> from origin/<base>.
func (m *Manager) Prepare(ctx context.Context, r Request) (Prepared, error) {
	owner, name, err := domain.SplitRepository(r.Repository)
	if err != nil {
		return Prepared{}, err
	}
	if err := domain.CheckBase(r.Base); err != nil {
		return Prepared{}, err
	}
	branch, err := domain.WorkBranch(r.Task, r.Step)
	if err != nil {
		return Prepared{}, err
	}
	if free, err := m.Store.Free(); err != nil {
		return Prepared{}, fmt.Errorf("measuring free disk space: %w", err)
	} else if free < m.MinFree {
		return Prepared{}, fmt.Errorf("%w: %d MiB free, %d MiB needed", ErrDiskFull, free>>20, m.MinFree>>20)
	}
	mirror, dir := m.Store.MirrorPath(owner, name), m.Store.WorktreePath(owner, name, r.Task+"-"+r.Step)
	unlock, err := m.Store.Lock(dir)
	if err != nil {
		return Prepared{}, err
	}
	p, err := m.prepare(ctx, r, branch, mirror, dir)
	if err != nil {
		unlock()
		return Prepared{}, err
	}
	m.mu.Lock()
	if m.held == nil {
		m.held = map[string]func(){}
	}
	m.held[key(r.Step, r.Fence)] = unlock
	m.mu.Unlock()
	return p, nil
}

func (m *Manager) prepare(ctx context.Context, r Request, branch, mirror, dir string) (Prepared, error) {
	sha, err := m.Git.FetchBase(ctx, mirror, r.Repository, r.Base, r.Token)
	if err != nil {
		return Prepared{}, fmt.Errorf("fetching %s of %s: %w", r.Base, r.Repository, err)
	}
	fork, admin, err := m.Git.AddWorktree(ctx, mirror, dir, branch, sha)
	if err != nil {
		return Prepared{}, fmt.Errorf("creating the worktree: %w", err)
	}
	files, truncated, err := m.Scanner.Scan(dir)
	if err == nil {
		err = m.Store.SaveMeta(Meta{Step: r.Step, Fence: r.Fence, Repository: r.Repository, Branch: branch, Base: r.Base,
			BaseSHA: fork, Mirror: mirror, Dir: dir, Admin: admin, Instructions: files})
	}
	if err != nil {
		_ = m.Git.RemoveWorktree(context.WithoutCancel(ctx), mirror, dir)
		return Prepared{}, fmt.Errorf("recording the workspace: %w", err)
	}
	fitted, cut := domain.Fit(files, func(i domain.Instruction) int { return len(i.Path) + entryOverhead }, instructionBudget)
	return Prepared{Dir: dir, Repository: r.Repository, Branch: branch, Base: r.Base, BaseSHA: fork,
		Instructions: fitted, InstructionsTruncated: truncated || cut}, nil
}

// SnapshotMessage is the commit message of work an attempt left uncommitted.
const SnapshotMessage = "kindevai: uncommitted changes at the end of attempt %s/%d"

// Finish reports what the attempt changed and removes its worktree (the branch keeps the work). It returns nil
// for an attempt without a workspace or one already finished. On an error the worktree is kept (never delete
// work that was not saved to the branch).
func (m *Manager) Finish(ctx context.Context, step string, fence int64) (*Result, error) {
	meta, ok, err := m.Store.LoadMeta(step, fence)
	if err != nil || !ok {
		return nil, err
	}
	m.mu.Lock()
	unlock, held := m.held[key(step, fence)]
	delete(m.held, key(step, fence))
	m.mu.Unlock()
	if !held { // prepared by an earlier runner process
		if unlock, err = m.Store.Lock(meta.Dir); err != nil {
			return nil, err
		}
	}
	defer unlock()
	after, _, err := m.Scanner.Scan(meta.Dir)
	if err != nil {
		return nil, fmt.Errorf("scanning instructions: %w", err)
	}
	head, err := m.Git.Snapshot(ctx, meta.Admin, meta.Dir, fmt.Sprintf(SnapshotMessage, step, fence))
	if err != nil {
		return nil, fmt.Errorf("saving the attempt's work to %s: %w", meta.Branch, err)
	}
	files, err := m.Git.Diff(ctx, meta.Admin, meta.BaseSHA, head)
	if err != nil {
		return nil, fmt.Errorf("listing changed files: %w", err)
	}
	if err := m.Git.RemoveWorktree(ctx, meta.Mirror, meta.Dir); err != nil {
		return nil, fmt.Errorf("removing the worktree: %w", err)
	}
	if err := m.Store.DeleteMeta(step, fence); err != nil {
		return nil, err
	}
	fitted, cut := domain.Fit(files, func(c domain.Change) int { return len(c.Path) + entryOverhead }, changesBudget)
	ichanged, _ := domain.Fit(domain.InstructionChanges(meta.Instructions, after),
		func(c domain.InstructionChange) int { return len(c.Path) + 2*entryOverhead }, instructionBudget/4)
	return &Result{Branch: meta.Branch, BaseSHA: meta.BaseSHA, HeadSHA: head, Files: fitted, Total: len(files),
		Truncated: cut, InstructionsChanged: ichanged}, nil
}
