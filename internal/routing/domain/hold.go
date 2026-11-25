package domain

import (
	"slices"
	"time"
)

// CLI holds (KIN-79, KIN-83): a runner's CLI that reported cli_limit is skipped there (a cool-down) while its hold
// lasts. A rate limit resets by itself; a logged-out CLI (auth) or a refused subscription (billing) needs a human,
// so those back off and finally wait for the Owner instead of retrying every 30 minutes forever.

// Causes of a cli_limit hold (the runner protocol's cause values).
const (
	CauseRateLimit = "rate_limit"
	CauseAuth      = "auth"
	CauseBilling   = "billing"
)

// HumanBackoff is the wait after the 1st, 2nd and 3rd consecutive auth/billing hold of a runner's CLI; the next one
// parks the CLI there until the Owner marks it ready again.
var HumanBackoff = []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// EscalateAfter is how many consecutive holds of the same cause make the CLI's alert critical.
const EscalateAfter = 3

// RateLimitDelay is the wait of a rate limit whose CLI named no reset time; MaxHold bounds any wait.
const (
	RateLimitDelay = 30 * time.Minute
	MaxHold        = 7 * 24 * time.Hour
)

// NeedsHuman reports a cause that does not go away by waiting (logged out, billing).
func NeedsHuman(cause string) bool { return cause == CauseAuth || cause == CauseBilling }

// KnownCause normalizes a reported cause: anything else counts as a rate limit.
func KnownCause(cause string) string {
	if slices.Contains([]string{CauseRateLimit, CauseAuth, CauseBilling}, cause) {
		return cause
	}
	return CauseRateLimit
}

// Hold is a runner's CLI held after cli_limit: why, how many holds of that cause in a row, until when, and whether
// it waits for the Owner (Parked: no timer, Until is only an upper bound for display).
type Hold struct {
	Cause   string
	Strikes int
	Until   time.Time
	Parked  bool
}

// Active reports a hold still in force at t.
func (h Hold) Active(t time.Time) bool { return h.Parked || t.Before(h.Until) }

// Escalated reports a streak long enough for a critical alert.
func (h Hold) Escalated() bool { return h.Strikes >= EscalateAfter }

// NeedsOwner reports a hold only the Owner can lift.
func (h Hold) NeedsOwner() bool { return h.Parked }

// NextHold is the hold after a new cli_limit report at now (prev: the pair's last hold, nil when none or after
// the CLI worked again). reported is the reset time the CLI named (zero: none; only rate limits use it); limit
// bounds any wait. A report while the same hold is still active (attempts that were already running) is not a
// new strike: the hold stays, a rate limit only takes a later reset.
func NextHold(prev *Hold, cause string, reported, now time.Time, limit time.Duration) Hold {
	cause = KnownCause(cause)
	if prev != nil && prev.Cause == cause && prev.Active(now) {
		h := *prev
		if !NeedsHuman(cause) {
			if u := rateUntil(reported, now, limit); u.After(h.Until) {
				h.Until = u
			}
		}
		return h
	}
	h := Hold{Cause: cause, Strikes: 1}
	if prev != nil && prev.Cause == cause {
		h.Strikes = prev.Strikes + 1
	}
	switch {
	case !NeedsHuman(cause):
		h.Until = rateUntil(reported, now, limit)
	case h.Strikes <= len(HumanBackoff):
		h.Until = now.Add(HumanBackoff[h.Strikes-1]).UTC()
	default:
		h.Until, h.Parked = now.Add(limit).UTC(), true
	}
	return h
}

func rateUntil(reported, now time.Time, limit time.Duration) time.Time {
	u := reported
	if u.IsZero() || !u.After(now) {
		u = now.Add(RateLimitDelay)
	}
	if m := now.Add(limit); u.After(m) {
		u = m
	}
	return u.UTC()
}
