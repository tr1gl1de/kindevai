package app_test

import (
	"encoding/json"
	"slices"
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
