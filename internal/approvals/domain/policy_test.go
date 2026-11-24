package domain

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMatchPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"**/auth/**", "internal/auth/service.go", true},
		{"**/auth/**", "auth/x.go", true},
		{"**/auth/**", "internal/auth", true}, // the directory itself
		{"**/auth/**", "internal/authz/x.go", false},
		{"**/auth/**", "internal", false}, // a parent directory with no literal segment matched is not covered
		{"**/*.sql", "db/0001_init.sql", true},
		{"**/*.sql", "init.sql", true},
		{"**/*.sql", "db/init.sqlx", false},
		{"**/migrations/*.sql", "internal/platform/db/migrations", true}, // the scope directory covers matching files
		{"**/migrations/*.sql", "internal/platform/db/migrations/0001.sql", true},
		{"**/migrations/*.sql", "internal/platform/db", false},
		{"web/src/auth/**", "web", true}, // a literal prefix of the pattern: the directory covers it
		{"web/src/auth/**", "api", false},
		{"Dockerfile", "Dockerfile", true},
		{"Dockerfile", "sub/Dockerfile", false},
		{"*/secrets/**", "foo", false}, // only wildcards matched: not a literal prefix
		{"**", "anything/at/all", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"|"+tt.path, func(t *testing.T) {
			t.Parallel()
			if got := MatchPattern(tt.pattern, tt.path); got != tt.want {
				t.Fatalf("MatchPattern(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestNewPolicyValidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules []Rule
		field string
	}{
		{"unknown category", []Rule{{Category: "misc", Risk: RiskL2}}, "rules[0].category"},
		{"duplicate category", []Rule{{Category: CategoryPaths, Risk: RiskL1}, {Category: CategoryPaths, Risk: RiskL2}}, "rules[1].category"},
		{"bad risk", []Rule{{Category: CategoryPaths, Risk: "L9"}}, "rules[0].risk"},
		{"absolute pattern", []Rule{{Category: CategoryPaths, Risk: RiskL2, Patterns: []string{"/etc/**"}}}, "rules[0].patterns"},
		{"parent pattern", []Rule{{Category: CategoryPaths, Risk: RiskL2, Patterns: []string{"../x"}}}, "rules[0].patterns"},
		{"bad glob", []Rule{{Category: CategoryPaths, Risk: RiskL2, Patterns: []string{"a/[b"}}}, "rules[0].patterns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewPolicy(tt.rules)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tt.field {
				t.Fatalf("NewPolicy() error = %v, want validation of %s", err, tt.field)
			}
		})
	}
}

func TestNewPolicyNormalizes(t *testing.T) {
	t.Parallel()
	p, err := NewPolicy([]Rule{{Category: CategoryMigrations, Risk: RiskL3, Patterns: []string{" b/** ", "a/**", "a/**"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != len(AllCategories) {
		t.Fatalf("rules = %d, want one per category", len(p.Rules))
	}
	m := p.Rule(CategoryMigrations)
	if m.Risk != RiskL3 || !slices.Equal(m.Patterns, []string{"a/**", "b/**"}) {
		t.Fatalf("migrations rule = %+v", m)
	}
	if s := p.Rule(CategorySecurity); s.Risk != RiskL0 || len(s.Patterns) != 0 {
		t.Fatalf("missing category must be an empty L0 rule, got %+v", s)
	}
}

func TestDefaultPolicyIsValid(t *testing.T) {
	t.Parallel()
	d := DefaultPolicy()
	if _, err := NewPolicy(d.Rules); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	for _, c := range AllCategories {
		if len(d.Rule(c).Patterns) == 0 {
			t.Errorf("default policy has no patterns for %s", c)
		}
	}
}

func TestEvaluateRaisesButNeverLowers(t *testing.T) {
	t.Parallel()
	pol := DefaultPolicy()
	tests := []struct {
		name     string
		proposed Risk
		paths    []string
		want     Risk
		category Category
	}{
		{"docs stay L0", RiskL0, []string{"docs/readme.md"}, RiskL0, ""},
		{"migration raises L0 to L2", RiskL0, []string{"internal/platform/db/migrations/0012_x.sql"}, RiskL2, CategoryMigrations},
		{"auth raises L1 to L3", RiskL1, []string{"internal/identity/auth/login.go", "docs/a.md"}, RiskL3, CategorySecurity},
		{"db schema raises to L2", RiskL0, []string{"db/schema.sql"}, RiskL2, CategoryDBSchema},
		{"ci workflows raise to L2", RiskL0, []string{".github/workflows/ci.yml"}, RiskL2, CategoryPaths},
		{"AI L3 stays L3 even with harmless paths", RiskL3, []string{"docs/a.md"}, RiskL3, ""},
		{"AI L2 is not lowered by an L2 match", RiskL2, []string{"migrations/1.sql"}, RiskL2, CategoryMigrations},
		{"invalid proposal counts as L3", "", []string{"docs/a.md"}, RiskL3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			targets := make([]Target, 0, len(tt.paths))
			for _, p := range tt.paths {
				targets = append(targets, Target{Repository: "app", Path: p})
			}
			a := pol.Evaluate(tt.proposed, targets)
			if a.Risk != tt.want {
				t.Fatalf("Evaluate() risk = %s, want %s (findings %+v)", a.Risk, tt.want, a.Findings)
			}
			if tt.proposed.Valid() && !a.Risk.AtLeast(tt.proposed) {
				t.Fatalf("Evaluate() lowered %s to %s", tt.proposed, a.Risk)
			}
			if tt.category != "" && !slices.ContainsFunc(a.Findings, func(f Finding) bool { return f.Category == tt.category }) {
				t.Fatalf("findings %+v lack %s", a.Findings, tt.category)
			}
		})
	}
}

// Property: for every proposal and every policy (including one with only L0 rules), the effective risk is at
// least the proposal. The evaluator has no way to lower a risk.
func TestEvaluateNeverLowersAnyProposal(t *testing.T) {
	t.Parallel()
	low, err := NewPolicy([]Rule{{Category: CategoryPaths, Risk: RiskL0, Patterns: []string{"**"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, pol := range []Policy{DefaultPolicy(), low, {}} {
		for _, r := range AllRisks {
			a := pol.Evaluate(r, []Target{{Repository: "app", Path: "internal/auth/x.go"}, {Repository: "app", Path: "README.md"}})
			if !a.Risk.AtLeast(r) || a.Proposed != r {
				t.Fatalf("policy %+v lowered %s to %s", pol, r, a.Risk)
			}
		}
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	t.Parallel()
	pol := DefaultPolicy()
	in := []Target{{Repository: "b", Path: "internal/auth/x.go"}, {Repository: "a", Path: "migrations/1.sql"},
		{Repository: "a", Path: "internal/auth/y.go"}}
	first := pol.Evaluate(RiskL0, in)
	rev := slices.Clone(in)
	slices.Reverse(rev)
	second := pol.Evaluate(RiskL0, rev)
	if !slices.Equal(first.Findings, second.Findings) || first.Risk != second.Risk {
		t.Fatalf("order changed the result:\n%+v\n%+v", first, second)
	}
}

func TestRiskRouting(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		risk            Risk
		needsApproval   bool
		integrateByRule bool
	}{
		{RiskL0, false, true}, {RiskL1, false, true}, {RiskL2, true, false}, {RiskL3, true, false},
	} {
		if tt.risk.RequiresApproval() != tt.needsApproval || tt.risk.IntegrateDevAutoApproved() != tt.integrateByRule {
			t.Errorf("%s: approval %v, integrate auto %v", tt.risk, tt.risk.RequiresApproval(), tt.risk.IntegrateDevAutoApproved())
		}
	}
}

// Review KIN-24: a scope that may touch anything cannot dodge the policy at plan time.
func TestEvaluateTreatsBroadScopesConservatively(t *testing.T) {
	t.Parallel()
	pol := DefaultPolicy()
	for _, tt := range []struct {
		path string
		want Risk
	}{
		{".", RiskL3},                          // the whole repository
		{"**/au*/**", RiskL3},                  // starts with a wildcard
		{"internal/platform/db/*.sql", RiskL2}, // the literal prefix stands for the glob ("db" is no rule, *.sql is)
		{"internal/auth/*.go", RiskL3},         // literal prefix internal/auth is covered by **/auth/**
		{"docs/*.md", RiskL0},
	} {
		a := pol.Evaluate(RiskL0, []Target{{Repository: "app", Path: tt.path}})
		if a.Risk != tt.want {
			t.Errorf("Evaluate(%q) = %s, want %s (%+v)", tt.path, a.Risk, tt.want, a.Findings)
		}
	}
}

// Review KIN-24: many "**" segments against a deep path must not take exponential time.
func TestMatchingIsPolynomial(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("a/", MaxPathSegments-1) + "c"
	glob := strings.Repeat("**/", 20) + "b"
	start := time.Now()
	for range 50 {
		if MatchPattern(glob, deep) || InScope([]string{glob}, deep) {
			t.Fatal("must not match")
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("matching took %s", d)
	}
	if !MatchPattern("docs/**", strings.Repeat("a/", MaxPathSegments+1)+"x") {
		t.Fatal("a path deeper than the limit counts as matching a policy rule")
	}
}
