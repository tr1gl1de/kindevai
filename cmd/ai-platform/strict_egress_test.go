package main

import (
	"slices"
	"testing"

	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
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
