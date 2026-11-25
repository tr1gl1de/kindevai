package main

import (
	"context"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/fsstore"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/gitcli"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/adapters/scan"
	workspaceapp "github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
)

// changesEvery is how often a running attempt reports its changed files (only when they changed): the Control Plane
// stops a step that leaves its approved scope within about this long instead of at its end (KIN-51).
const changesEvery = 30 * time.Second

// newWorkspaces wires the workspace context (KIN-29): bare repository caches in repos/, attempt worktrees in
// workspaces/, git driven with the ai-runner binary as its credential helper (`ai-runner git-credential`).
func newWorkspaces(dataDir, exe, gitBaseURL string, minFreeMiB uint64) (sessionWorkspaces, error) {
	g, err := gitcli.New(gitBaseURL, []string{exe, gitcli.HelperCommand})
	if err != nil {
		return sessionWorkspaces{}, err
	}
	return sessionWorkspaces{&workspaceapp.Manager{Git: g, Scanner: scan.Scanner{}, Store: fsstore.Store{DataDir: dataDir},
		MinFree: minFreeMiB << 20}}, nil
}

// sessionWorkspaces adapts the workspace context's Manager to the session context's port.
type sessionWorkspaces struct{ m *workspaceapp.Manager }

var _ sessionapp.Workspaces = sessionWorkspaces{}

func (w sessionWorkspaces) Prepare(ctx context.Context, r sessionapp.WorkspaceRequest) (string, protocol.WorkspacePrepared, error) {
	p, err := w.m.Prepare(ctx, workspaceapp.Request{Step: r.Step, Fence: r.Fence, Task: r.Task, Repository: r.Repository,
		Base: r.Base, Token: workspaceapp.NewToken(r.Token.Reveal()), Progress: r.Progress})
	if err != nil {
		return "", protocol.WorkspacePrepared{}, err
	}
	files := make([]protocol.InstructionFile, 0, len(p.Instructions))
	for _, i := range p.Instructions {
		files = append(files, protocol.InstructionFile{Path: i.Path, SHA256: i.SHA256, Size: i.Size, Symlink: i.Symlink})
	}
	return p.Dir, protocol.WorkspacePrepared{Repository: p.Repository, Branch: p.Branch, Base: p.Base, BaseSHA: p.BaseSHA,
		Instructions: files, InstructionsTruncated: p.InstructionsTruncated}, nil
}

func (w sessionWorkspaces) Finish(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	res, err := w.m.Finish(ctx, step, fence)
	if err != nil || res == nil {
		return nil, err
	}
	return changes(res), nil
}

// Changes reports a running attempt's worktree changes (KIN-51).
func (w sessionWorkspaces) Changes(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error) {
	res, err := w.m.Changes(ctx, step, fence)
	if err != nil || res == nil {
		return nil, err
	}
	ch := changes(res)
	ch.Interim = true
	return ch, nil
}

func changes(res *workspaceapp.Result) *protocol.WorkspaceChanges {
	files := make([]protocol.ChangedFile, 0, len(res.Files))
	for _, f := range res.Files {
		files = append(files, protocol.ChangedFile{Path: f.Path, Status: f.Status})
	}
	var ichanged []protocol.InstructionChange
	for _, c := range res.InstructionsChanged {
		ichanged = append(ichanged, protocol.InstructionChange{Path: c.Path, Before: c.Before, After: c.After})
	}
	return &protocol.WorkspaceChanges{Branch: res.Branch, BaseSHA: res.BaseSHA, HeadSHA: res.HeadSHA, Files: files,
		FilesTotal: res.Total, Truncated: res.Truncated, InstructionsChanged: ichanged}
}

// Diff is the diff a reviewer reviews (KIN-35).
func (w sessionWorkspaces) Diff(ctx context.Context, step string, fence int64, from, to string, limit int) (string, bool, error) {
	return w.m.Diff(ctx, step, fence, from, to, limit)
}
