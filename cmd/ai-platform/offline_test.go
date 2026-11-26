package main

import (
	"maps"
	"testing"
	"time"

	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runnersdomain "github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// KIN-118: a runner counts as offline without a live session (one that just reconnected is online even with an old
// heartbeat, review finding), from its last heartbeat — but never from before the control plane started (its own downtime is not the runner's).
func TestOfflineSinceCountsFromTheLastHeartbeatButNotBeforeTheStart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	before, after := start.Add(-time.Hour), start.Add(5*time.Minute)
	pres := map[string]runnersapp.Presence{
		"live":        {Availability: runnersdomain.AvailabilityOnline, LastHeartbeatAt: &after},
		"no-session":  {Availability: runnersdomain.AvailabilityOnline, LastHeartbeatAt: &after},
		"stopped":     {Availability: runnersdomain.AvailabilityUnreachable, LastHeartbeatAt: &after},
		"before-boot": {Availability: runnersdomain.AvailabilityUnreachable, LastHeartbeatAt: &before},
	}
	session := map[string]bool{"live": true, "stopped": true}
	got := offlineSince([]string{"live", "no-session", "stopped", "before-boot", "never-seen"}, pres,
		func(id string) bool { return session[id] }, start)
	want := map[string]time.Time{"no-session": after, "before-boot": start, "never-seen": start}
	if !maps.Equal(got, want) {
		t.Fatalf("offline = %v, want %v", got, want)
	}
}
