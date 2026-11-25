package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/events"
	alertsdb "github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type nobody struct{}

func (nobody) Visible(context.Context, auth.Principal) ([]app.ProjectRef, error) { return nil, nil }
func (nobody) Name(context.Context, string) (string, error)                      { return "", nil }
func (nobody) Members(context.Context, string) ([]string, error)                 { return nil, nil }
func (nobody) Admins(context.Context, string) ([]string, error)                  { return nil, nil }
func (nobody) Owners(context.Context) ([]string, error)                          { return nil, nil }
func (nobody) TelegramChat(context.Context, string) (int64, bool, error)         { return 0, false, nil }
func (nobody) Pending(context.Context, string) (bool, error)                     { return false, nil }
func (nobody) Record(context.Context, auditapp.Entry) error                      { return nil }
func (nobody) Enqueue(context.Context, messaging.Message) error                  { return nil }

type env struct {
	h    events.Handlers
	repo *alertsdb.Repository
	n    int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), "INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')"); err != nil {
		t.Fatal(err)
	}
	e := &env{repo: alertsdb.New(d)}
	ids := 0
	e.h = events.Handlers{Svc: &app.Service{Repo: e.repo, Projects: nobody{}, People: nobody{}, Approvals: nobody{},
		Tx: db.Transactor{DB: d}, Audit: nobody{}, Outbox: nobody{}, Now: time.Now,
		NewID: func() string { ids++; return "a" + string(rune('0'+ids)) }}}
	return e
}

// deliver hands a payload to a handler as a fresh outbox delivery.
func (e *env) deliver(t *testing.T, fn func(context.Context, messaging.Delivery) error, topic string, p any) error {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	e.n++
	return fn(t.Context(), messaging.Delivery{Topic: topic, Key: topic + "/" + string(rune('a'+e.n)), Payload: b})
}

func (e *env) active(t *testing.T, key string) (domain.Alert, bool) {
	t.Helper()
	a, err := e.repo.Active(t.Context(), key)
	if errors.Is(err, domain.ErrNotFound) {
		return a, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return a, true
}

// KIN-60 AC: each source raises its alert (dedup per subject, right scope and kind) and its paired event resolves it.
func TestSourcesRaiseAndResolve(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		raise, resolve   func(events.Handlers) func(context.Context, messaging.Delivery) error
		raiseP, resolveP any
		key, project     string
		kind             domain.Kind
	}{
		{"egress",
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.EgressCheckFailed },
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.EgressCheckPassed },
			map[string]any{"runnerId": "r1", "testId": "t1", "code": "egress_ip_mismatch"}, map[string]any{"runnerId": "r1"},
			"runner/r1/egress", "", domain.KindEgressCheckFailed},
		{"ci on branch",
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.CIFailed },
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.CIPassed },
			map[string]any{"projectId": "p1", "repository": "acme/shop", "branch": "dev", "conclusion": "failure"},
			map[string]any{"projectId": "p1", "repository": "acme/shop", "branch": "dev"},
			"ci/acme/shop/branch/dev", "p1", domain.KindCIFailed},
		{"ci on pr",
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.CIFailed },
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.CIPassed },
			map[string]any{"projectId": "p1", "repository": "acme/shop", "branch": "fix", "pullRequest": 7, "conclusion": "timed_out"},
			map[string]any{"projectId": "p1", "repository": "acme/shop", "branch": "fix", "pullRequest": 7},
			"ci/acme/shop/pr/7", "p1", domain.KindCIFailed},
		{"circuit",
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.CircuitOpen },
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.CircuitClosed },
			map[string]any{"adapter": "youtrack", "failures": 5}, map[string]any{"adapter": "youtrack"},
			"integration/youtrack/circuit", "", domain.KindIntegrationCircuitOpen},
		{"blocked task",
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.TaskBlocked },
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.TaskUnblocked },
			map[string]any{"projectId": "p1", "taskId": "t9", "reason": "runner"}, map[string]any{"taskId": "t9"},
			"task/t9/blocked", "p1", domain.KindTaskBlocked},
		{"storage recovered",
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.StorageLevel },
			func(h events.Handlers) func(context.Context, messaging.Delivery) error { return h.StorageRecovered },
			map[string]any{"machineId": "m1", "role": "data", "level": "high", "usedPercent": 88},
			map[string]any{"machineId": "m1", "role": "data"},
			"storage/m1/data", "", domain.KindStorageLevel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			for range 2 { // a repeat is grouped into the same alert
				if err := e.deliver(t, c.raise(e.h), "raise", c.raiseP); err != nil {
					t.Fatal(err)
				}
			}
			a, ok := e.active(t, c.key)
			if !ok || a.Kind != c.kind || a.ProjectID != c.project || a.Occurrences != 2 {
				t.Fatalf("alert %+v, active %v", a, ok)
			}
			if err := e.deliver(t, c.resolve(e.h), "resolve", c.resolveP); err != nil {
				t.Fatal(err)
			}
			if _, ok := e.active(t, c.key); ok {
				t.Fatal("still active after the source recovered")
			}
			got, err := e.repo.Get(t.Context(), a.ID)
			if err != nil || got.State != domain.StateResolved || got.ClosedBy != "" {
				t.Fatalf("closed alert %+v, %v", got, err)
			}
		})
	}
}

// Malformed events can never succeed: they are permanent failures, not retried.
func TestSourcesRejectIncompleteEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for name, fn := range map[string]func(context.Context, messaging.Delivery) error{
		"egress": e.h.EgressCheckFailed, "ci": e.h.CIFailed, "circuit": e.h.CircuitOpen, "blocked": e.h.TaskBlocked,
		"egressOK": e.h.EgressCheckPassed, "ciOK": e.h.CIPassed, "circuitOK": e.h.CircuitClosed,
		"unblocked": e.h.TaskUnblocked, "storageOK": e.h.StorageRecovered,
	} {
		if err := e.deliver(t, fn, name, map[string]any{}); !messaging.IsPermanent(err) {
			t.Errorf("%s: err = %v, want permanent", name, err)
		}
	}
}

// KIN-59: a full runner output storage is one grouped Control Plane alert; output stored again resolves it.
func TestRunnerOutputFullGroupsAndResolves(t *testing.T) {
	t.Parallel()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	repo := alertsdb.New(d)
	n := 0
	svc := &app.Service{Repo: repo, People: nobody{}, Tx: db.Transactor{DB: d},
		Now:   func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
		NewID: func() string { n++; return fmt.Sprintf("a%d", n) }}
	h := events.Handlers{Svc: svc}
	full := func(key string) {
		t.Helper()
		if err := h.RunnerOutputFull(t.Context(), messaging.Delivery{Topic: "alerts.runner_output_full", Key: key,
			Payload: []byte(`{"limitBytes":1073741824,"runnerId":"r1","stepId":"s1"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	full("runner-output/full/1")
	full("runner-output/full/2")
	active, err := repo.List(t.Context(), app.ListQuery{ActiveOnly: true, Platform: true, Limit: 10})
	if err != nil || len(active) != 1 {
		t.Fatalf("active alerts %+v %v", active, err)
	}
	a := active[0]
	if a.Kind != domain.KindRunnerOutputFull || a.Severity != domain.SeverityWarning || a.ProjectID != "" ||
		a.Occurrences != 2 || a.Subject != "control-plane" || a.Details["limitBytes"] != "1073741824" {
		t.Fatalf("alert %+v", a)
	}
	if err := h.RunnerOutputFull(t.Context(), messaging.Delivery{Topic: "alerts.runner_output_full", Key: "k",
		Payload: []byte(`{"limitBytes":"x"}`)}); !messaging.IsPermanent(err) {
		t.Fatalf("malformed payload: %v", err)
	}
	// Runner-supplied ids are clipped to what an alert may hold instead of losing the alert.
	long := strings.Repeat("r", 300)
	if err := h.RunnerOutputFull(t.Context(), messaging.Delivery{Topic: "alerts.runner_output_full", Key: "runner-output/full/3",
		Payload: []byte(`{"limitBytes":1073741824,"runnerId":"` + long + `","stepId":"s\u0007x"}`)}); err != nil {
		t.Fatalf("long runner id: %v", err)
	}
	if active, _ := repo.List(t.Context(), app.ListQuery{ActiveOnly: true, Platform: true, Limit: 10}); len(active) != 1 ||
		active[0].Occurrences != 3 || len(active[0].Details["runnerId"]) > 128 || active[0].Details["stepId"] != "sx" {
		t.Fatalf("clipped alert %+v", active)
	}
	if err := h.RunnerOutputRecovered(t.Context(), messaging.Delivery{Topic: "alerts.runner_output_recovered", Key: "r",
		Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if active, _ := repo.List(t.Context(), app.ListQuery{ActiveOnly: true, Platform: true, Limit: 10}); len(active) != 0 {
		t.Fatalf("still active %+v", active)
	}
}

// KIN-62: a task_finished event that can never succeed (malformed or for a state that is not announced) is a
// permanent failure, not a retry loop.
func TestTaskFinishedRejectsBadEventsPermanently(t *testing.T) {
	t.Parallel()
	h := events.Handlers{Svc: &app.Service{}}
	for _, payload := range []string{`{`, `{"taskId":"t1","projectId":"p1","state":"CANCELLED"}`, `{"projectId":"p1","state":"FAILED"}`} {
		err := h.TaskFinished(t.Context(), messaging.Delivery{Topic: "telegram.task_finished", Payload: []byte(payload)})
		if !messaging.IsPermanent(err) {
			t.Fatalf("%s: err = %v, want permanent", payload, err)
		}
	}
}

// KIN-83: a held CLI is one platform alert per runner and CLI; it escalates to critical with the streak and resolves
// once the CLI works again (or the Owner marks it ready).
func TestCLIHeldEscalatesAndResolves(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	held := func(strikes int, escalated bool) {
		t.Helper()
		if err := e.deliver(t, e.h.CLIHeld, "alerts.cli_held", map[string]any{"runnerId": "r1", "agent": "claude",
			"cause": "auth", "strikes": strikes, "escalated": escalated}); err != nil {
			t.Fatal(err)
		}
	}
	held(1, false)
	a, ok := e.active(t, events.CLIDedupKey("r1", "claude"))
	if !ok || a.Kind != domain.KindCLIHeld || a.Severity != domain.SeverityWarning || a.ProjectID != "" ||
		a.Details["runnerId"] != "r1" || a.Details["agent"] != "claude" || a.Details["cause"] != "auth" {
		t.Fatalf("alert %+v %v", a, ok)
	}
	held(2, false)
	held(3, true)
	if a, _ = e.active(t, events.CLIDedupKey("r1", "claude")); a.Severity != domain.SeverityCritical || a.Occurrences != 3 ||
		a.Details["strikes"] != "3" {
		t.Fatalf("escalated alert %+v", a)
	}
	if err := e.deliver(t, e.h.CLIReady, "alerts.cli_ready", map[string]any{"runnerId": "r1", "agent": "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.active(t, events.CLIDedupKey("r1", "claude")); ok {
		t.Fatal("still active after the CLI is ready")
	}
	for _, fn := range []func(context.Context, messaging.Delivery) error{e.h.CLIHeld, e.h.CLIReady} {
		if err := e.deliver(t, fn, "x", map[string]any{"runnerId": "r1"}); !messaging.IsPermanent(err) {
			t.Fatalf("incomplete event: %v", err)
		}
	}
}
