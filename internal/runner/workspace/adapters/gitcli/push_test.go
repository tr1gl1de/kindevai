package gitcli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/workspace/app"
	"github.com/tr1gl1de/kindevai/internal/runner/workspace/domain"
)

// workOn prepares the step's branch in the cache with one commit and returns its head.
func workOn(t *testing.T, e env, branch string) string {
	t.Helper()
	ctx := t.Context()
	sha, err := e.git.FetchBase(ctx, e.mirror, "acme/api", "dev", app.Token{})
	if err != nil {
		t.Fatal(err)
	}
	_, admin, err := e.git.AddWorktree(ctx, e.mirror, e.wt, branch, sha)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.wt, "README.md"), "changed\n")
	head, err := e.git.Snapshot(ctx, admin, e.wt, "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.git.RemoveWorktree(ctx, e.mirror, e.wt); err != nil {
		t.Fatal(err)
	}
	return head
}

// KIN-36: the reviewed commit is pushed to the step's feature branch of the repository; a repeated push of the
// same commit is a no-op.
func TestPushPublishesTheReviewedCommitToTheFeatureBranch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	head := workOn(t, e, "feature/KIN-1--s1")
	remote := strings.TrimPrefix(e.git.URL("acme/api"), "file://")
	for range 2 {
		if err := e.git.Push(t.Context(), e.mirror, "acme/api", "feature/KIN-1--s1", head, app.Token{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := sh(t, remote, "rev-parse", "refs/heads/feature/KIN-1--s1"); got != head {
		t.Fatalf("remote branch at %s, want %s", got, head)
	}
	if got := sh(t, remote, "rev-parse", "refs/heads/dev"); got != e.base {
		t.Fatal("the base moved")
	}
}

// KIN-36: the push target is checked in the application path: main/master, a non-feature branch, a bad commit id or
// repository is refused before git runs, and a non-fast-forward update is rejected (never forced).
func TestPushRefusesProtectedTargetsAndForcing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	head := workOn(t, e, "feature/KIN-1--s1")
	ctx := t.Context()
	for _, b := range []string{"main", "master", "refs/heads/main", "MAIN"} {
		if err := e.git.Push(ctx, e.mirror, "acme/api", b, head, app.Token{}); !errors.Is(err, domain.ErrProtectedBranch) {
			t.Errorf("%s: %v", b, err)
		}
	}
	for _, c := range [][3]string{{"acme/api", "dev", head}, {"acme/api", "feature/x", "HEAD"}, {"../x", "feature/x", head},
		{"acme/api", "feature/a..b", head}} {
		if err := e.git.Push(ctx, e.mirror, c[0], c[1], c[2], app.Token{}); err == nil {
			t.Errorf("%v pushed", c)
		}
	}
	// The remote branch moved on (someone else pushed): the platform never overwrites it.
	if err := e.git.Push(ctx, e.mirror, "acme/api", "feature/KIN-1--s2", head, app.Token{}); err != nil {
		t.Fatal(err)
	}
	if err := e.git.Push(ctx, e.mirror, "acme/api", "feature/KIN-1--s2", e.base, app.Token{}); err == nil {
		t.Fatal("a non-fast-forward push was accepted")
	}
}
