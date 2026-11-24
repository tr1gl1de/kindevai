package domain

import (
	"slices"
	"testing"
)

func TestInScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		scope []string
		file  string
		want  bool
	}{
		{[]string{"web/src"}, "web/src/app/App.tsx", true},
		{[]string{"web/src"}, "web/src", true},
		{[]string{"web/src"}, "web/srcx/a.ts", false},
		{[]string{"README.md"}, "README.md", true},
		{[]string{"README.md"}, "docs/README.md", false},
		{[]string{"."}, "anything/x.go", true},
		{[]string{"docs/*.md"}, "docs/a.md", true},
		{[]string{"docs/*.md"}, "docs/sub/a.md", false},
		{[]string{"docs/**"}, "docs/sub/a.md", true},
		{[]string{"web"}, "../web/a.ts", false}, // never trust a path that escapes the repository
		{[]string{"web"}, "/web/a.ts", false},
		{nil, "a.go", false},
	}
	for _, tt := range tests {
		if got := InScope(tt.scope, tt.file); got != tt.want {
			t.Errorf("InScope(%v, %q) = %v, want %v", tt.scope, tt.file, got, tt.want)
		}
	}
}

func TestCheckChanges(t *testing.T) {
	t.Parallel()
	pol := DefaultPolicy()
	scope := []string{"internal", "docs"}

	v := CheckChanges(scope, RiskL1, pol, "app", []string{"docs/a.md", "internal/x/y.go"})
	if !v.Empty() {
		t.Fatalf("changes inside scope and risk: %+v", v)
	}

	v = CheckChanges(scope, RiskL1, pol, "app", []string{"docs/a.md", "web/x.ts", "Makefile", "web/x.ts"})
	if !slices.Equal(v.Outside, []string{"Makefile", "web/x.ts"}) {
		t.Fatalf("outside = %v", v.Outside)
	}

	// Inside the approved directory, but a file the policy rates above the approved risk.
	v = CheckChanges(scope, RiskL1, pol, "app", []string{"internal/auth/login.go"})
	if len(v.Outside) != 0 || len(v.Sensitive) != 1 || v.Sensitive[0].Category != CategorySecurity {
		t.Fatalf("sensitive = %+v", v)
	}
	if v.Empty() {
		t.Fatal("a sensitive change must stop the step")
	}

	// An L3 approval already covers it.
	if v := CheckChanges(scope, RiskL3, pol, "app", []string{"internal/auth/login.go"}); !v.Empty() {
		t.Fatalf("L3-approved change flagged: %+v", v)
	}
}
