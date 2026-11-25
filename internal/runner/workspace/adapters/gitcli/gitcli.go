// Package gitcli is the workspace context's git adapter (KIN-29): a bare repository cache per repository and one
// worktree per attempt, driven by the git CLI with an isolated configuration.
//
// Hardening: no system or global git config is read; hooks, fsmonitor and external diff/textconv drivers are off;
// only the configured base URL's protocol is allowed; implicit bare repositories are refused; every revision and
// path argument comes after --end-of-options; worktree commands name the worktree's administrative directory
// (GIT_DIR) instead of following its .git file, which the agent can rewrite. A token reaches git only through the
// credential helper (credential.go).
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

// ErrInsecureTransport: a token would be sent over plain http to a non-loopback host.
var ErrInsecureTransport = errors.New("git token refused: the base URL is not https")

// DefaultBaseURL is where repositories live ("owner/name" → https://github.com/owner/name.git).
const DefaultBaseURL = "https://github.com"

// DefaultTimeout bounds one git command (a first fetch of a large repository is the slowest).
const DefaultTimeout = 15 * time.Minute

// maxOutput bounds what a git command may print (a diff of a huge change set).
const maxOutput = 32 << 20

// Committer of the snapshot commit (work an attempt left uncommitted).
const (
	CommitterName  = "kindevai-runner"
	CommitterEmail = "runner@kindevai.invalid"
)

// Git runs the git CLI.
type Git struct {
	Program string        // "git" when empty
	Timeout time.Duration // DefaultTimeout when zero
	base    *url.URL
	helper  []string
}

var _ app.Git = (*Git)(nil)

// New validates the base URL (https, http or file, without credentials, query or fragment: the URL never carries
// a secret) and takes the credential helper command (absolute program and its arguments, e.g. the ai-runner binary
// and HelperCommand).
func New(baseURL string, helper []string) (*Git, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "file") || (u.Scheme != "file" && u.Host == "") {
		return nil, fmt.Errorf("invalid git base URL %q: want https://host[/path] without credentials", baseURL)
	}
	for _, a := range helper {
		if strings.ContainsAny(a, "'\x00\n") {
			return nil, fmt.Errorf("credential helper argument %q cannot be quoted", a)
		}
	}
	return &Git{base: u, helper: helper}, nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func checkSHA(s string) error {
	if !shaRe.MatchString(s) {
		return fmt.Errorf("not a commit id: %q", s)
	}
	return nil
}

func (g *Git) program() string {
	if g.Program == "" {
		return "git"
	}
	return g.Program
}

// config is passed with -c on every command; command-line config overrides the repository's.
func (g *Git) config() []string {
	return []string{
		"core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "core.askPass=", "credential.helper=",
		"protocol.allow=never", "protocol." + g.base.Scheme + ".allow=always", "safe.bareRepository=explicit",
		"gc.auto=0", "maintenance.auto=false", "advice.detachedHead=false", "commit.gpgSign=false",
		"diff.external=", "core.sshCommand=false",
	}
}

// environ is git's whole environment: nothing of the runner's own environment (or secrets in it) is inherited.
func environ(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "HOME=" + os.DevNull}, extra...)
}

// limited keeps the first max bytes a command writes and notes more (over). It is no io.ReaderFrom: exec copies
// through Write, so the bound holds (an embedded bytes.Buffer's ReadFrom would bypass it). Write never fails, so
// the command runs to its end.
type limited struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); len(p) > room {
		l.over = true
		l.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return l.buf.Write(p)
}

func (l *limited) Bytes() []byte  { return l.buf.Bytes() }
func (l *limited) String() string { return l.buf.String() }

// run executes git with args after the hardening config; extraConfig adds -c entries (the credential helper).
func (g *Git) run(ctx context.Context, env, extraConfig []string, args ...string) ([]byte, error) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var full []string
	for _, c := range append(g.config(), extraConfig...) {
		full = append(full, "-c", c)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, g.program(), full...) //nolint:gosec // G204: fixed program, validated arguments
	cmd.Env = environ(env...)
	stdout, stderr := &limited{max: maxOutput}, &limited{max: 2048}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil && stdout.over {
		err = errors.New("output too large")
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (g *Git) mirrorRun(ctx context.Context, mirror string, extraConfig []string, args ...string) ([]byte, error) {
	return g.run(ctx, nil, extraConfig, append([]string{"--git-dir=" + mirror}, args...)...)
}

// worktreeRun runs in a worktree through its administrative directory, never through dir/.git.
func (g *Git) worktreeRun(ctx context.Context, admin, dir string, args ...string) ([]byte, error) {
	env := []string{"GIT_DIR=" + admin}
	if dir != "" {
		env = append(env, "GIT_WORK_TREE="+dir)
	}
	return g.run(ctx, env, nil, args...)
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// URL is the repository's clone URL (no credentials).
func (g *Git) URL(repository string) string {
	return g.base.String() + "/" + repository + ".git"
}

// FetchBase implements app.Git.
func (g *Git) FetchBase(ctx context.Context, mirror, repository, base string, tok app.Token) (string, error) {
	if _, _, err := domain.SplitRepository(repository); err != nil {
		return "", err
	}
	if err := domain.CheckBase(base); err != nil {
		return "", err
	}
	if err := secfile.EnsureDir(filepath.Dir(mirror)); err != nil {
		return "", err
	}
	if !exists(filepath.Join(mirror, "HEAD")) {
		if _, err := g.run(ctx, nil, nil, "init", "--bare", "--quiet", "--template=", "--end-of-options", mirror); err != nil {
			return "", err
		}
	}
	extra, done, err := g.credentials(ctx, tok)
	if err != nil {
		return "", err
	}
	defer done()
	ref := "refs/remotes/origin/" + base
	if _, err := g.mirrorRun(ctx, mirror, extra, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
		"--end-of-options", g.URL(repository), "+refs/heads/"+base+":"+ref); err != nil {
		return "", err
	}
	out, err := g.mirrorRun(ctx, mirror, nil, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	return sha, checkSHA(sha)
}

// credentials serves tok to git through the credential helper for one command: the -c entries to pass and a
// function that stops serving it. A token is never sent over plain http to a non-loopback host.
func (g *Git) credentials(ctx context.Context, tok app.Token) (extra []string, done func(), err error) {
	if !tok.Empty() && g.base.Scheme == "http" && !loopback(g.base.Hostname()) {
		return nil, nil, ErrInsecureTransport
	}
	if tok.Empty() || g.base.Scheme == "file" {
		return nil, func() {}, nil
	}
	c, err := serve(ctx, credential{scheme: g.base.Scheme, host: g.base.Host, token: tok.Reveal()})
	if err != nil {
		return nil, nil, err
	}
	return []string{"credential.helper=" + c.helperConfig(g.helper), "credential.useHttpPath=false"}, c.close, nil
}

// Push implements app.Git (KIN-36): it publishes commit sha of the cache to branch of the repository. The target is
// checked first (a feature/ work branch, never main/master); the update is fast-forward only (never forced), and
// pushing the commit the branch already points to is a no-op.
func (g *Git) Push(ctx context.Context, mirror, repository, branch, sha string, tok app.Token) error {
	if _, _, err := domain.SplitRepository(repository); err != nil {
		return err
	}
	if err := domain.CheckWorkBranch(branch); err != nil {
		return err
	}
	if err := checkSHA(sha); err != nil {
		return err
	}
	if _, err := g.mirrorRun(ctx, mirror, nil, "cat-file", "-e", "--end-of-options", sha+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is not in the repository cache: %w", sha, err)
	}
	extra, done, err := g.credentials(ctx, tok)
	if err != nil {
		return err
	}
	defer done()
	_, err = g.mirrorRun(ctx, mirror, extra, "push", "--quiet", "--no-verify", "--no-follow-tags", "--end-of-options",
		g.URL(repository), sha+":refs/heads/"+branch)
	return err
}

// AddWorktree implements app.Git. A directory left by an earlier attempt of the same branch (its worktree was
// not removed) is reused, so its work is never lost.
func (g *Git) AddWorktree(ctx context.Context, mirror, dir, branch, baseSHA string) (string, string, error) {
	if err := domain.CheckWorkBranch(branch); err != nil {
		return "", "", err
	}
	if err := checkSHA(baseSHA); err != nil {
		return "", "", err
	}
	admin := filepath.Join(mirror, "worktrees", filepath.Base(dir))
	if _, err := g.mirrorRun(ctx, mirror, nil, "worktree", "prune"); err != nil {
		return "", "", err
	}
	if exists(dir) {
		head, _ := os.ReadFile(filepath.Join(admin, "HEAD")) //nolint:gosec // G304: our cache
		if strings.TrimSpace(string(head)) != "ref: refs/heads/"+branch || !g.owns(admin, dir) {
			return "", "", fmt.Errorf("%s exists and is not the worktree of %s", dir, branch)
		}
	} else {
		if err := secfile.EnsureDir(filepath.Dir(dir)); err != nil {
			return "", "", err
		}
		_, err := g.mirrorRun(ctx, mirror, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+branch)
		if err == nil {
			_, err = g.mirrorRun(ctx, mirror, nil, "worktree", "add", "--quiet", "--end-of-options", dir, branch)
		} else {
			_, err = g.mirrorRun(ctx, mirror, nil, "worktree", "add", "--quiet", "--no-track", "-b", branch,
				"--end-of-options", dir, baseSHA)
		}
		if err != nil {
			return "", "", err
		}
		if !g.owns(admin, dir) {
			_ = g.RemoveWorktree(ctx, mirror, dir)
			return "", "", fmt.Errorf("git placed the worktree of %s elsewhere than %s", dir, admin)
		}
	}
	out, err := g.mirrorRun(ctx, mirror, nil, "merge-base", "--end-of-options", baseSHA, "refs/heads/"+branch)
	if err != nil {
		return "", "", err
	}
	fork := strings.TrimSpace(string(out))
	return fork, admin, checkSHA(fork)
}

// owns reports that admin is dir's administrative directory.
func (g *Git) owns(admin, dir string) bool {
	b, err := os.ReadFile(filepath.Join(admin, "gitdir")) //nolint:gosec // G304: our cache
	return err == nil && strings.TrimSpace(string(b)) == filepath.Join(dir, ".git")
}

// Snapshot implements app.Git.
func (g *Git) Snapshot(ctx context.Context, admin, dir, message string) (string, error) {
	if _, err := g.worktreeRun(ctx, admin, dir, "add", "--all"); err != nil {
		return "", err
	}
	if _, err := g.worktreeRun(ctx, admin, dir, "diff", "--cached", "--quiet", "--no-ext-diff"); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return "", err
		}
		if _, err := g.worktreeRun(ctx, admin, dir, "-c", "user.name="+CommitterName, "-c", "user.email="+CommitterEmail,
			"commit", "--quiet", "--no-verify", "--no-gpg-sign", "-m", message); err != nil {
			return "", err
		}
	}
	return g.Head(ctx, admin)
}

// Head implements app.Git: the worktree's HEAD commit (nothing is staged or committed).
func (g *Git) Head(ctx context.Context, admin string) (string, error) {
	out, err := g.worktreeRun(ctx, admin, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(out))
	return head, checkSHA(head)
}

// Diff implements app.Git.
func (g *Git) Diff(ctx context.Context, admin, from, to string) ([]domain.Change, error) {
	if err := errors.Join(checkSHA(from), checkSHA(to)); err != nil {
		return nil, err
	}
	out, err := g.worktreeRun(ctx, admin, "", "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff",
		"--no-textconv", "--end-of-options", from, to)
	if err != nil {
		return nil, err
	}
	return domain.ParseNameStatus(out)
}

// Patch implements app.Git: the unified diff from..to (commits) for a reviewer (KIN-35), at most limit bytes (cut
// reports more). No external diff or textconv driver runs (repository config is untrusted).
func (g *Git) Patch(ctx context.Context, admin, from, to string, limit int) (string, bool, error) {
	if err := errors.Join(checkSHA(from), checkSHA(to)); err != nil {
		return "", false, err
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var full []string
	for _, c := range g.config() {
		full = append(full, "-c", c)
	}
	full = append(full, "diff", "--no-renames", "--no-ext-diff", "--no-textconv", "--no-color", "--end-of-options", from, to)
	cmd := exec.CommandContext(ctx, g.program(), full...) //nolint:gosec // G204: fixed program, validated arguments
	cmd.Env = environ("GIT_DIR=" + admin)
	stdout, stderr := &limited{max: limit}, &limited{max: 2048}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stdout.over, nil
}

// Changes implements app.Git: the worktree against base (tracked paths, the agent's commits included) plus the
// untracked files a snapshot would add; read-only (nothing is staged or committed).
func (g *Git) Changes(ctx context.Context, admin, dir, base string) ([]domain.Change, error) {
	if err := checkSHA(base); err != nil {
		return nil, err
	}
	// The agent may run git in the worktree meanwhile: never take the index lock to refresh it.
	env := []string{"GIT_DIR=" + admin, "GIT_WORK_TREE=" + dir, "GIT_OPTIONAL_LOCKS=0"}
	out, err := g.run(ctx, env, nil, "diff", "--name-status", "-z", "--no-renames", "--no-ext-diff",
		"--no-textconv", "--end-of-options", base, "--")
	if err != nil {
		return nil, err
	}
	tracked, err := domain.ParseNameStatus(out)
	if err != nil {
		return nil, err
	}
	out, err = g.run(ctx, env, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	return domain.MergeChanges(tracked, domain.ParseUntracked(out)), nil
}

// RemoveWorktree implements app.Git: git removes the worktree; if it cannot (a corrupted worktree), the
// directory is deleted without following symlinks and git forgets it.
func (g *Git) RemoveWorktree(ctx context.Context, mirror, dir string) error {
	if exists(dir) {
		if _, err := g.mirrorRun(ctx, mirror, nil, "worktree", "remove", "--force", "--force", "--end-of-options", dir); err != nil {
			if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	_, err := g.mirrorRun(ctx, mirror, nil, "worktree", "prune")
	return err
}
