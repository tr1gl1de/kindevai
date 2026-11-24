package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type health struct{ states map[string]messaging.State }

func (h *health) Snapshot() []messaging.AdapterStatus {
	var out []messaging.AdapterStatus
	for a, s := range h.states {
		out = append(out, messaging.AdapterStatus{Adapter: a, State: s, ConsecutiveFailures: 5})
	}
	return out
}

// KIN-60 AC: an integration's circuit opening raises one event per opening; the adapter working again resolves it
// (also right after a restart, when the in-memory health starts every adapter as ok).
func TestCircuitMonitorReportsTransitions(t *testing.T) {
	t.Parallel()
	h := &health{states: map[string]messaging.State{"youtrack": messaging.StateOK}}
	out := &outbox{}
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	m := &app.CircuitMonitor{Health: h, Outbox: out, Now: func() time.Time { now = now.Add(time.Second); return now }}
	steps := []messaging.State{messaging.StateOK, messaging.StateDegraded, messaging.StateCircuitOpen,
		messaging.StateCircuitOpen, messaging.StateDegraded, messaging.StateOK, messaging.StateOK}
	for _, s := range steps {
		h.states["youtrack"] = s
		if err := m.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var topics, keys []string
	for _, msg := range out.msgs {
		topics, keys = append(topics, msg.Topic), append(keys, msg.Key)
		var p struct{ Adapter string }
		if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Adapter != "youtrack" || msg.Adapter != "alerts" {
			t.Fatalf("event %+v", msg)
		}
	}
	want := []string{app.TopicCircuitClosed, app.TopicCircuitOpen, app.TopicCircuitClosed}
	if !slices.Equal(topics, want) {
		t.Fatalf("topics = %v, want %v", topics, want)
	}
	slices.Sort(keys)
	if len(slices.Compact(keys)) != 3 {
		t.Fatalf("keys not unique: %v", keys)
	}
}

// Review: the poll interval must stay below the default breaker cooldown, or a short opening could be missed.
func TestCircuitIntervalBelowCooldown(t *testing.T) {
	t.Parallel()
	if cd := (messaging.Config{}).WithDefaults().BreakerCooldown; app.CircuitInterval >= cd {
		t.Fatalf("interval %v >= cooldown %v", app.CircuitInterval, cd)
	}
}

type failingOutbox struct {
	outbox
	fail bool
}

func (o *failingOutbox) Enqueue(ctx context.Context, m messaging.Message) error {
	if o.fail {
		return errors.New("db busy")
	}
	return o.outbox.Enqueue(ctx, m)
}

// Review: an event that could not be enqueued is retried at the next poll; the alerts adapter itself is skipped.
func TestCircuitMonitorRetriesAndSkipsItself(t *testing.T) {
	t.Parallel()
	h := &health{states: map[string]messaging.State{"github": messaging.StateCircuitOpen, app.AdapterAlerts: messaging.StateCircuitOpen}}
	out := &failingOutbox{fail: true}
	m := &app.CircuitMonitor{Health: h, Outbox: out, Now: time.Now}
	if err := m.Poll(t.Context()); err == nil {
		t.Fatal("enqueue failure not reported")
	}
	out.fail = false
	if err := m.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(out.msgs) != 1 || out.msgs[0].Topic != app.TopicCircuitOpen || !strings.Contains(string(out.msgs[0].Payload), "github") {
		t.Fatalf("events = %+v", out.msgs)
	}
}
