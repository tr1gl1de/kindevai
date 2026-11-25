package domain

import (
	"reflect"
	"testing"
)

func TestDiffPlans(t *testing.T) {
	t.Parallel()
	prev := PlanSnapshot{Summary: "Add login", Risk: RiskL1, MaxParallel: 1, Tests: []string{"go test ./..."}, Steps: []StepSnapshot{
		{Key: "api", Title: "API", Repository: "app", Scope: []string{"internal/api"}},
		{Key: "docs", Title: "Docs", Repository: "app", Scope: []string{"docs"}},
	}}
	cur := PlanSnapshot{Summary: "Add login with 2FA", Risk: RiskL2, MaxParallel: 2, Tests: []string{"go test ./...", "make e2e"},
		Migrations: []string{"0042_totp"}, Steps: []StepSnapshot{
			{Key: "api", Title: "API", Repository: "app", Role: "reviewer", Scope: []string{"internal/api", "internal/auth"}, DependsOn: []string{"db"}},
			{Key: "db", Title: "Migration", Repository: "app", Scope: []string{"migrations"}},
		}}
	got := DiffPlans(prev, cur)
	want := []Change{
		{Kind: ChangeModified, Field: "summary", Before: "Add login", After: "Add login with 2FA"},
		{Kind: ChangeModified, Field: "risk", Before: "L1", After: "L2"},
		{Kind: ChangeModified, Field: "maxParallel", Before: "1", After: "2"},
		{Kind: ChangeModified, Field: "tests", Before: "go test ./...", After: "go test ./..., make e2e", Added: []string{"make e2e"}},
		{Kind: ChangeModified, Field: "migrations", Before: "", After: "0042_totp", Added: []string{"0042_totp"}},
		{Kind: ChangeModified, StepKey: "api", Field: "role", Before: "", After: "reviewer"},
		{Kind: ChangeModified, StepKey: "api", Field: "scope", Before: "internal/api", After: "internal/api, internal/auth",
			Added: []string{"internal/auth"}},
		{Kind: ChangeModified, StepKey: "api", Field: "dependsOn", Before: "", After: "db", Added: []string{"db"}},
		{Kind: ChangeRemoved, StepKey: "docs", Field: "step", Before: "Docs"},
		{Kind: ChangeAdded, StepKey: "db", Field: "step", After: "Migration"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffPlans() =\n%+v\nwant\n%+v", got, want)
	}
	if d := DiffPlans(cur, cur); len(d) != 0 {
		t.Fatalf("identical plans differ: %+v", d)
	}
}

// A list field lists the items it gained and lost, so the UI shows "+x / −y" rather than two long strings.
func TestDiffPlansListsAddedAndRemovedItems(t *testing.T) {
	t.Parallel()
	step := func(scope ...string) PlanSnapshot {
		return PlanSnapshot{Steps: []StepSnapshot{{Key: "a", Title: "A", Repository: "r", Scope: scope}}}
	}
	got := DiffPlans(step("web/**", "docs", "api"), step("api", "web/src/**", "docs"))
	want := []Change{{Kind: ChangeModified, StepKey: "a", Field: "scope", Before: "web/**, docs, api",
		After: "api, web/src/**, docs", Added: []string{"web/src/**"}, Removed: []string{"web/**"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffPlans() =\n%+v\nwant\n%+v", got, want)
	}
	// Reordering a set is not a change.
	if d := DiffPlans(step("a", "b"), step("b", "a")); len(d) != 0 {
		t.Fatalf("reordered scope differs: %+v", d)
	}
}
