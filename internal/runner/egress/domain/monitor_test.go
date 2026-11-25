package domain

import "testing"

// KIN-81: a running strict attempt's egress is checked again now and then; a wrong egress IP cuts it at once,
// other failures (the proxy or the diagnostic endpoint briefly unreachable) only when they repeat.
func TestWatchObserve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		codes []string
		want  []bool
	}{
		{"passing checks never cut", []string{"", "", ""}, []bool{false, false, false}},
		{"egress IP mismatch cuts at once", []string{"", CodeIPMismatch}, []bool{false, true}},
		{"one unreachable check is tolerated", []string{"proxy_unreachable", ""}, []bool{false, false}},
		{"repeated failures cut", []string{"proxy_unreachable", "timeout"}, []bool{false, true}},
		{"a pass resets the streak", []string{"timeout", "", "timeout", ""}, []bool{false, false, false, false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var w Watch
			for i, c := range tc.codes {
				if got := w.Observe(c); got != tc.want[i] {
					t.Fatalf("check %d (%q): cut = %v, want %v", i, c, got, tc.want[i])
				}
			}
		})
	}
}
