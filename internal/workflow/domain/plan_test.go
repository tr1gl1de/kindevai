package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var repos = []string{"api", "web"}

func samplePlan() Plan {
	return Plan{
		Summary: "Login page", Risk: RiskL1, MaxParallel: 2,
		Steps: []PlanStep{
			{Key: "backend", Title: "API", Repository: "api", Scope: []string{"internal/auth/", "cmd/"}},
			{Key: "frontend", Title: "UI", Repository: "web", DependsOn: []string{"backend"}, Capabilities: []string{"node", "go"}},
			{Key: "docs", Title: "Docs", Repository: "web", DependsOn: []string{"frontend", "backend", "frontend"}},
		},
	}
}

func TestCanonicalJSONIsSortedCompactAndNormalized(t *testing.T) {
	t.Parallel()
	got, err := samplePlan().CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"maxParallel":2,"risk":"L1","steps":[` +
		`{"capabilities":[],"dependsOn":[],"key":"backend","repository":"api","scope":["cmd/","internal/auth/"],"title":"API"},` +
		`{"capabilities":["go","node"],"dependsOn":["backend"],"key":"frontend","repository":"web","scope":[],"title":"UI"},` +
		`{"capabilities":[],"dependsOn":["backend","frontend"],"key":"docs","repository":"web","scope":[],"title":"Docs"}` +
		`],"summary":"Login page"}`
	if string(got) != want {
		t.Fatalf("canonical JSON\n got %s\nwant %s", got, want)
	}
}

func TestPlanHashIsSHA256OfTheCanonicalJSON(t *testing.T) {
	t.Parallel()
	p := samplePlan()
	c, _ := p.CanonicalJSON()
	sum := sha256.Sum256(c)
	h, err := p.Hash()
	if err != nil || h != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %s, %v", h, err)
	}
	same := samplePlan()
	same.Steps[2].DependsOn = []string{"backend", "frontend"} // order and duplicates of sets do not matter
	same.Steps[0].Scope = []string{"cmd/", "internal/auth/", "cmd/"}
	same.Summary = "  Login page "
	if h2, _ := same.Hash(); h2 != h {
		t.Fatal("equivalent plans must hash equally")
	}
	changed := samplePlan()
	changed.Steps[1].Title = "UI!"
	if h3, _ := changed.Hash(); h3 == h {
		t.Fatal("a changed plan must change the hash")
	}
	special := samplePlan()
	special.Summary = "<a> & b"
	if c, _ := special.CanonicalJSON(); !strings.Contains(string(c), `"summary":"<a> & b"`) {
		t.Fatalf("HTML characters are not escaped: %s", c)
	}
}

func TestParsePlanIsStrict(t *testing.T) {
	t.Parallel()
	c, _ := samplePlan().CanonicalJSON()
	p, err := ParsePlan(c)
	if err != nil {
		t.Fatal(err)
	}
	if h1, _ := p.Hash(); h1 != mustHash(t, samplePlan()) {
		t.Fatal("parse(canonical) must round-trip")
	}
	for name, in := range map[string]string{
		"unknown field":  `{"steps":[],"extra":1}`,
		"trailing data":  `{"steps":[]} {}`,
		"not an object":  `[]`,
		"too large":      `{"summary":"` + strings.Repeat("x", MaxPlanBytes) + `"}`,
		"wrong type":     `{"maxParallel":"2"}`,
		"invalid syntax": `{`,
	} {
		if _, err := ParsePlan([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func mustHash(t *testing.T, p Plan) string {
	t.Helper()
	h, err := p.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestValidatePlanDAG(t *testing.T) {
	t.Parallel()
	if err := ValidatePlan(samplePlan(), repos); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(p *Plan)
		field  string
		code   string
	}{
		{"no steps", func(p *Plan) { p.Steps = nil }, "steps", "steps_required"},
		{"too many steps", func(p *Plan) {
			for i := range MaxPlanSteps {
				p.Steps = append(p.Steps, PlanStep{Key: fmt.Sprintf("s%d", i), Title: "x", Repository: "api"})
			}
		}, "steps", "too_many_steps"},
		{"duplicate key", func(p *Plan) { p.Steps[1].Key = "backend" }, "steps[1].key", "duplicate_key"},
		{"bad key", func(p *Plan) { p.Steps[0].Key = "Back End" }, "steps[0].key", "key_format"},
		{"empty title", func(p *Plan) { p.Steps[0].Title = " " }, "steps[0].title", "title_format"},
		{"unknown dependency", func(p *Plan) { p.Steps[1].DependsOn = []string{"nope"} }, "steps[1].dependsOn", "unknown_dependency"},
		{"self dependency", func(p *Plan) { p.Steps[0].DependsOn = []string{"backend"} }, "steps[0].dependsOn", "self_dependency"},
		{"two-step cycle", func(p *Plan) { p.Steps[0].DependsOn = []string{"frontend"} }, "steps", "cycle"},
		{"three-step cycle", func(p *Plan) { p.Steps[0].DependsOn = []string{"docs"} }, "steps", "cycle"},
		{"foreign repository", func(p *Plan) { p.Steps[2].Repository = "other-team/secret" }, "steps[2].repository", "repository_not_in_project"},
		{"no repository", func(p *Plan) { p.Steps[2].Repository = "" }, "steps[2].repository", "repository_not_in_project"},
		{"parallelism too high", func(p *Plan) { p.MaxParallel = MaxParallelLimit + 1 }, "maxParallel", "parallelism_limit"},
		{"negative parallelism", func(p *Plan) { p.MaxParallel = -1 }, "maxParallel", "parallelism_limit"},
		{"unknown risk", func(p *Plan) { p.Risk = "L4" }, "risk", "risk_level"},
		{"unknown step role", func(p *Plan) { p.Steps[1].Role = "architect" }, "steps[1].role", "role_unknown"},
		{"absolute scope", func(p *Plan) { p.Steps[0].Scope = []string{"/etc/passwd"} }, "steps[0].scope", "scope_path"},
		{"escaping scope", func(p *Plan) { p.Steps[0].Scope = []string{"src/../../x"} }, "steps[0].scope", "scope_path"},
		{"bad capability", func(p *Plan) { p.Steps[0].Capabilities = []string{"rm -rf"} }, "steps[0].capabilities", "capability_format"},
		{"long summary", func(p *Plan) { p.Summary = strings.Repeat("x", 4001) }, "summary", "summary_format"},
		{"canonical JSON over the size limit", func(p *Plan) {
			for i := range 90 {
				s := PlanStep{Key: fmt.Sprintf("big%d", i), Title: "x", Repository: "api"}
				for j := range 10 {
					s.Scope = append(s.Scope, fmt.Sprintf("%s/%d", strings.Repeat("d", 400), j))
				}
				p.Steps = append(p.Steps, s)
			}
		}, "plan", "plan_too_large"},
	}
	for _, tt := range tests {
		p := samplePlan()
		tt.mutate(&p)
		err := ValidatePlan(p, repos)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != tt.field || ve.Code != tt.code {
			t.Errorf("%s: err = %v, want %s/%s", tt.name, err, tt.field, tt.code)
		}
	}
	zero := samplePlan()
	zero.MaxParallel = 0
	if err := ValidatePlan(zero, repos); err != nil || zero.Normalize().MaxParallel != 1 {
		t.Fatalf("maxParallel 0 defaults to 1: %v", err)
	}
}

func TestPlanRevisionIsImmutable(t *testing.T) {
	t.Parallel()
	p := samplePlan()
	rev, err := NewPlanRevision("t1", 1, p, RiskL2, repos, "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if rev.TaskID() != "t1" || rev.Number() != 1 || rev.Risk() != RiskL2 || rev.CreatedBy() != "u1" ||
		!rev.CreatedAt().Equal(t0) || rev.Hash() != mustHash(t, p) {
		t.Fatalf("revision = %+v", rev)
	}
	p.Steps[0].Title = "changed by the caller"
	got := rev.Plan()
	got.Steps[1].DependsOn[0] = "mutated"
	got.Steps = append(got.Steps[:0], PlanStep{})
	if again := rev.Plan(); again.Steps[0].Title != "API" || again.Steps[1].DependsOn[0] != "backend" || len(again.Steps) != 3 {
		t.Fatalf("revision changed through an alias: %+v", again)
	}
	if rev.Hash() != mustHash(t, rev.Plan()) {
		t.Fatal("hash no longer matches the plan")
	}
}

func TestNewPlanRevisionChecks(t *testing.T) {
	t.Parallel()
	if _, err := NewPlanRevision("t1", 1, samplePlan(), RiskL0, repos, "u1", t0); err == nil {
		t.Fatal("the effective risk can only raise the proposed one (L1 -> L0 rejected)")
	}
	if _, err := NewPlanRevision("t1", 0, samplePlan(), RiskL1, repos, "u1", t0); err == nil {
		t.Fatal("revision numbers start at 1")
	}
	bad := samplePlan()
	bad.Steps[0].DependsOn = []string{"docs"}
	if _, err := NewPlanRevision("t1", 1, bad, RiskL1, repos, "u1", t0); err == nil {
		t.Fatal("an invalid DAG must not become a revision")
	}
}

func TestRestorePlanRevisionVerifiesTheHash(t *testing.T) {
	t.Parallel()
	rev, _ := NewPlanRevision("t1", 3, samplePlan(), RiskL1, repos, "u1", t0)
	c, _ := rev.Plan().CanonicalJSON()
	got, err := RestorePlanRevision("t1", 3, c, rev.Hash(), RiskL1, "u1", t0)
	if err != nil || got.Hash() != rev.Hash() || got.Number() != 3 {
		t.Fatalf("restore = %+v, %v", got, err)
	}
	if _, err := RestorePlanRevision("t1", 3, c, strings.Repeat("0", 64), RiskL1, "u1", t0); !errors.Is(err, ErrPlanHashMismatch) {
		t.Fatalf("tampered hash = %v", err)
	}
}

// KIN-33: a step may name its platform role (implementer by default, reviewer); a plan without roles keeps its
// canonical JSON and hash.
func TestPlanStepRole(t *testing.T) {
	t.Parallel()
	p := samplePlan()
	before, err := p.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "role") {
		t.Fatalf("a plan without roles encodes a role: %s", before)
	}
	p.Steps[2].Role = " reviewer "
	if err := ValidatePlan(p, repos); err != nil {
		t.Fatalf("reviewer step: %v", err)
	}
	n := p.Normalize()
	if n.Steps[2].Role != StepRoleReviewer || n.Steps[0].StepRole() != StepRoleImplementer {
		t.Fatalf("roles: %q, %q", n.Steps[2].Role, n.Steps[0].StepRole())
	}
	got, _ := p.CanonicalJSON()
	if !strings.Contains(string(got), `"repository":"web","role":"reviewer","scope"`) {
		t.Fatalf("role not in canonical key order: %s", got)
	}
	if _, err := ParsePlan(got); err != nil {
		t.Fatalf("parsing a plan with a role: %v", err)
	}
}
