package domain

import "testing"

func TestPickRunner(t *testing.T) {
	t.Parallel()
	git := func(id string) RunnerCandidate {
		return RunnerCandidate{ID: id, Tools: []string{"git", "claude"}, DiskHeavyAllowed: true}
	}
	full := RunnerCandidate{ID: "a-full", Tools: []string{"git"}, DiskHeavyAllowed: false}
	noGit := RunnerCandidate{ID: "a-nogit", Tools: []string{"claude"}, DiskHeavyAllowed: true}
	tests := []struct {
		name  string
		cands []RunnerCandidate
		caps  []string
		held  map[string]int
		want  string // "" = no runner
	}{
		{"no candidates", nil, []string{"git"}, nil, ""},
		{"only ineligible candidates", []RunnerCandidate{full, noGit}, []string{"git"}, nil, ""},
		{"no capabilities needed still needs disk", []RunnerCandidate{full}, nil, nil, ""},
		{"equal load ties by id", []RunnerCandidate{git("r2"), git("r1"), git("r3")}, []string{"git"}, nil, "r1"},
		{"least loaded wins over a lower id", []RunnerCandidate{git("r1"), git("r2")}, []string{"git"},
			map[string]int{"r1": 1}, "r2"},
		{"fewest held steps", []RunnerCandidate{git("r1"), git("r2"), git("r3")}, []string{"git"},
			map[string]int{"r1": 3, "r2": 2, "r3": 5}, "r2"},
		{"load tie among the least loaded ties by id", []RunnerCandidate{git("r3"), git("r2"), git("r1")}, []string{"git"},
			map[string]int{"r1": 2, "r2": 1, "r3": 1}, "r2"},
		{"idle ineligible runners never win", []RunnerCandidate{full, noGit, git("r1")}, []string{"git"},
			map[string]int{"r1": 7}, "r1"},
		{"missing one of several capabilities", []RunnerCandidate{git("r1"), {ID: "r0", Tools: []string{"git"},
			DiskHeavyAllowed: true}}, []string{"git", "claude"}, map[string]int{"r1": 4}, "r1"},
		{"load of unknown runners is ignored", []RunnerCandidate{git("r2")}, []string{"git"},
			map[string]int{"r1": 0, "gone": 9}, "r2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := PickRunner(tt.cands, tt.caps, tt.held)
			if ok != (tt.want != "") || got.ID != tt.want {
				t.Fatalf("PickRunner = %q, %v; want %q", got.ID, ok, tt.want)
			}
		})
	}
}

// The choice does not depend on the order the candidates are listed in.
func TestPickRunnerIsOrderIndependent(t *testing.T) {
	t.Parallel()
	a := RunnerCandidate{ID: "a", Tools: []string{"git"}, DiskHeavyAllowed: true}
	b := RunnerCandidate{ID: "b", Tools: []string{"git"}, DiskHeavyAllowed: true}
	c := RunnerCandidate{ID: "c", Tools: []string{"git"}, DiskHeavyAllowed: true}
	held := map[string]int{"a": 1}
	for _, order := range [][]RunnerCandidate{{a, b, c}, {c, b, a}, {b, a, c}, {c, a, b}} {
		if got, _ := PickRunner(order, []string{"git"}, held); got.ID != "b" {
			t.Fatalf("order %v picked %q", order, got.ID)
		}
	}
}

// KIN-76: a runner's parallel-step limit, its recent attempt failures and its disk level, and the order in which they
// rank: at the limit a runner is no candidate; then not failing before failing, ok disk before warning before high,
// fewer held steps, smaller id.
func TestPickRunnerFactors(t *testing.T) {
	t.Parallel()
	r := func(id string, opts ...func(*RunnerCandidate)) RunnerCandidate {
		c := RunnerCandidate{ID: id, Tools: []string{"git"}, DiskHeavyAllowed: true}
		for _, o := range opts {
			o(&c)
		}
		return c
	}
	limit := func(n int) func(*RunnerCandidate) { return func(c *RunnerCandidate) { c.MaxParallel = n } }
	fails := func(n int) func(*RunnerCandidate) { return func(c *RunnerCandidate) { c.RecentFailures = n } }
	disk := func(l DiskLevel) func(*RunnerCandidate) { return func(c *RunnerCandidate) { c.Disk = l } }
	tests := []struct {
		name  string
		cands []RunnerCandidate
		held  map[string]int
		want  string // "" = no runner
	}{
		{"at its limit a runner is no candidate", []RunnerCandidate{r("r1", limit(2))}, map[string]int{"r1": 2}, ""},
		{"over its limit (limit lowered) neither", []RunnerCandidate{r("r1", limit(1))}, map[string]int{"r1": 3}, ""},
		{"below its limit it is", []RunnerCandidate{r("r1", limit(2))}, map[string]int{"r1": 1}, "r1"},
		{"no limit (0) is unlimited", []RunnerCandidate{r("r1")}, map[string]int{"r1": 50}, "r1"},
		{"a full runner leaves the step to a busier one", []RunnerCandidate{r("r1", limit(1)), r("r2")},
			map[string]int{"r1": 1, "r2": 4}, "r2"},
		{"one failure is no penalty", []RunnerCandidate{r("r1", fails(1)), r("r2")}, nil, "r1"},
		{"a failing runner ranks after a clean one", []RunnerCandidate{r("r1", fails(FailingAttempts)), r("r2")},
			nil, "r2"},
		{"a failing runner stays eligible", []RunnerCandidate{r("r1", fails(9))}, nil, "r1"},
		{"failing outranks load", []RunnerCandidate{r("r1", fails(2)), r("r2")}, map[string]int{"r2": 5}, "r2"},
		{"disk warning ranks below ok", []RunnerCandidate{r("r1", disk(DiskWarning)), r("r2")}, nil, "r2"},
		{"disk high ranks below warning", []RunnerCandidate{r("r1", disk(DiskHigh)), r("r2", disk(DiskWarning))},
			nil, "r2"},
		{"disk outranks load", []RunnerCandidate{r("r1", disk(DiskWarning)), r("r2")}, map[string]int{"r2": 5}, "r2"},
		{"failing outranks disk", []RunnerCandidate{r("r1", fails(2)), r("r2", disk(DiskHigh))}, nil, "r2"},
		{"all failing: disk then load decide", []RunnerCandidate{r("r1", fails(3), disk(DiskWarning)),
			r("r2", fails(2)), r("r3", fails(2))}, map[string]int{"r2": 1}, "r3"},
		{"critical disk is still never picked", []RunnerCandidate{{ID: "r0", Tools: []string{"git"}}, r("r1", fails(5))},
			nil, "r1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := PickRunner(tt.cands, []string{"git"}, tt.held)
			if ok != (tt.want != "") || got.ID != tt.want {
				t.Fatalf("PickRunner = %q, %v; want %q", got.ID, ok, tt.want)
			}
		})
	}
}

// KIN-76: a step that only runners at their limit could take waits for a runner, and the reason says they are busy.
func TestPickRouteRunnersAtLimit(t *testing.T) {
	t.Parallel()
	busy := RunnerCandidate{ID: "r1", Tools: []string{"git", "claude"}, DiskHeavyAllowed: true, MaxParallel: 1}
	chain := []AgentChoice{{Agent: "claude"}}
	_, _, reason, ok := PickRoute([]RunnerCandidate{busy}, []string{"git"}, map[string]int{"r1": 1}, chain, nil)
	if ok || reason.Code != WaitRunner || reason.Detail != DetailRunnersAtLimit {
		t.Fatalf("PickRoute = %v %+v", ok, reason)
	}
	if !AtLimit([]RunnerCandidate{busy}, []string{"git"}, map[string]int{"r1": 1}) ||
		AtLimit([]RunnerCandidate{busy}, []string{"git"}, nil) {
		t.Fatal("AtLimit")
	}
	if c, _, _, ok := PickRoute([]RunnerCandidate{busy}, []string{"git"}, nil, chain, nil); !ok || c.ID != "r1" {
		t.Fatalf("below the limit: %q %v", c.ID, ok)
	}
}
