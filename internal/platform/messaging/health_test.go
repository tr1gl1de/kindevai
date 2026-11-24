package messaging_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

func TestHealthStates(t *testing.T) {
	t.Parallel()
	c := newClock()
	h := messaging.NewHealth(messaging.Config{Now: c.Now, BreakerThreshold: 3, BreakerCooldown: time.Minute})
	h.Register("telegram")
	if s := h.Status("telegram"); s.State != messaging.StateOK || s.Adapter != "telegram" {
		t.Fatalf("new adapter = %+v", s)
	}
	// Inbound adapters (polling) report their own outcomes.
	h.Failure("telegram", errors.New("dial tcp: timeout"))
	if s := h.Status("telegram"); s.State != messaging.StateDegraded || s.ConsecutiveFailures != 1 {
		t.Fatalf("after one failure = %+v, want degraded", s)
	}
	h.Failure("telegram", errors.New("x"))
	h.Failure("telegram", errors.New("x"))
	s := h.Status("telegram")
	if s.State != messaging.StateCircuitOpen || s.OpenUntil == nil || !s.OpenUntil.Equal(c.Now().Add(time.Minute)) {
		t.Fatalf("after threshold = %+v, want circuit_open for the cooldown", s)
	}
	if h.Allow("telegram") {
		t.Fatal("open circuit allowed a call")
	}
	c.Advance(time.Minute)
	if !h.Allow("telegram") {
		t.Fatal("after the cooldown one probe must be allowed")
	}
	if h.Allow("telegram") {
		t.Fatal("only one probe at a time")
	}
	h.Failure("telegram", errors.New("still down"))
	if s := h.Status("telegram"); s.State != messaging.StateCircuitOpen || !s.OpenUntil.Equal(c.Now().Add(time.Minute)) {
		t.Fatalf("failed probe = %+v, want the circuit reopened", s)
	}
	c.Advance(time.Minute)
	if !h.Allow("telegram") {
		t.Fatal("second probe not allowed")
	}
	h.Success("telegram")
	if s := h.Status("telegram"); s.State != messaging.StateOK || s.ConsecutiveFailures != 0 || s.OpenUntil != nil {
		t.Fatalf("after success = %+v, want ok", s)
	}
	if !h.Allow("unknown") {
		t.Fatal("an adapter without failures must be allowed")
	}
	h.Register("github")
	all := h.Snapshot()
	if len(all) != 2 || all[0].Adapter != "github" || all[1].Adapter != "telegram" {
		t.Fatalf("snapshot = %+v, want adapters sorted by name", all)
	}
}

// Error texts are kept short; long upstream bodies are not stored in full.
func TestHealthTruncatesErrors(t *testing.T) {
	t.Parallel()
	h := messaging.NewHealth(messaging.Config{})
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'e'
	}
	h.Failure("github", errors.New(string(long)))
	if s := h.Status("github"); len(s.LastError) > messaging.MaxErrorBytes {
		t.Fatalf("last error is %d bytes", len(s.LastError))
	}
	// Invalid UTF-8 at the start must not erase the whole text.
	h.Failure("github", errors.New("\xff"+string(long)))
	if s := h.Status("github"); len(s.LastError) < messaging.MaxErrorBytes/2 {
		t.Fatalf("invalid UTF-8 truncated the error to %d bytes", len(s.LastError))
	}
}
