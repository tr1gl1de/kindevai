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

// DefaultMaxPreparing bounds how many workspaces are prepared at once (KIN-71): each preparation may be a first
// clone of a large repository, which takes the network and the disk.
const DefaultMaxPreparing = 2

// Preparation phases, reported through Request.Progress (KIN-71).
const (
	PhaseQueued   = "queued"   // waiting for a preparation slot or for another preparation of the repository
	PhaseFetching = "fetching" // fetching the base into the repository's mirror
	PhaseCheckout = "checkout" // creating the worktree and scanning its instructions
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
	// Head returns the worktree's HEAD commit without committing anything.
	Head(ctx context.Context, admin string) (string, error)
	// Diff lists the paths changed between two commits.
	Diff(ctx context.Context, admin, from, to string) ([]domain.Change, error)
	// Changes lists the paths the worktree (untracked files included) changed against base, read-only.
	Changes(ctx context.Context, admin, dir, base string) ([]domain.Change, error)
	// Patch is the unified diff from..to (commits), at most limit bytes (cut: there was more; KIN-35 review).
	Patch(ctx context.Context, admin, from, to string, limit int) (string, bool, error)
	// RemoveWorktree removes the worktree; the branch stays in the mirror.
	RemoveWorktree(ctx context.Context, mirror, dir string) error
	// Push publishes commit sha of the mirror to branch of repository, fast-forward only (KIN-36).
	Push(ctx context.Context, mirror, repository, branch, sha string, tok Token) error
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
	// ReadOnly: a reviewer's workspace (KIN-35). Its end commits nothing: whatever it wrote is discarded with the
	// worktree, so the work branch keeps the reviewed head.
	ReadOnly bool `json:"readOnly,omitempty"`
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
	ReadOnly   bool // a reviewer's workspace: Finish commits nothing (KIN-35)
	// Progress, when set, gets the preparation's phases (Phase*) as it reaches them.
	Progress func(phase string)
}

func (r Request) progress(phase string) {
	if r.Progress != nil {
		r.Progress(phase)
	}
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
	// MaxPreparing bounds the preparations running at once (DefaultMaxPreparing when zero); more wait, queued.
	MaxPreparing int

	mu   sync.Mutex
	held map[string]func() // step/fence → unlock
	// Per mirror: fetches holds one fetch at a time (it can take long: a first clone); worktrees one worktree add or
	// remove at a time (fast: an attempt's end never waits for another attempt's fetch).
	fetches   map[string]chan struct{}
	worktrees map[string]chan struct{}
	once      sync.Once
	slots     chan struct{}
}

// acquire takes a token of sem (a buffered channel used as a lock or semaphore), reporting queued first when it
// has to wait; it gives up when ctx ends.
func acquire(ctx context.Context, sem chan struct{}, r Request, queued *bool) (release func(), err error) {
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	default:
	}
	if !*queued {
		*queued = true
		r.progress(PhaseQueued)
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) slotSem() chan struct{} {
	m.once.Do(func() {
		n := m.MaxPreparing
		if n <= 0 {
			n = DefaultMaxPreparing
		}
		m.slots = make(chan struct{}, n)
	})
	return m.slots
}

// mirrorSem returns the mirror's lock in locks (created on first use).
func (m *Manager) mirrorSem(locks *map[string]chan struct{}, mirror string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if *locks == nil {
		*locks = map[string]chan struct{}{}
	}
	sem, ok := (*locks)[mirror]
	if !ok {
		sem = make(chan struct{}, 1)
		(*locks)[mirror] = sem
	}
	return sem
}

// removeWorktree removes a worktree under the mirror's worktree lock (worktree bookkeeping lives in the mirror);
// the lock is only held for worktree add/remove, never across a fetch.
func (m *Manager) removeWorktree(mirror, dir string) error {
	queued := true // nothing to report
	release, _ := acquire(context.Background(), m.mirrorSem(&m.worktrees, mirror), Request{}, &queued)
	defer release()
	return m.Git.RemoveWorktree(context.Background(), mirror, dir)
}

func key(step string, fence int64) string { return fmt.Sprintf("%s/%d", step, fence) }

// Prepare creates the attempt's worktree on feature/<task>--<step> from origin/<base>.
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
	leaf, _ := domain.Leaf(r.Task, r.Step) // valid: WorkBranch checked it
	mirror, dir := m.Store.MirrorPath(owner, name), m.Store.WorktreePath(owner, name, leaf)
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

// prepare runs within a preparation slot; the fetch under the mirror's fetch lock, worktree add under its worktree
// lock.
// Cancelling ctx stops a waiting preparation at once and a running git command (its process is killed).
func (m *Manager) prepare(ctx context.Context, r Request, branch, mirror, dir string) (Prepared, error) {
	queued := false
	releaseSlot, err := acquire(ctx, m.slotSem(), r, &queued)
	if err != nil {
		return Prepared{}, err
	}
	defer releaseSlot()
	fork, admin, err := m.checkout(ctx, r, branch, mirror, dir, &queued)
	if err != nil {
		return Prepared{}, err
	}
	files, truncated, err := m.Scanner.Scan(dir)
	if err == nil {
		err = m.Store.SaveMeta(Meta{Step: r.Step, Fence: r.Fence, Repository: r.Repository, Branch: branch, Base: r.Base,
			BaseSHA: fork, Mirror: mirror, Dir: dir, Admin: admin, Instructions: files, ReadOnly: r.ReadOnly})
	}
	if err != nil {
		_ = m.removeWorktree(mirror, dir)
		return Prepared{}, fmt.Errorf("recording the workspace: %w", err)
	}
	fitted, cut := domain.Fit(files, func(i domain.Instruction) int { return len(i.Path) + entryOverhead }, instructionBudget)
	return Prepared{Dir: dir, Repository: r.Repository, Branch: branch, Base: r.Base, BaseSHA: fork,
		Instructions: fitted, InstructionsTruncated: truncated || cut}, nil
}

// checkout fetches the base and creates the worktree, each under its mirror lock.
func (m *Manager) checkout(ctx context.Context, r Request, branch, mirror, dir string, queued *bool) (fork, admin string, err error) {
	release, err := acquire(ctx, m.mirrorSem(&m.fetches, mirror), r, queued)
	if err != nil {
		return "", "", err
	}
	r.progress(PhaseFetching)
	sha, err := m.Git.FetchBase(ctx, mirror, r.Repository, r.Base, r.Token)
	release()
	if err != nil {
		return "", "", fmt.Errorf("fetching %s of %s: %w", r.Base, r.Repository, err)
	}
	if release, err = acquire(ctx, m.mirrorSem(&m.worktrees, mirror), r, queued); err != nil {
		return "", "", err
	}
	r.progress(PhaseCheckout)
	fork, admin, err = m.Git.AddWorktree(ctx, mirror, dir, branch, sha)
	release()
	if err != nil {
		return "", "", fmt.Errorf("creating the worktree: %w", err)
	}
	return fork, admin, nil
}

// Changes reports what a running attempt changed so far (KIN-51): the worktree against the base, untracked files
// included, without committing; HeadSHA is empty. It returns nil for an attempt whose workspace this process does
// not hold (not prepared here, or already finished). Call it only while the attempt is not being finished.
func (m *Manager) Changes(ctx context.Context, step string, fence int64) (*Result, error) {
	m.mu.Lock()
	_, held := m.held[key(step, fence)]
	m.mu.Unlock()
	if !held {
		return nil, nil
	}
	meta, ok, err := m.Store.LoadMeta(step, fence)
	if err != nil || !ok {
		return nil, err
	}
	files, err := m.Git.Changes(ctx, meta.Admin, meta.Dir, meta.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("listing changed files: %w", err)
	}
	fitted, cut := domain.Fit(files, func(c domain.Change) int { return len(c.Path) + entryOverhead }, changesBudget)
	return &Result{Branch: meta.Branch, BaseSHA: meta.BaseSHA, Files: fitted, Total: len(files), Truncated: cut}, nil
}

// Diff is the diff from..to in the attempt's workspace (a reviewer's work to review, KIN-35), at most limit bytes.
// It fails for an attempt whose workspace this process does not hold.
func (m *Manager) Diff(ctx context.Context, step string, fence int64, from, to string, limit int) (string, bool, error) {
	m.mu.Lock()
	_, held := m.held[key(step, fence)]
	m.mu.Unlock()
	meta, ok, err := m.Store.LoadMeta(step, fence)
	switch {
	case err != nil:
		return "", false, err
	case !held || !ok:
		return "", false, fmt.Errorf("no workspace for attempt %s", key(step, fence))
	}
	return m.Git.Patch(ctx, meta.Admin, from, to, limit)
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
	var head string
	if meta.ReadOnly {
		head, err = m.Git.Head(ctx, meta.Admin)
	} else {
		head, err = m.Git.Snapshot(ctx, meta.Admin, meta.Dir, fmt.Sprintf(SnapshotMessage, step, fence))
	}
	if err != nil {
		return nil, fmt.Errorf("saving the attempt's work to %s: %w", meta.Branch, err)
	}
	files, err := m.Git.Diff(ctx, meta.Admin, meta.BaseSHA, head)
	if err != nil {
		return nil, fmt.Errorf("listing changed files: %w", err)
	}
	if err := m.removeWorktree(meta.Mirror, meta.Dir); err != nil {
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

// PushRequest asks to publish a step's work branch (KIN-36): Head is the commit the review approved.
type PushRequest struct {
	Step       string
	Task       string
	Repository string
	Head       string
	Token      Token
}

// Push publishes the step's work branch (feature/<task>--<step>) at r.Head from the repository cache to the
// repository, fast-forward only. It returns the branch. The branch is derived here, never taken from the request,
// so a push can only ever target the step's own feature branch.
func (m *Manager) Push(ctx context.Context, r PushRequest) (string, error) {
	owner, name, err := domain.SplitRepository(r.Repository)
	if err != nil {
		return "", err
	}
	branch, err := domain.WorkBranch(r.Task, r.Step)
	if err != nil {
		return "", err
	}
	mirror := m.Store.MirrorPath(owner, name)
	queued := true // nothing to report
	release, err := acquire(ctx, m.mirrorSem(&m.fetches, mirror), Request{}, &queued)
	if err != nil {
		return "", err
	}
	defer release()
	if err := m.Git.Push(ctx, mirror, r.Repository, branch, r.Head, r.Token); err != nil {
		return "", fmt.Errorf("pushing %s: %w", branch, err)
	}
	return branch, nil
}
