package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/projects/app"
)

type auditRow struct {
	actorID, actorLabel, kind, projectID, subject, details string
}

func auditRows(t *testing.T, d *sql.DB) []auditRow {
	t.Helper()
	rows, err := d.QueryContext(t.Context(), `SELECT coalesce(actor_id, ''), actor_label, kind, coalesce(project_id, ''),
		coalesce(subject, ''), coalesce(details, '') FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actorID, &r.actorLabel, &r.kind, &r.projectID, &r.subject, &r.details); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// KIN-46 AC: project.created, project.member_set (with the role) and project.member_removed are recorded.
func TestProjectChangesAreAudited(t *testing.T) {
	t.Parallel()
	h, d := stackWith(t, nil)
	_, p, _ := do(t, h, "owner", "POST", "/api/projects", `{"name":"Web","slug":"web"}`)
	id := p["id"].(string)
	do(t, h, "owner", "PUT", "/api/projects/"+id+"/members/alice", `{"role":"member"}`)
	do(t, h, "owner", "PUT", "/api/projects/"+id+"/members/alice", `{"role":"project_admin"}`)
	do(t, h, "owner", "DELETE", "/api/projects/"+id+"/members/alice", "")
	want := []auditRow{
		{"u-owner", "owner", "project.created", id, "project:" + id, `{"name":"Web","slug":"web"}`},
		{"u-owner", "owner", "project.member_set", id, "user:u-alice", `{"login":"alice","role":"member"}`},
		{"u-owner", "owner", "project.member_set", id, "user:u-alice", `{"login":"alice","role":"project_admin"}`},
		{"u-owner", "owner", "project.member_removed", id, "user:u-alice", `{"login":"alice"}`},
	}
	got := auditRows(t, d)
	if len(got) != len(want) {
		t.Fatalf("audit rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A rejected change records nothing: removing a non-member, a non-Owner adding a member.
	do(t, h, "owner", "DELETE", "/api/projects/"+id+"/members/bob", "")
	do(t, h, "bob", "PUT", "/api/projects/"+id+"/members/bob", `{"role":"member"}`)
	if n := len(auditRows(t, d)); n != len(want) {
		t.Fatalf("rejected changes were audited: %d rows", n)
	}
}

type failingAudit struct{}

func (failingAudit) Record(context.Context, auditapp.Entry) error { return errors.New("disk full") }

// KIN-46 AC: the change and its event commit together: when the event cannot be written, the change is rolled back.
func TestProjectChangeRollsBackWithoutAudit(t *testing.T) {
	t.Parallel()
	h, d := stackWith(t, func(*sql.DB) app.AuditLog { return failingAudit{} })
	if code, _, _ := do(t, h, "owner", "POST", "/api/projects", `{"name":"Web","slug":"web"}`); code != 500 {
		t.Fatalf("create with a broken audit log = %d, want 500", code)
	}
	var n int
	if err := d.QueryRowContext(t.Context(), "SELECT count(*) FROM projects").Scan(&n); err != nil || n != 0 {
		t.Fatalf("projects = %d, %v: the change outlived its failed audit event", n, err)
	}
}
