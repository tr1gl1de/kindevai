package domain

import (
	"testing"
	"time"
)

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// Acceptance (KIN-83): auth/billing holds back off 30 min, then 2 h, then 6 h; after that the runner's CLI is parked
// until the Owner marks it ready again (no timer).
func TestLoggedOutAndBillingHoldsBackOffThenPark(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{CauseAuth, CauseBilling} {
		var prev *Hold
		var at = now
		for i, want := range []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour} {
			h := NextHold(prev, cause, time.Time{}, at, MaxHold)
			if h.Strikes != i+1 || h.Parked || !h.Until.Equal(at.Add(want)) {
				t.Fatalf("%s hold %d = %+v, want +%v", cause, i+1, h, want)
			}
			prev, at = &h, h.Until.Add(time.Minute) // the retry after the wait fails again
		}
		h := NextHold(prev, cause, time.Time{}, at, MaxHold)
		if h.Strikes != 4 || !h.Parked || !h.Until.Equal(at.Add(MaxHold)) {
			t.Fatalf("%s hold 4 = %+v, want parked", cause, h)
		}
		if !h.Active(at.Add(30*24*time.Hour)) || !h.NeedsOwner() {
			t.Fatalf("%s: a parked hold expired: %+v", cause, h)
		}
	}
}

// A rate limit resets by itself: the CLI's reset time (else 30 min) is kept however often it repeats; it is never
// parked.
func TestRateLimitHoldsKeepTheResetTime(t *testing.T) {
	t.Parallel()
	var prev *Hold
	at := now
	for i := range 5 {
		h := NextHold(prev, CauseRateLimit, at.Add(3*time.Hour), at, MaxHold)
		if h.Strikes != i+1 || h.Parked || !h.Until.Equal(at.Add(3*time.Hour)) || h.NeedsOwner() {
			t.Fatalf("rate limit hold %d = %+v", i+1, h)
		}
		prev, at = &h, h.Until.Add(time.Minute)
	}
	if h := NextHold(nil, "", time.Time{}, now, MaxHold); h.Cause != CauseRateLimit || !h.Until.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("unknown cause = %+v, want a rate limit of 30 min", h)
	}
	if h := NextHold(nil, CauseRateLimit, now.Add(30*24*time.Hour), now, MaxHold); !h.Until.Equal(now.Add(MaxHold)) {
		t.Fatalf("far reset = %+v, want capped", h)
	}
}

// Acceptance (KIN-83): after EscalateAfter consecutive holds of the same cause the alert escalates; another cause
// starts a new streak.
func TestConsecutiveHoldsOfTheSameCauseEscalate(t *testing.T) {
	t.Parallel()
	h1 := NextHold(nil, CauseAuth, time.Time{}, now, MaxHold)
	h2 := NextHold(&h1, CauseAuth, time.Time{}, h1.Until, MaxHold)
	h3 := NextHold(&h2, CauseAuth, time.Time{}, h2.Until, MaxHold)
	if h1.Escalated() || h2.Escalated() || !h3.Escalated() || EscalateAfter != 3 {
		t.Fatalf("escalation %v %v %v", h1.Escalated(), h2.Escalated(), h3.Escalated())
	}
	other := NextHold(&h3, CauseBilling, time.Time{}, h3.Until, MaxHold)
	if other.Strikes != 1 || other.Escalated() || !other.Until.Equal(h3.Until.Add(30*time.Minute)) {
		t.Fatalf("a new cause continued the streak: %+v", other)
	}
}

// Attempts already running when the CLI was found logged out end with the same hold: they do not count as further
// strikes while the hold is still active.
func TestHoldsWhileTheHoldIsActiveDoNotCount(t *testing.T) {
	t.Parallel()
	h1 := NextHold(nil, CauseAuth, time.Time{}, now, MaxHold)
	again := NextHold(&h1, CauseAuth, time.Time{}, now.Add(time.Minute), MaxHold)
	if again != h1 {
		t.Fatalf("a concurrent hold changed the hold: %+v, want %+v", again, h1)
	}
	r1 := NextHold(nil, CauseRateLimit, now.Add(time.Hour), now, MaxHold)
	later := NextHold(&r1, CauseRateLimit, now.Add(2*time.Hour), now.Add(time.Minute), MaxHold)
	if later.Strikes != 1 || !later.Until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("a later reset of a concurrent rate limit = %+v", later)
	}
}
