package sqlite_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// stepClock says when each step's execution finished; listed in running, it still runs.
type stepClock struct {
	finished map[string]time.Time
	running  map[string]bool
}

func (s stepClock) FinishedSteps(_ context.Context, ids []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, id := range ids {
		if !s.running[id] {
			out[id] = s.finished[id]
		}
	}
	return out, nil
}

// recordAttempt stores an attempt as the channel does: accepted, started, outputs of size bytes (counted), a
// rejected output without body, completed; all received at.
func recordAttempt(t *testing.T, d *db.Transactor, r *sqlite.Repository, step string, outputs int, size int, at time.Time) {
	t.Helper()
	seq := int64(0)
	rec := func(kind string, body []byte, outcome string) {
		seq++
		e := domain.AttemptEvent{RunnerID: "r1", Step: step, Fence: 1, AttemptSeq: seq, Kind: kind, Body: body,
			Outcome: outcome, RunnerAt: at, ReceivedAt: at}
		if err := d.InTx(t.Context(), func(ctx context.Context) error {
			if body != nil {
				if v, err := r.AddOutput(ctx, step, 1, int64(len(body)), domain.OutputLimits{Attempt: domain.MaxAttemptOutput, Total: 1 << 40}, at); err != nil || v != domain.OutputStored {
					return fmt.Errorf("output %v %w", v, err)
				}
			}
			return r.RecordEvent(ctx, e)
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec(protocol.EventAccepted, nil, domain.OutcomeApplied)
	rec(protocol.EventStarted, nil, domain.OutcomeApplied)
	for range outputs {
		rec(protocol.EventOutput, []byte(strings.Repeat("x", size)), domain.OutcomeApplied)
	}
	rec(protocol.EventOutput, nil, domain.OutcomeRejected)
	rec(protocol.EventCompleted, nil, domain.OutcomeApplied)
}

func eventCounts(t *testing.T, tx *db.Transactor, step string) (output, other int) {
	t.Helper()
	if err := tx.DB.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE kind = ? AND body IS NOT NULL),
		count(*) FILTER (WHERE NOT (kind = ? AND body IS NOT NULL)) FROM runner_attempt_events WHERE step_id = ?`,
		protocol.EventOutput, protocol.EventOutput, step).Scan(&output, &other); err != nil {
		t.Fatal(err)
	}
	return output, other
}

// The retention pass on a real database: expired output and events of finished executions go in batches, running
// executions keep everything, and the output counters follow.
func TestRetentionOnSQLite(t *testing.T) {
	t.Parallel()
	d, r := withRunner(t)
	tx := &db.Transactor{DB: d}
	now := t0.Add(60 * domain.Day)
	old := t0
	recordAttempt(t, tx, r, "running", 3, 100, old)
	recordAttempt(t, tx, r, "fresh", 3, 100, old)
	recordAttempt(t, tx, r, "output", 300, 100, old) // more than one delete batch
	recordAttempt(t, tx, r, "expired", 3, 100, old)
	recordAttempt(t, tx, r, "recent", 3, 100, now.Add(-domain.Day))
	steps := stepClock{finished: map[string]time.Time{
		"fresh": now.Add(-13 * domain.Day), "output": now.Add(-15 * domain.Day), "expired": now.Add(-31 * domain.Day),
		"recent": now.Add(-40 * domain.Day), // events are new even if the execution is long done (clock skew)
	}, running: map[string]bool{"running": true}}
	ret := &app.Retention{Repo: r, Steps: steps, Policy: domain.DefaultRetention(), Now: func() time.Time { return now }}
	rep, err := ret.Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for step, want := range map[string][2]int{
		"running": {3, 4}, "fresh": {3, 4}, "output": {0, 3}, "expired": {0, 0}, "recent": {3, 4},
	} {
		if o, e := eventCounts(t, tx, step); o != want[0] || e != want[1] {
			t.Errorf("%s: %d output, %d other events; want %v", step, o, e, want)
		}
	}
	if rep.OutputAttempts != 2 || rep.OutputBytes != 30300 || rep.Events != 3 {
		t.Errorf("report %+v", rep)
	}
	if used, err := r.OutputUsage(t.Context()); used != 900 || err != nil {
		t.Errorf("usage %d %v", used, err)
	}
	var counters int
	_ = d.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_attempt_output`).Scan(&counters)
	if counters != 3 {
		t.Errorf("%d attempt counters left", counters)
	}
	// A second pass finds nothing more.
	if rep, err := ret.Purge(t.Context()); rep != (app.RetentionReport{}) || err != nil {
		t.Errorf("second pass %+v %v", rep, err)
	}
}

// Keyset pages over the old events and output attempts never repeat or skip a row.
func TestRetentionPages(t *testing.T) {
	t.Parallel()
	d, r := withRunner(t)
	tx := &db.Transactor{DB: d}
	for i := range 5 {
		recordAttempt(t, tx, r, fmt.Sprint("s", i), 1, 10, t0.Add(time.Duration(i%2)*time.Minute))
	}
	seen := map[int64]bool{}
	var cur domain.EventRef
	for {
		page, err := r.OldEvents(t.Context(), t0.Add(time.Hour), cur, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			if seen[e.ID] {
				t.Fatalf("event %d listed twice", e.ID)
			}
			seen[e.ID] = true
		}
		cur = page[len(page)-1]
	}
	if len(seen) != 5*4 { // accepted, started, rejected output, completed: counted output is not listed
		t.Fatalf("listed %d events", len(seen))
	}
	var atts []domain.AttemptOutput
	var acur domain.AttemptOutput
	for {
		page, err := r.OutputAttempts(t.Context(), t0.Add(time.Hour), acur, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		atts = append(atts, page...)
		acur = page[len(page)-1]
	}
	if len(atts) != 5 || atts[0].Bytes != 10 || !atts[0].LastAt.Equal(t0) || !atts[4].LastAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("attempts %+v", atts)
	}
	if page, _ := r.OutputAttempts(t.Context(), t0, domain.AttemptOutput{}, 10); len(page) != 0 {
		t.Fatalf("attempts before their last output listed: %+v", page)
	}
}

// KIN-59: output dropped for the total limit is listed per attempt, newest first, within the window; other drops
// (the per-attempt cap, rejected events) and older ones are not.
func TestOutputDropsPerAttempt(t *testing.T) {
	t.Parallel()
	_, r := withRunner(t)
	seq := int64(0)
	rec := func(step, reason string, at time.Time) {
		t.Helper()
		seq++
		if err := r.RecordEvent(t.Context(), domain.AttemptEvent{RunnerID: "r1", Step: step, Fence: 2, AttemptSeq: seq,
			Kind: protocol.EventOutput, Outcome: domain.OutcomeApplied, Reason: reason, RunnerAt: at, ReceivedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	since := t0.Add(-domain.OutputDropsWindow)
	rec("a", "output_storage_limit", t0.Add(-time.Hour))
	rec("a", "output_storage_limit", t0.Add(-30*time.Minute))
	rec("b", "output_storage_limit", t0.Add(-10*time.Minute))
	rec("c", "output_storage_limit", since.Add(-time.Second)) // too old
	rec("d", "output_limit", t0)                              // the attempt's own cap
	rec("e", "", t0)
	drops, total, err := r.OutputDrops(t.Context(), since, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.OutputDrop{
		{RunnerID: "r1", Step: "b", Fence: 2, Events: 1, LastAt: t0.Add(-10 * time.Minute)},
		{RunnerID: "r1", Step: "a", Fence: 2, Events: 2, LastAt: t0.Add(-30 * time.Minute)},
	}
	if total != 3 || len(drops) != len(want) {
		t.Fatalf("total %d, drops %+v", total, drops)
	}
	for i := range want {
		if drops[i].RunnerID != want[i].RunnerID || drops[i].Step != want[i].Step || drops[i].Fence != want[i].Fence ||
			drops[i].Events != want[i].Events || !drops[i].LastAt.Equal(want[i].LastAt) {
			t.Errorf("drop %d = %+v, want %+v", i, drops[i], want[i])
		}
	}
	if drops, total, err := r.OutputDrops(t.Context(), since, 1); err != nil || total != 3 || len(drops) != 1 || drops[0].Step != "b" {
		t.Errorf("limited: %+v %d %v", drops, total, err)
	}
}

// addRunner enrolls runner id and brings it to status (approved, disabled or revoked) with n commands in its log,
// the first acknowledged.
func addRunner(t *testing.T, d *db.Transactor, r *sqlite.Repository, id string, status domain.Status, n int) {
	t.Helper()
	ctx := t.Context()
	tok, _, _ := domain.NewEnrollmentToken(rand.Reader, "tok-"+id, "owner1", t0)
	if err := r.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	key, _ := newKey(t)
	x := domain.Enroll(id, tok, key, domain.Metadata{Hostname: "h", OS: "linux", Arch: "amd64"}, t0)
	if err := r.CreateRunner(ctx, x); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureChannel(ctx, id, "ep-"+id); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := d.InTx(ctx, func(ctx context.Context) error {
			_, err := r.AppendCommand(ctx, id, domain.QueuedCommand{ID: fmt.Sprint("c", i), Kind: protocol.CommandConfigUpdate,
				Body: []byte(`{"sealed":"secret"}`)}, t0)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.AckCommands(ctx, id, 1, t0); err != nil {
		t.Fatal(err)
	}
	if err := x.Approve([]string{"p1"}, "owner1", t0); err != nil {
		t.Fatal(err)
	}
	switch status {
	case domain.StatusDisabled:
		if err := x.Disable(t0); err != nil {
			t.Fatal(err)
		}
	case domain.StatusRevoked:
		if err := x.Revoke("owner1", t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveRunner(ctx, x); err != nil {
		t.Fatal(err)
	}
}

func commandCount(t *testing.T, d *db.Transactor, runnerID string) int {
	t.Helper()
	var n int
	if err := d.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_commands WHERE runner_id = ?`, runnerID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// KIN-72: the retention empties the command logs of revoked runners (acknowledged or not, sealed proxy passwords
// included) in batches; logs of approved and disabled runners stay as they are.
func TestRetentionPurgesRevokedRunnerCommands(t *testing.T) {
	t.Parallel()
	d, r := withRunner(t)
	tx := &db.Transactor{DB: d}
	addRunner(t, tx, r, "live", domain.StatusApproved, 3)
	addRunner(t, tx, r, "paused", domain.StatusDisabled, 3)
	addRunner(t, tx, r, "gone", domain.StatusRevoked, app.RetentionBatch+5) // more than one delete batch
	addRunner(t, tx, r, "gone2", domain.StatusRevoked, 2)
	ret := &app.Retention{Repo: r, Steps: stepClock{}, Policy: domain.DefaultRetention(), Now: func() time.Time { return t0 }}
	rep, err := ret.Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{"live": 3, "paused": 3, "gone": 0, "gone2": 0} {
		if got := commandCount(t, tx, id); got != want {
			t.Errorf("%s: %d commands left, want %d", id, got, want)
		}
	}
	if rep.RevokedCommands != app.RetentionBatch+7 {
		t.Errorf("report %+v", rep)
	}
	if rep, err := ret.Purge(t.Context()); rep != (app.RetentionReport{}) || err != nil {
		t.Errorf("second pass %+v %v", rep, err)
	}
}
