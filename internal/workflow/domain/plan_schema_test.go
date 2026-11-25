package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPlanImpactFieldsAreCanonicalAndOptional(t *testing.T) {
	t.Parallel()
	p := samplePlan()
	before, _ := p.Hash()
	p.Contracts, p.Docs, p.Migrations, p.Tests = []string{}, nil, []string{}, nil
	if h, _ := p.Hash(); h != before {
		t.Fatal("empty impact lists must not change the hash of a plan without them")
	}
	p.Tests = []string{" login e2e ", "auth unit", "login e2e"}
	p.Migrations = []string{"0026 add sessions table"}
	c, err := p.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	s := string(c)
	if !strings.HasPrefix(s, `{"maxParallel":2,"migrations":["0026 add sessions table"],"risk":"L1",`) ||
		!strings.HasSuffix(s, `"summary":"Login page","tests":["auth unit","login e2e"]}`) {
		t.Fatalf("canonical JSON with impact fields: %s", s)
	}
	parsed, err := ParsePlan(c)
	if err != nil || !slices.Equal(parsed.Tests, []string{"auth unit", "login e2e"}) {
		t.Fatalf("round trip: %+v, %v", parsed, err)
	}
}

func TestValidatePlanChecksImpactFields(t *testing.T) {
	t.Parallel()
	cases := map[string]func(p *Plan){
		"contracts": func(p *Plan) { p.Contracts = []string{"line\nbreak"} },
		"docs":      func(p *Plan) { p.Docs = []string{strings.Repeat("d", maxImpactItemLen+1)} },
		"migrations": func(p *Plan) {
			for i := range maxListItems + 1 {
				p.Migrations = append(p.Migrations, strings.Repeat("m", i+1))
			}
		},
		"tests": func(p *Plan) { p.Tests = []string{"ok", "\x00"} },
	}
	for field, mutate := range cases {
		p := samplePlan()
		mutate(&p)
		var ve *ValidationError
		if err := ValidatePlan(p, repos); !errors.As(err, &ve) || ve.Field != field || ve.Code != "impact_format" {
			t.Errorf("%s: got %v", field, err)
		}
	}
	ok := samplePlan()
	ok.Contracts, ok.Docs = []string{"POST /api/login"}, []string{"README login section"}
	if err := ValidatePlan(ok, repos); err != nil {
		t.Fatal(err)
	}
}

// The JSON Schema versioned in the repository (the planner's output contract) must describe exactly the fields
// ParsePlan accepts, so the planner is never told about a field the parser rejects or the other way round.
func TestPlanSchemaMatchesThePlanType(t *testing.T) {
	t.Parallel()
	var schema struct {
		ID         string                     `json:"$id"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Additional bool                       `json:"additionalProperties"`
		Defs       struct {
			Step struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Additional bool                       `json:"additionalProperties"`
			} `json:"step"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(PlanSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema.ID, PlanSchemaVersion) || schema.Additional || schema.Defs.Step.Additional {
		t.Fatalf("schema id %q, additionalProperties %v/%v", schema.ID, schema.Additional, schema.Defs.Step.Additional)
	}
	if got, want := sortedKeys(schema.Properties), jsonFields(reflect.TypeFor[Plan]()); !slices.Equal(got, want) {
		t.Fatalf("plan properties %v, type fields %v", got, want)
	}
	if got, want := sortedKeys(schema.Defs.Step.Properties), jsonFields(reflect.TypeFor[PlanStep]()); !slices.Equal(got, want) {
		t.Fatalf("step properties %v, type fields %v", got, want)
	}
	for _, r := range schema.Required {
		if _, ok := schema.Properties[r]; !ok {
			t.Fatalf("required %q is no property", r)
		}
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
