package domain

import (
	"slices"
	"testing"
)

func TestDiffPlans(t *testing.T) {
	t.Parallel()
	prev := PlanSnapshot{Summary: "Add login", Risk: RiskL1, MaxParallel: 1, Steps: []StepSnapshot{
		{Key: "api", Title: "API", Repository: "app", Scope: []string{"internal/api"}},
		{Key: "docs", Title: "Docs", Repository: "app", Scope: []string{"docs"}},
	}}
	cur := PlanSnapshot{Summary: "Add login with 2FA", Risk: RiskL2, MaxParallel: 2, Steps: []StepSnapshot{
		{Key: "api", Title: "API", Repository: "app", Scope: []string{"internal/api", "internal/auth"}, DependsOn: []string{"db"}},
		{Key: "db", Title: "Migration", Repository: "app", Scope: []string{"migrations"}},
	}}
	got := DiffPlans(prev, cur)
	want := []Change{
		{Kind: ChangeModified, Field: "summary", Before: "Add login", After: "Add login with 2FA"},
		{Kind: ChangeModified, Field: "risk", Before: "L1", After: "L2"},
		{Kind: ChangeModified, Field: "maxParallel", Before: "1", After: "2"},
		{Kind: ChangeModified, StepKey: "api", Field: "scope", Before: "internal/api", After: "internal/api, internal/auth"},
		{Kind: ChangeModified, StepKey: "api", Field: "dependsOn", Before: "", After: "db"},
		{Kind: ChangeRemoved, StepKey: "docs", Field: "step", Before: "Docs"},
		{Kind: ChangeAdded, StepKey: "db", Field: "step", After: "Migration"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DiffPlans() =\n%+v\nwant\n%+v", got, want)
	}
	if d := DiffPlans(cur, cur); len(d) != 0 {
		t.Fatalf("identical plans differ: %+v", d)
	}
}
