package main

import (
	"slices"
	"testing"

	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runnersdomain "github.com/tr1gl1de/kindevai/internal/runners/domain"
	workflowdomain "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// KIN-31: a runner whose effective profile is strict gets steps only once its sandbox verified (strictEgress);
// macOS reports false, so it never gets them.
func TestStrictStepsOnlyOnVerifiedRunners(t *testing.T) {
	t.Parallel()
	cs := []runnersapp.Candidate{
		{ID: "linux-verified", StrictEgress: true},
		{ID: "mac", StrictEgress: false},
		{ID: "linux-loose", StrictEgress: false},
	}
	strict := func(id string) bool { return id != "linux-loose" }
	var ids []string
	for _, c := range schedulable(cs, strict) {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"linux-verified", "linux-loose"}) {
		t.Fatalf("candidates %v", ids)
	}
	if n := len(schedulable(cs, func(string) bool { return false })); n != 3 {
		t.Fatalf("no strict profile: %d candidates", n)
	}
}

// KIN-76: the scheduler gets each runner's parallel-step limit and disk level.
func TestSchedulableCarriesLimitAndDisk(t *testing.T) {
	t.Parallel()
	cs := []runnersapp.Candidate{
		{ID: "ok", DiskHeavyAllowed: true, Disk: runnersdomain.LevelOK, MaxParallelSteps: 2},
		{ID: "warn", DiskHeavyAllowed: true, Disk: runnersdomain.LevelWarning},
		{ID: "high", DiskHeavyAllowed: true, Disk: runnersdomain.LevelHigh},
	}
	got := schedulable(cs, func(string) bool { return false })
	want := []workflowdomain.DiskLevel{workflowdomain.DiskOK, workflowdomain.DiskWarning, workflowdomain.DiskHigh}
	for i, c := range got {
		if c.Disk != want[i] || !c.DiskHeavyAllowed {
			t.Errorf("%s: disk %v", c.ID, c.Disk)
		}
	}
	if got[0].MaxParallel != 2 || got[1].MaxParallel != 0 {
		t.Fatalf("limits %+v", got)
	}
}
