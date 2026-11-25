package app_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

var adminPrincipal = auth.Principal{UserID: "adm1", Login: "adm"}

// KIN-85 AC: auto-planning is a per-project setting, off by default, changed by the Owner or the project's admin
// (members may not; outsiders learn nothing) and audited.
func TestAutoPlanSettingIsOffByDefaultAndManagedByOwnerOrProjectAdmin(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	ctx := t.Context()
	if b, _ := e.intake.Binding(ctx, "p1"); b.AutoPlan {
		t.Fatal("auto-planning on by default")
	}
	if _, err := e.intake.SetAutoPlan(ctx, alicePrincipal, "p1", true); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	if _, err := e.intake.SetAutoPlan(ctx, auth.Principal{UserID: "bob1"}, "p1", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	b, err := e.intake.SetAutoPlan(ctx, adminPrincipal, "p1", true)
	if err != nil || !b.AutoPlan {
		t.Fatalf("admin: %+v %v", b, err)
	}
	if b, _ := e.intake.Binding(ctx, "p1"); !b.AutoPlan {
		t.Fatal("not saved")
	}
	if _, err := e.intake.SetAutoPlan(ctx, owner, "p1", false); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := e.intake.SetAutoPlan(ctx, owner, "p2", true); !errors.Is(err, domain.ErrNotBound) {
		t.Fatalf("unbound: %v", err)
	}
	var changes []string
	for _, en := range e.audit.entries {
		if en.Kind == app.KindYouTrackAutoPlanChanged {
			changes = append(changes, en.Details["autoPlan"])
		}
	}
	if !slices.Equal(changes, []string{"true", "false"}) {
		t.Fatalf("audit %v", changes)
	}
	// Setting the same value again changes (and audits) nothing.
	if _, err := e.intake.SetAutoPlan(ctx, owner, "p1", false); err != nil {
		t.Fatal(err)
	}
	if n := countKind(e, app.KindYouTrackAutoPlanChanged); n != 2 {
		t.Fatalf("audited %d changes", n)
	}
}

func countKind(e *intakeEnv, kind string) int {
	n := 0
	for _, k := range e.audit.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// KIN-85 AC: with the setting on, a task the intake creates is handed to the planner in the same use case (on
// behalf of the initiator) and audited; a redelivered or changed issue never starts a second run; with the setting
// off nothing is planned.
func TestIntakeTaskIsPlannedAutomaticallyOnce(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.src.put(issue("KIN-1", 100, "jdoe"))
	e.poll(t)
	if len(e.tasks.planned) != 0 {
		t.Fatalf("planned with the setting off: %v", e.tasks.planned)
	}

	if _, err := e.intake.SetAutoPlan(t.Context(), owner, "p1", true); err != nil {
		t.Fatal(err)
	}
	e.now = t0.Add(time.Minute)
	e.src.put(issue("KIN-2", 200, "jdoe"))
	e.poll(t)
	if !slices.Equal(e.tasks.planned, []string{"task2/" + alice}) {
		t.Fatalf("planned %v", e.tasks.planned)
	}
	var started []map[string]string
	for _, en := range e.audit.entries {
		if en.Kind == app.KindYouTrackPlanningStarted {
			started = append(started, en.Details)
		}
	}
	if len(started) != 1 || started[0]["issue"] != "KIN-2" || started[0]["initiator"] != alice {
		t.Fatalf("audit %v", started)
	}

	// The same issue again (changed summary, redelivery) follows the task only.
	changed := issue("KIN-2", 300, "jdoe")
	changed.Summary = "Renamed"
	e.src.put(changed)
	e.poll(t)
	e.src.put(issue("KIN-1", 400, "jdoe")) // created before the setting: not planned later either
	e.poll(t)
	if len(e.tasks.planned) != 1 {
		t.Fatalf("planned again: %v", e.tasks.planned)
	}
}

// KIN-85: a start the workflow declines (rate cap, planner off) leaves the task CREATED and is audited with the reason.
func TestDeclinedAutoPlanIsAudited(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.tasks.outcome = "rate_limited"
	if _, err := e.intake.SetAutoPlan(t.Context(), owner, "p1", true); err != nil {
		t.Fatal(err)
	}
	e.src.put(issue("KIN-1", 100, "jdoe"))
	e.poll(t)
	var reason string
	for _, en := range e.audit.entries {
		if en.Kind == app.KindYouTrackPlanningSkipped {
			reason = en.Details["reason"]
		}
	}
	if reason != "rate_limited" || countKind(e, app.KindYouTrackPlanningStarted) != 0 {
		t.Fatalf("audit %v", e.audit.kinds())
	}
	if r := e.record(t, "p1", "KIN-1"); r.Outcome != domain.OutcomeCreated {
		t.Fatalf("record %+v", r)
	}
}

// KIN-38 AC2: the platform's own writes (a comment, a state change, even resolving the issue) change the issue's
// `updated` and come back through the webhook and the poller; they never create a second task or plan again.
func TestOwnWriteBackDoesNotLoopIntoNewTasks(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	ctx := t.Context()
	if _, err := e.intake.SetAutoPlan(ctx, owner, "p1", true); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 40)
	if err := e.intake.SetWebhookSecret(ctx, owner, secret); err != nil {
		t.Fatal(err)
	}
	e.src.put(issue("KIN-5", 100, "jdoe"))
	e.poll(t)
	if len(e.tasks.titles) != 1 || len(e.tasks.planned) != 1 {
		t.Fatalf("setup: tasks %v planned %v", e.tasks.titles, e.tasks.planned)
	}
	for i, updated := range []int64{200, 300, 400} { // plan comment, state change, final comment + resolved
		after := issue("KIN-5", updated, "jdoe")
		after.Resolved = i == 2
		e.src.put(after)
		body := []byte(fmt.Sprintf(`{"issueId":"KIN-5","updated":%d}`, updated))
		if err := e.intake.ReceiveWebhook(ctx, secret, body); err != nil {
			t.Fatal(err)
		}
		e.drain(t)
		e.deliver(t)
		e.drain(t)
		e.poll(t)
	}
	if len(e.tasks.titles) != 1 || len(e.tasks.planned) != 1 || countKind(e, app.KindYouTrackTaskCreated) != 1 {
		t.Fatalf("write-back looped: tasks %v planned %v", e.tasks.titles, e.tasks.planned)
	}
	if r := e.record(t, "p1", "KIN-5"); r.Outcome != domain.OutcomeUnchanged || r.IssueUpdated != 400 {
		t.Fatalf("record %+v", r)
	}
}
