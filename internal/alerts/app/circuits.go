package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// Integration circuit events (KIN-60), raised by CircuitMonitor through the outbox like every other monitor's.
const (
	AdapterAlerts      = "alerts"
	TopicCircuitOpen   = "alerts.integration_circuit_open"
	TopicCircuitClosed = "alerts.integration_recovered"
)

// CircuitInterval is how often CircuitMonitor reads the integrations' health (a circuit stays open for at least
// the breaker cooldown, 1 min by default, so no opening is missed).
const CircuitInterval = 30 * time.Second

// HealthSource is the outbox's per-adapter health (messaging.Health).
type HealthSource interface {
	Snapshot() []messaging.AdapterStatus
}

// CircuitMonitor turns integration circuit breaker transitions into alert events: circuit_open raises a platform
// alert per adapter, ok resolves it. The health is in memory, so after a restart every adapter starts as ok: the
// first poll resolves alerts left open by the previous process (an adapter still failing opens again).
type CircuitMonitor struct {
	Health HealthSource
	Outbox Outbox
	Now    func() time.Time
	Logger *slog.Logger

	open map[string]bool // adapter → circuit open as last reported; absent: not reported yet
}

// Poll reads the health once and enqueues an event for each adapter whose circuit opened or closed since the last
// poll. An event that could not be enqueued is retried at the next poll.
func (m *CircuitMonitor) Poll(ctx context.Context) error {
	if m.open == nil {
		m.open = map[string]bool{}
	}
	for _, s := range m.Health.Snapshot() {
		was, known := m.open[s.Adapter]
		var topic string
		switch {
		case s.State == messaging.StateCircuitOpen && (!known || !was):
			topic = TopicCircuitOpen
		case s.State == messaging.StateOK && (!known || was):
			topic = TopicCircuitClosed
		default:
			continue
		}
		b, err := json.Marshal(map[string]any{"adapter": s.Adapter, "failures": s.ConsecutiveFailures})
		if err != nil {
			return err
		}
		key := "integration/" + s.Adapter + "/" + topic + "/" + strconv.FormatInt(m.Now().UnixNano(), 10)
		if err := m.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
			return fmt.Errorf("enqueueing %s: %w", topic, err)
		}
		m.open[s.Adapter] = topic == TopicCircuitOpen
	}
	return nil
}

// Run polls every CircuitInterval until ctx ends.
func (m *CircuitMonitor) Run(ctx context.Context) {
	tick := time.NewTicker(CircuitInterval)
	defer tick.Stop()
	for {
		if err := m.Poll(ctx); err != nil && ctx.Err() == nil && m.Logger != nil {
			m.Logger.Warn("integration circuit monitor", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
