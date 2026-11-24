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
