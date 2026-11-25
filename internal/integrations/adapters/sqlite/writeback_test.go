package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// KIN-38: a binding's state map is empty by default (comments only), saved per project.
func TestBindingStateMap(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	if err := r.SetStateMap(ctx, "p1", domain.StateMap{"RUNNING": "In Progress"}, t0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unbound: %v", err)
	}
	if _, err := r.StateMap(ctx, "p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unbound read: %v", err)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "KIN", t0); err != nil {
		t.Fatal(err)
	}
	if m, err := r.StateMap(ctx, "p1"); err != nil || len(m) != 0 {
		t.Fatalf("default = %v, %v", m, err)
	}
	if err := r.SetStateMap(ctx, "p1", domain.StateMap{"RUNNING": "In Progress", "COMPLETED": "Fixed"}, t0); err != nil {
		t.Fatal(err)
	}
	if m, err := r.StateMap(ctx, "p1"); err != nil || len(m) != 2 || m["COMPLETED"] != "Fixed" {
		t.Fatalf("saved = %v, %v", m, err)
	}
}

// KIN-38 AC5: writes are tracked per task; a repeated save of a key keeps the first (outbox idempotency), and the
// task's publication sums pending and failed writes with the newest error.
func TestIssueWritesAndPublication(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	w := domain.IssueWrite{Key: "task/t1/v2/plan", TaskID: "t1", IssueID: "KIN-7", Kind: domain.WriteComment, CreatedAt: t0}
	for range 2 {
		if err := r.SaveIssueWrite(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	art := domain.IssueWrite{Key: "task/t1/v9/article/KIN-A-1", TaskID: "t1", IssueID: "KIN-7", Kind: domain.WriteArticle,
		Target: "KIN-A-1", CreatedAt: t0}
	if err := r.SaveIssueWrite(ctx, art); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveIssueWrite(ctx, domain.IssueWrite{Key: "task/t2/v1/final", TaskID: "t2", IssueID: "KIN-8",
		Kind: domain.WriteComment, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	p, err := r.TaskPublication(ctx, "t1")
	if err != nil || p != (domain.Publication{Pending: 2}) {
		t.Fatalf("publication = %+v, %v", p, err)
	}
	if err := r.MarkIssueWrite(ctx, w.Key, domain.WritePending, "http_503", 1, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.TaskPublication(ctx, "t1"); p != (domain.Publication{Pending: 2, LastError: "http_503"}) {
		t.Fatalf("retrying = %+v", p)
	}
	if err := r.MarkIssueWrite(ctx, w.Key, domain.WritePublished, "", 2, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkIssueWrite(ctx, art.Key, domain.WriteFailed, "conflict", 1, t0.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.TaskPublication(ctx, "t1"); p != (domain.Publication{Failed: 1, Published: 1, LastError: "conflict"}) {
		t.Fatalf("after = %+v", p)
	}
	if got, err := r.IssueWrite(ctx, art.Key); err != nil || got.Status != domain.WriteFailed || got.Target != "KIN-A-1" ||
		got.Attempts != 1 {
		t.Fatalf("write = %+v, %v", got, err)
	}
	if err := r.MarkIssueWrite(ctx, "nope", domain.WritePublished, "", 1, t0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if p, _ := r.TaskPublication(ctx, "t9"); p != (domain.Publication{}) {
		t.Fatalf("no writes = %+v", p)
	}
}

// KIN-107: only the first write published after the task's latest failure (in enqueue order) closes its alert.
func TestFollowsFailure(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	save := func(key, task string, s domain.WriteStatus) {
		t.Helper()
		if err := r.SaveIssueWrite(ctx, domain.IssueWrite{Key: key, TaskID: task, IssueID: "KIN-7", Kind: domain.WriteComment,
			CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if s != domain.WritePending {
			if err := r.MarkIssueWrite(ctx, key, s, "", 1, t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	save("before", "t1", domain.WritePublished) // enqueued before the failure
	save("fail", "t1", domain.WriteFailed)
	save("other", "t2", domain.WritePublished) // another task's write does not count
	save("next", "t1", domain.WritePublished)
	save("later", "t1", domain.WritePublished)
	for key, want := range map[string]bool{"before": false, "next": true, "later": false, "other": false, "nope": false} {
		if got, err := r.FollowsFailure(ctx, key); err != nil || got != want {
			t.Errorf("FollowsFailure(%s) = %v, %v; want %v", key, got, err, want)
		}
	}
}
