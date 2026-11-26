package db

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// KIN-36: migration 0033 replaces workflow_steps.phase (its CHECK now allows 'publish') on a database with steps:
// their phases are kept and 'publish' can be stored afterwards; other values are still refused.
func TestMigration0033KeepsStepPhases(t *testing.T) {
	t.Parallel()
	d := openTemp(t)
	ctx := t.Context()
	upTo, through := fstest.MapFS{}, fstest.MapFS{} // later migrations check foreign keys the fake steps lack
	all := Migrations()
	names, err := fs.Glob(all, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		b, err := fs.ReadFile(all, n)
		if err != nil {
			t.Fatal(err)
		}
		if n < "0033" {
			upTo[n] = &fstest.MapFile{Data: b}
		}
		if n < "0034" {
			through[n] = &fstest.MapFile{Data: b}
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
		"PRAGMA foreign_keys = OFF", // only the steps matter here
		"INSERT INTO workflow_steps(id, execution_id, position, step_key, title, repository, state, phase) VALUES ('s1', 'e1', 0, 'a', 'A', 'r', 'READY', '')",
		"INSERT INTO workflow_steps(id, execution_id, position, step_key, title, repository, state, phase) VALUES ('s2', 'e1', 1, 'b', 'B', 'r', 'READY', 'review')",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = c.Close()
	if _, err := Migrate(ctx, d, through); err != nil {
		t.Fatal(err)
	}
	var phases []string
	for _, id := range []string{"s1", "s2"} {
		var p string
		if err := d.QueryRowContext(ctx, "SELECT phase FROM workflow_steps WHERE id = ?", id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		phases = append(phases, p)
	}
	if strings.Join(phases, ",") != ",review" {
		t.Fatalf("phases %q", phases)
	}
	if _, err := d.ExecContext(ctx, "UPDATE workflow_steps SET phase = 'publish' WHERE id = 's1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "UPDATE workflow_steps SET phase = 'deploy' WHERE id = 's1'"); err == nil {
		t.Fatal("an unknown phase was stored")
	}
}
