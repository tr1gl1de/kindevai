package events_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/events"
	alertsdb "github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type nobody struct{}

func (nobody) Owners(context.Context) ([]string, error)                  { return nil, nil }
func (nobody) TelegramChat(context.Context, string) (int64, bool, error) { return 0, false, nil }

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
