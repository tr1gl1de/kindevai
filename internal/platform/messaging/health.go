package messaging

import (
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// State is an adapter's integration state.
type State string

// Integration states.
const (
	StateOK          State = "ok"           // last call succeeded (or none failed yet)
	StateDegraded    State = "degraded"     // recent failures, below the breaker threshold
	StateCircuitOpen State = "circuit_open" // calls suspended until the cooldown ends, then one probe
)

// MaxErrorBytes bounds stored error texts.
const MaxErrorBytes = 512

// AdapterStatus is a point-in-time view of one adapter.
type AdapterStatus struct {
	Adapter             string
	State               State
	ConsecutiveFailures int
	LastError           string
	LastFailureAt       *time.Time
	OpenUntil           *time.Time // set while the circuit is open
}

type adapterHealth struct {
	failures    int
	lastError   string
	lastFailure time.Time
	openUntil   time.Time
	probing     bool
}

// Health tracks per-adapter state in memory (a restart starts every adapter as ok). Safe for concurrent use.
type Health struct {
	cfg Config
	mu  sync.Mutex
	m   map[string]*adapterHealth
}

// NewHealth returns a tracker using cfg's clock and breaker settings.
func NewHealth(cfg Config) *Health {
	return &Health{cfg: cfg.WithDefaults(), m: map[string]*adapterHealth{}}
}

func (h *Health) get(adapter string) *adapterHealth {
	a, ok := h.m[adapter]
	if !ok {
		a = &adapterHealth{}
		h.m[adapter] = a
	}
	return a
}

// Register makes an adapter visible (as ok) before its first call.
func (h *Health) Register(adapter string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.get(adapter)
}

// Success records a successful call and closes the circuit.
func (h *Health) Success(adapter string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.get(adapter) = adapterHealth{}
}

// Failure records a failed call (an outage, not a rejected request); at the threshold, or when a probe fails,
// the circuit opens for the cooldown.
func (h *Health) Failure(adapter string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.get(adapter)
	now := h.cfg.Now()
	a.failures++
	a.lastError = truncate(err.Error())
	a.lastFailure = now
	if a.probing || a.failures >= h.cfg.BreakerThreshold {
		a.openUntil = now.Add(h.cfg.BreakerCooldown)
	}
	a.probing = false
}

// Allow reports whether the adapter may be called now. With the circuit open it returns false until the
// cooldown ends, then true exactly once (the probe) until Success or Failure reports the probe's outcome.
func (h *Health) Allow(adapter string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.m[adapter]
	if !ok || a.openUntil.IsZero() {
		return true
	}
	if a.probing || h.cfg.Now().Before(a.openUntil) {
		return false
	}
	a.probing = true
	return true
}

// blocked lists adapters that Allow would refuse right now, without starting a probe.
func (h *Health) blocked() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.cfg.Now()
	var out []string
	for name, a := range h.m {
		if !a.openUntil.IsZero() && (a.probing || now.Before(a.openUntil)) {
			out = append(out, name)
		}
	}
	return out
}

// Status returns one adapter's state.
func (h *Health) Status(adapter string) AdapterStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.m[adapter]
	if !ok {
		a = &adapterHealth{}
	}
	return h.status(adapter, a)
}

func (h *Health) status(name string, a *adapterHealth) AdapterStatus {
	s := AdapterStatus{Adapter: name, State: StateOK, ConsecutiveFailures: a.failures, LastError: a.lastError}
	if a.failures > 0 {
		t := a.lastFailure
		s.LastFailureAt = &t
		s.State = StateDegraded
	}
	if !a.openUntil.IsZero() {
		t := a.openUntil
		s.OpenUntil = &t
		s.State = StateCircuitOpen
	}
	return s
}

// Snapshot returns all known adapters sorted by name.
func (h *Health) Snapshot() []AdapterStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]AdapterStatus, 0, len(h.m))
	for name, a := range h.m {
		out = append(out, h.status(name, a))
	}
	slices.SortFunc(out, func(a, b AdapterStatus) int { return strings.Compare(a.Adapter, b.Adapter) })
	return out
}

// truncate bounds an error text to MaxErrorBytes on a UTF-8 boundary.
func truncate(s string) string {
	if len(s) <= MaxErrorBytes {
		return s
	}
	s = s[:MaxErrorBytes-3]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}

// abandon cancels a probe started by Allow whose call never happened (the claim was not committed).
func (h *Health) abandon(adapter string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a, ok := h.m[adapter]; ok {
		a.probing = false
	}
}
