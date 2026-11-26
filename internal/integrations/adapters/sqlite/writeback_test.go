package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
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
	// KIN-117: a write skipped as superseded published nothing, so the next real publication still closes the alert.
	save("fail2", "t3", domain.WriteFailed)
	save("skipped", "t3", domain.WritePending)
	if err := r.MarkIssueWrite(ctx, "skipped", domain.WritePublished, app.SupersededCode, 1, t0); err != nil {
		t.Fatal(err)
	}
	save("after", "t3", domain.WritePublished)
	for key, want := range map[string]bool{"before": false, "next": true, "later": false, "other": false, "nope": false,
		"after": true} {
		if got, err := r.FollowsFailure(ctx, key); err != nil || got != want {
			t.Errorf("FollowsFailure(%s) = %v, %v; want %v", key, got, err, want)
		}
	}
}

// KIN-108: the outbox hooks move a write only from the status they expect; an empty code keeps the last error.
func TestMoveIssueWrite(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	key := "task/t1/v4/final"
	if _, err := r.MoveIssueWrite(ctx, key, domain.WritePending, domain.WriteFailed, "dead", 10, t0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := r.SaveIssueWrite(ctx, domain.IssueWrite{Key: key, TaskID: "t1", IssueID: "KIN-7", Kind: domain.WriteComment,
		CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		from, to domain.WriteStatus
		code     string
		moved    bool
		want     domain.WriteStatus
		wantCode string
	}{
		{domain.WritePending, domain.WriteFailed, "dead", true, domain.WriteFailed, "dead"},
		{domain.WritePending, domain.WriteFailed, "dead", false, domain.WriteFailed, "dead"}, // already failed
		{domain.WriteFailed, domain.WritePending, "", true, domain.WritePending, "dead"},     // retried: error kept
	}
	for i, s := range steps {
		moved, err := r.MoveIssueWrite(ctx, key, s.from, s.to, s.code, 10, t0.Add(time.Minute))
		if err != nil || moved != s.moved {
			t.Fatalf("step %d: moved = %v, %v", i, moved, err)
		}
		w, err := r.IssueWrite(ctx, key)
		if err != nil || w.Status != s.want || w.Error != s.wantCode || w.Attempts != 10 {
			t.Fatalf("step %d: write = %+v, %v", i, w, err)
		}
	}
}

// KIN-117: a state write is superseded by a later state write of its issue, an article write by a later write of the
// same article section (any task); a write is overtaken once a later write of its issue was published (a superseded
// one does not count). The task's publication counts superseded writes apart from the published ones.
func TestNewerWrites(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	save := func(key, issue string, kind domain.WriteKind, target string, s domain.WriteStatus, code string) {
		t.Helper()
		if err := r.SaveIssueWrite(ctx, domain.IssueWrite{Key: key, TaskID: "t1", IssueID: issue, Kind: kind,
			Target: target, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if s != domain.WritePending {
			if err := r.MarkIssueWrite(ctx, key, s, code, 1, t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	save("state1", "KIN-7", domain.WriteState, "", domain.WriteFailed, "dead")
	save("comment1", "KIN-7", domain.WriteComment, "", domain.WritePending, "")
	save("artS1", "KIN-7", domain.WriteArticle, "KIN-A-1#S", domain.WritePending, "")
	save("artT", "KIN-7", domain.WriteArticle, "KIN-A-1#T", domain.WritePending, "")
	save("state8", "KIN-8", domain.WriteState, "", domain.WritePublished, "")
	save("artS2", "KIN-8", domain.WriteArticle, "KIN-A-1#S", domain.WritePublished, app.SupersededCode)
	save("state2", "KIN-7", domain.WriteState, "", domain.WritePending, "")
	save("comment2", "KIN-7", domain.WriteComment, "", domain.WritePublished, "")
	save("comment9", "KIN-9", domain.WriteComment, "", domain.WritePending, "")
	save("art9", "KIN-9", domain.WriteArticle, "KIN-A-2#S", domain.WritePublished, "") // ordered apart from the issue
	// Written before KIN-117, an article write's target has no section: it is never superseded.
	save("legacy1", "KIN-9", domain.WriteArticle, "KIN-A-3", domain.WritePending, "")
	save("legacy2", "KIN-9", domain.WriteArticle, "KIN-A-3", domain.WritePending, "")
	// A newer write that failed (rejected, given up) never reaches YouTrack: it supersedes nothing.
	save("art4a", "KIN-9", domain.WriteArticle, "KIN-A-4#S", domain.WritePending, "")
	save("art4b", "KIN-9", domain.WriteArticle, "KIN-A-4#S", domain.WriteFailed, "invalid_content")
	for key, want := range map[string][2]bool{
		"state1": {true, true}, "comment1": {false, true}, "artS1": {true, true}, "artT": {false, true},
		"state8": {false, false}, "state2": {false, true}, "comment2": {false, false}, "comment9": {false, false}, "legacy1": {false, false}, "art4a": {false, false},
		"nope": {false, false},
	} {
		sup, over, err := r.NewerWrites(ctx, key)
		if err != nil || sup != want[0] || over != want[1] {
			t.Errorf("NewerWrites(%s) = %v %v %v, want %v", key, sup, over, err, want)
		}
	}
	if p, _ := r.TaskPublication(ctx, "t1"); p.Superseded != 1 || p.Published != 3 || p.Pending != 8 || p.Failed != 2 {
		t.Fatalf("publication = %+v", p)
	}
}
