package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// memRow is one recorded event in memRetentionRepo.
type memRow struct {
	id       int64
	step     string
	fence    int64
	kind     string
	body     int64 // stored body bytes; -1 when the body was dropped
	received time.Time
}

// memRetentionRepo is an in-memory RetentionRepo that records the batch sizes it was asked for.
type memRetentionRepo struct {
	mu       sync.Mutex
	rows     []memRow
	attempts map[string]*domain.AttemptOutput // step/fence
	nextID   int64
	maxBatch int
	revoked  int64 // commands in the logs of revoked runners
}

func newMemRetentionRepo() *memRetentionRepo {
	return &memRetentionRepo{attempts: map[string]*domain.AttemptOutput{}}
}

func attKey(step string, fence int64) string { return fmt.Sprintf("%s/%d", step, fence) }

func (r *memRetentionRepo) add(step string, fence int64, kind string, body int64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.rows = append(r.rows, memRow{id: r.nextID, step: step, fence: fence, kind: kind, body: body, received: at})
	if isOutput(kind) && body >= 0 {
		a := r.attempts[attKey(step, fence)]
		if a == nil {
			a = &domain.AttemptOutput{Step: step, Fence: fence}
			r.attempts[attKey(step, fence)] = a
		}
		a.Bytes += body
		a.LastAt = at
	}
}

func isOutput(kind string) bool {
	return kind == protocol.EventOutput || kind == protocol.EventOutputDropped
}

func (r *memRetentionRepo) batch(n int) {
	r.maxBatch = max(r.maxBatch, n)
}

func (r *memRetentionRepo) OutputAttempts(_ context.Context, before time.Time, after domain.AttemptOutput, limit int) ([]domain.AttemptOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batch(limit)
	var out []domain.AttemptOutput
	for _, a := range r.attempts {
		if a.LastAt.Before(before) && attemptAfter(*a, after) {
			out = append(out, *a)
		}
	}
	slices.SortFunc(out, func(a, b domain.AttemptOutput) int {
		if c := a.LastAt.Compare(b.LastAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.Step, b.Step); c != 0 {
			return c
		}
		return int(a.Fence - b.Fence)
	})
	return out[:min(limit, len(out))], nil
}

func attemptAfter(a, cur domain.AttemptOutput) bool {
	if !a.LastAt.Equal(cur.LastAt) {
		return a.LastAt.After(cur.LastAt)
	}
	if a.Step != cur.Step {
		return a.Step > cur.Step
	}
	return a.Fence > cur.Fence
}

func (r *memRetentionRepo) DeleteAttemptOutput(_ context.Context, step string, fence int64, limit int) (int, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batch(limit)
	n := 0
	r.rows = slices.DeleteFunc(r.rows, func(row memRow) bool {
		if n < limit && row.step == step && row.fence == fence && isOutput(row.kind) {
			n++
			if row.body > 0 {
				r.attempts[attKey(step, fence)].Bytes -= row.body
			}
			return true
		}
		return false
	})
	if n < limit {
		delete(r.attempts, attKey(step, fence))
		return n, true, nil
	}
	return n, false, nil
}

func (r *memRetentionRepo) DeleteRevokedCommands(_ context.Context, limit int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batch(limit)
	n := min(r.revoked, int64(limit))
	r.revoked -= n
	return n, nil
}

func (r *memRetentionRepo) OldEvents(_ context.Context, before time.Time, after domain.EventRef, limit int) ([]domain.EventRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batch(limit)
	var out []domain.EventRef
	for _, row := range r.rows {
		counted := isOutput(row.kind) && row.body >= 0
		if counted || !row.received.Before(before) {
			continue
		}
		if row.received.After(after.ReceivedAt) || (row.received.Equal(after.ReceivedAt) && row.id > after.ID) {
			out = append(out, domain.EventRef{ID: row.id, Step: row.step, ReceivedAt: row.received})
		}
	}
	slices.SortFunc(out, func(a, b domain.EventRef) int {
		if c := a.ReceivedAt.Compare(b.ReceivedAt); c != 0 {
			return c
		}
		return int(a.ID - b.ID)
	})
	return out[:min(limit, len(out))], nil
}

func (r *memRetentionRepo) DeleteEvents(_ context.Context, refs []domain.EventRef) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batch(len(refs))
	ids := map[int64]bool{}
	for _, e := range refs {
		ids[e.ID] = true
	}
	before := len(r.rows)
	r.rows = slices.DeleteFunc(r.rows, func(row memRow) bool { return ids[row.id] })
	return int64(before - len(r.rows)), nil
}

func (r *memRetentionRepo) OutputUsage(context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, a := range r.attempts {
		n += a.Bytes
	}
	return n, nil
}

// kinds lists the kinds of the step's remaining events in order.
func (r *memRetentionRepo) kinds(step string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, row := range r.rows {
		if row.step == step {
			out = append(out, row.kind)
		}
	}
	return out
}

// fakeSteps knows when each step's execution finished; steps missing from both maps no longer exist.
type fakeSteps struct {
	finished map[string]time.Time
	running  map[string]bool
	asked    int
}

func (f *fakeSteps) FinishedSteps(_ context.Context, ids []string) (map[string]time.Time, error) {
	f.asked++
	out := map[string]time.Time{}
	for _, id := range ids {
		switch {
		case f.running[id]:
		case !f.finished[id].IsZero():
			out[id] = f.finished[id]
		default:
			out[id] = time.Time{}
		}
	}
	return out, nil
}

var retNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func days(n int) time.Duration { return time.Duration(n) * domain.Day }

// addAttempt records a whole attempt of step: accepted, started, n output chunks of size bytes, completed.
func addAttempt(r *memRetentionRepo, step string, at time.Time, n int, size int64) {
	r.add(step, 1, protocol.EventAccepted, -1, at)
	r.add(step, 1, protocol.EventStarted, -1, at)
	for range n {
		r.add(step, 1, protocol.EventOutput, size, at)
	}
	r.add(step, 1, protocol.EventCompleted, -1, at)
}

func newRetention(repo *memRetentionRepo, steps *fakeSteps, logs *bytes.Buffer) *Retention {
	return &Retention{Repo: repo, Steps: steps, Policy: domain.DefaultRetention(), Now: func() time.Time { return retNow },
		Logger: slog.New(slog.NewTextHandler(logs, nil))}
}

var lifecycle = []string{protocol.EventAccepted, protocol.EventStarted, protocol.EventCompleted}

// Output goes 14 days after its execution finished, lifecycle events after 30; records of a running execution
// (fencing, reconciliation) and of executions finished more recently stay.
func TestRetentionByExecutionFinish(t *testing.T) {
	t.Parallel()
	repo := newMemRetentionRepo()
	steps := &fakeSteps{finished: map[string]time.Time{
		"fresh":   retNow.Add(-days(13)),
		"output":  retNow.Add(-days(15)),
		"waited":  retNow.Add(-days(2)), // last output long ago, but the execution finished only recently
		"expired": retNow.Add(-days(31)),
	}, running: map[string]bool{"running": true}}
	for _, s := range []string{"fresh", "output", "expired", "running"} {
		addAttempt(repo, s, retNow.Add(-days(40)), 3, 100)
	}
	addAttempt(repo, "waited", retNow.Add(-days(40)), 3, 100)
	addAttempt(repo, "gone", retNow.Add(-days(40)), 3, 100) // its task was deleted: nothing to protect
	var logs bytes.Buffer
	rep, err := newRetention(repo, steps, &logs).Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	full := []string{protocol.EventAccepted, protocol.EventStarted, protocol.EventOutput, protocol.EventOutput,
		protocol.EventOutput, protocol.EventCompleted}
	for step, want := range map[string][]string{
		"fresh": full, "running": full, "waited": full,
		"output": lifecycle, "expired": nil, "gone": nil,
	} {
		if got := repo.kinds(step); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", step, got, want)
		}
	}
	if rep.OutputAttempts != 3 || rep.OutputBytes != 900 || rep.Events != 6 {
		t.Errorf("report %+v", rep)
	}
	if used, _ := repo.OutputUsage(t.Context()); used != 900 {
		t.Errorf("usage %d", used)
	}
	if !strings.Contains(logs.String(), "runner events purged") {
		t.Errorf("no summary logged:\n%s", logs.String())
	}
}

// Deletion runs in small batches, and a long run of protected records at the head does not stop the pass.
func TestRetentionBatchesPastProtectedRecords(t *testing.T) {
	t.Parallel()
	repo := newMemRetentionRepo()
	steps := &fakeSteps{finished: map[string]time.Time{}, running: map[string]bool{}}
	old := retNow.Add(-days(60))
	for i := range 300 {
		s := fmt.Sprintf("run%03d", i)
		steps.running[s] = true
		addAttempt(repo, s, old, 1, 10)
	}
	steps.finished["done"] = retNow.Add(-days(31))
	addAttempt(repo, "done", old.Add(time.Minute), 500, 10)
	rep, err := newRetention(repo, steps, &bytes.Buffer{}).Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := repo.kinds("done"); got != nil {
		t.Fatalf("finished step kept %d events", len(got))
	}
	if rep.Events != 3 || rep.OutputBytes != 5000 || len(repo.kinds("run299")) != 4 {
		t.Fatalf("report %+v", rep)
	}
	if repo.maxBatch > RetentionBatch {
		t.Fatalf("batch of %d rows", repo.maxBatch)
	}
}

// Over the output limit the oldest finished output goes first, regardless of age, until 90% of the limit; running
// attempts keep theirs, and when only they remain the shortage is logged.
func TestRetentionEvictsOldestFinishedOutputOverLimit(t *testing.T) {
	t.Parallel()
	repo := newMemRetentionRepo()
	steps := &fakeSteps{finished: map[string]time.Time{}, running: map[string]bool{"running": true}}
	chunk := int64(1 << 20)
	for i, s := range []string{"a", "b", "c", "d"} { // 8 MiB each, a is the oldest
		steps.finished[s] = retNow.Add(-days(1))
		addAttempt(repo, s, retNow.Add(-days(5)+time.Duration(i)*time.Hour), 8, chunk)
	}
	addAttempt(repo, "running", retNow.Add(-days(9)), 8, chunk)
	var logs bytes.Buffer
	ret := newRetention(repo, steps, &logs)
	ret.Policy.MaxOutputBytes = 32 * chunk // 40 MiB stored; 90% is 28.8 MiB
	if _, err := ret.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if repo.kinds("a")[2] == protocol.EventOutput || repo.kinds("b")[2] == protocol.EventOutput || repo.kinds("c")[2] != protocol.EventOutput {
		t.Fatalf("evicted a=%v b=%v c=%v", repo.kinds("a"), repo.kinds("b"), repo.kinds("c"))
	}
	if used, _ := repo.OutputUsage(t.Context()); used != 24*chunk {
		t.Fatalf("usage %d", used)
	}
	if !strings.Contains(logs.String(), "over the limit") {
		t.Fatalf("eviction not logged:\n%s", logs.String())
	}

	ret.Policy.MaxOutputBytes = 4 * chunk // only the running attempt's 8 MiB can't go
	logs.Reset()
	if _, err := ret.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if used, _ := repo.OutputUsage(t.Context()); used != 8*chunk {
		t.Fatalf("usage %d", used)
	}
	if !strings.Contains(logs.String(), "output storage full") {
		t.Fatalf("shortage not logged:\n%s", logs.String())
	}
}

// KIN-72: a pass empties the command logs of revoked runners in bounded batches and reports it.
func TestRetentionPurgesRevokedRunnerCommands(t *testing.T) {
	t.Parallel()
	repo := newMemRetentionRepo()
	repo.revoked = 3*RetentionBatch + 1
	var logs bytes.Buffer
	rep, err := newRetention(repo, &fakeSteps{}, &logs).Purge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RevokedCommands != 3*RetentionBatch+1 || repo.revoked != 0 {
		t.Errorf("report %+v, %d left", rep, repo.revoked)
	}
	if repo.maxBatch > RetentionBatch {
		t.Errorf("batch of %d", repo.maxBatch)
	}
	if !strings.Contains(logs.String(), "revoked_commands=") {
		t.Errorf("log %q", logs.String())
	}
}
