// Package domain is the runner's workspace context (KIN-29): the git worktree an attempt runs in, its branch and
// base, and the repository's native instruction files. Pure Go, no I/O.
//
// Everything that comes out of the repository (file names, instruction files, git output) is untrusted data: it
// is hashed and reported, never parsed into settings, so it can never widen what the runner or the agent may do.
package domain

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

var (
	// ErrInvalidRef: a branch or base name that is not a plain, safe git branch name.
	ErrInvalidRef = errors.New("invalid git branch name")
	// ErrProtectedBranch: agents never work on main/master (KB KIN-A-20); only the Owner merges there.
	ErrProtectedBranch = errors.New("protected branch: agents never work on main/master")
	// ErrInvalidRepository: not an "owner/name" repository.
	ErrInvalidRepository = errors.New("invalid repository")
)

// WorkBranchPrefix starts every branch an attempt works on (git flow: feature/<ISSUE>-<slug>).
const WorkBranchPrefix = "feature/"

var (
	refRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	idRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,127}$`)
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,99}$`)
)

// CheckBase validates a base branch name: a subset of git's check-ref-format (letters, digits, ".", "_", "-" and
// "/" between non-empty components that do not start with "." or end with ".lock"), so it can never be read as an
// option, a revision expression or a path outside refs/.
func CheckBase(ref string) error {
	if !refRe.MatchString(ref) || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.HasSuffix(ref, ".") {
		return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	for c := range strings.SplitSeq(ref, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
		}
	}
	return nil
}

// CheckWorkBranch validates the branch an attempt commits to: feature/…, never main or master.
func CheckWorkBranch(b string) error {
	if p := strings.ToLower(strings.TrimPrefix(b, "refs/heads/")); p == "main" || p == "master" {
		return fmt.Errorf("%w: %q", ErrProtectedBranch, b)
	}
	if err := CheckBase(b); err != nil {
		return err
	}
	if !strings.HasPrefix(b, WorkBranchPrefix) || len(b) == len(WorkBranchPrefix) {
		return fmt.Errorf("%w: %q is not a %s branch", ErrInvalidRef, b, WorkBranchPrefix)
	}
	return nil
}

// Leaf is the workspace name of a step, <task>--<step>: unambiguous, since an id never contains "--" nor starts
// or ends with "-".
func Leaf(task, step string) (string, error) {
	for _, id := range []string{task, step} {
		if !idRe.MatchString(id) || strings.Contains(id, "--") || strings.HasSuffix(id, "-") {
			return "", fmt.Errorf("%w: task %q, step %q", ErrInvalidRef, task, step)
		}
	}
	return task + "--" + step, nil
}

// WorkBranch is the branch of a step: feature/<task>--<step>. Every attempt of the step continues on it.
func WorkBranch(task, step string) (string, error) {
	leaf, err := Leaf(task, step)
	if err != nil {
		return "", err
	}
	b := WorkBranchPrefix + leaf
	return b, CheckWorkBranch(b)
}

// SplitRepository splits "owner/name"; neither part may start with "." (no "..", ".git" or hidden directories).
func SplitRepository(r string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(r, "/")
	if !ok || !ownerRe.MatchString(owner) || !nameRe.MatchString(name) {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidRepository, r)
	}
	return owner, name, nil
}

// Instruction files (KB KIN-A-15 "Repository-native instructions"): found in any directory, or in a .claude
// directory's agents, skills, rules and commands trees and settings, or at the repository root (Codex, MCP).
var (
	instructionNames  = []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "CLAUDE.local.md"}
	claudeTrees       = []string{"agents", "skills", "rules", "commands"}
	claudeFiles       = []string{"settings.json", "settings.local.json"}
	rootInstructions  = []string{".mcp.json"}
	rootInstructionTr = []string{".codex"}
	skippedDirs       = []string{".git", "node_modules", "vendor", ".venv", "__pycache__"}
)

// IsInstruction reports whether the repository-relative, slash-separated path p is an instruction file.
func IsInstruction(p string) bool {
	parts := strings.Split(p, "/")
	if slices.Contains(instructionNames, parts[len(parts)-1]) {
		return true
	}
	if len(parts) == 1 && slices.Contains(rootInstructions, p) {
		return true
	}
	if len(parts) > 1 && slices.Contains(rootInstructionTr, parts[0]) {
		return true
	}
	for i, c := range parts[:len(parts)-1] {
		if c != ".claude" {
			continue
		}
		rest := parts[i+1:]
		if len(rest) > 1 && slices.Contains(claudeTrees, rest[0]) || len(rest) == 1 && slices.Contains(claudeFiles, rest[0]) {
			return true
		}
	}
	return false
}

// Descend reports whether discovery looks into directory d (dependency and VCS trees are skipped).
func Descend(d string) bool {
	return !slices.Contains(skippedDirs, path.Base(d))
}

// Instruction is one discovered instruction file (see protocol.InstructionFile).
type Instruction struct {
	Path    string
	SHA256  string
	Size    int64
	Symlink bool
}

// InstructionChange is an instruction file whose hash differs between two scans; an empty hash: absent.
type InstructionChange struct {
	Path   string
	Before string
	After  string
}

// InstructionChanges compares the manifest at the start of the attempt with the one at its end, sorted by path.
func InstructionChanges(before, after []Instruction) []InstructionChange {
	b := map[string]string{}
	for _, i := range before {
		b[i.Path] = i.SHA256
	}
	a := map[string]string{}
	for _, i := range after {
		a[i.Path] = i.SHA256
	}
	var out []InstructionChange
	for p, h := range b {
		if a[p] != h {
			out = append(out, InstructionChange{Path: p, Before: h, After: a[p]})
		}
	}
	for p, h := range a {
		if _, ok := b[p]; !ok {
			out = append(out, InstructionChange{Path: p, After: h})
		}
	}
	slices.SortFunc(out, func(x, y InstructionChange) int { return strings.Compare(x.Path, y.Path) })
	return out
}

// Change is one changed path (see protocol.ChangedFile).
type Change struct {
	Path   string
	Status string
}

// ParseNameStatus parses `git diff --name-status -z --no-renames`: status NUL path NUL, repeated.
func ParseNameStatus(out []byte) ([]Change, error) {
	if len(out) == 0 {
		return nil, nil
	}
	f := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	if len(f)%2 != 0 {
		return nil, errors.New("malformed git name-status output")
	}
	cs := make([]Change, 0, len(f)/2)
	for i := 0; i < len(f); i += 2 {
		cs = append(cs, Change{Path: string(f[i+1]), Status: string(f[i])})
	}
	return cs, nil
}

// ParseUntracked parses `git ls-files --others -z` (path NUL, repeated) as added files.
func ParseUntracked(out []byte) []Change {
	var cs []Change
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			cs = append(cs, Change{Path: string(p), Status: "A"})
		}
	}
	return cs
}

// MergeChanges joins change lists sorted by path; a path listed twice keeps its first entry.
func MergeChanges(lists ...[]Change) []Change {
	var out []Change
	seen := map[string]bool{}
	for _, l := range lists {
		for _, c := range l {
			if !seen[c.Path] {
				seen[c.Path] = true
				out = append(out, c)
			}
		}
	}
	slices.SortFunc(out, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Fit keeps the leading items whose summed cost stays within budget (a message must fit the channel frame) and
// reports whether any were dropped.
func Fit[T any](items []T, cost func(T) int, budget int) ([]T, bool) {
	used := 0
	for i, it := range items {
		used += cost(it)
		if used > budget {
			return items[:i], true
		}
	}
	return items, false
}
