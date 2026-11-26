package db

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// KIN-37: migration 0034 rebuilds tasks so a task may be BLOCKED with reason 'ci'; existing tasks are kept, the
// task's dependants still reference it, and an unknown reason is still refused.
func TestMigration0034KeepsTasksAndAllowsTheCIBlockReason(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	ctx := t.Context()
	upTo := fstest.MapFS{}
	all := Migrations()
	names, err := fs.Glob(all, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n < "0034" {
			b, err := fs.ReadFile(all, n)
			if err != nil {
				t.Fatal(err)
			}
			upTo[n] = &fstest.MapFile{Data: b}
		}
	}
	if _, err := Migrate(ctx, d, upTo); err != nil {
		t.Fatal(err)
	}
	c, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, q := range []string{
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
		`INSERT INTO tasks(id, project_id, title, state, block_reason, blocked_from, created_at, updated_at, version)
		 VALUES ('t1', 'p1', 'Blocked', 'BLOCKED', 'runner', 'RUNNING', 'x', 'x', 3)`,
		`INSERT INTO tasks(id, project_id, title, youtrack_issue, state, created_at, updated_at, version)
		 VALUES ('t2', 'p1', 'Verifying', 'KIN-9', 'VERIFYING', 'x', 'x', 1)`,
	} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = c.Close()
	if _, err := Migrate(ctx, d, all); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE (id = 't1' AND block_reason = 'runner'
		AND version = 3) OR (id = 't2' AND youtrack_issue = 'KIN-9')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("kept %d tasks: %v", n, err)
	}
	if _, err := d.ExecContext(ctx, "UPDATE tasks SET block_reason = 'ci' WHERE id = 't1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "UPDATE tasks SET block_reason = 'weather' WHERE id = 't1'"); err == nil {
		t.Fatal("an unknown block reason was stored")
	}
	if _, err := d.ExecContext(ctx, "UPDATE change_set_artifacts SET merge_state = 'merged'"); err != nil {
		t.Fatal(err)
	}
}
