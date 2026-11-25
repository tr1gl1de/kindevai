package domain

import (
	"errors"
	"slices"
	"testing"
)

func TestSwitchesDefaultToEnabled(t *testing.T) {
	t.Parallel()
	var s Switches
	if !s.Enabled(AgentClaude) || !s.Enabled(AgentCodex) {
		t.Fatal("a CLI without a stored switch is enabled")
	}
	if s.Enabled("zcode") {
		t.Fatal("an unknown CLI is never enabled")
	}
	s = Switches{AgentClaude: false}
	if s.Enabled(AgentClaude) || !s.Enabled(AgentCodex) {
		t.Fatalf("stored switch ignored: %v", s)
	}
}

// Acceptance: a project allowlist cannot enable a CLI the Owner disabled globally.
func TestProjectCannotEnableGloballyDisabledCLI(t *testing.T) {
	t.Parallel()
	p := Policy{Global: Switches{AgentCodex: false}, Project: Switches{AgentCodex: true}}
	if p.Allowed(AgentCodex) {
		t.Fatal("codex disabled globally is allowed by the project switch")
	}
	if !p.Allowed(AgentClaude) {
		t.Fatal("claude enabled on both levels is not allowed")
	}
	p = Policy{Project: Switches{AgentClaude: false}}
	if p.Allowed(AgentClaude) {
		t.Fatal("the project disabled claude, yet it is allowed")
	}
	if err := CheckProjectSwitch(Switches{AgentCodex: false}, AgentCodex, true); !errors.Is(err, ErrDisabledGlobally) {
		t.Fatalf("enabling a globally disabled CLI in a project: %v, want ErrDisabledGlobally", err)
	}
	if err := CheckProjectSwitch(Switches{AgentCodex: false}, AgentCodex, false); err != nil {
		t.Fatalf("disabling is always allowed: %v", err)
	}
}

// Acceptance: primary + ordered fallback; a forbidden CLI is never in the chain (no silent fallback).
func TestChainSkipsForbiddenCLIs(t *testing.T) {
	t.Parallel()
	b := Binding{Role: RoleImplementer, Primary: Choice{Agent: AgentCodex, Model: "gpt-5"},
		Fallback: []Choice{{Agent: AgentClaude, Model: "opus"}, {Agent: AgentClaude}}}
	p := Policy{Binding: b}
	got, skipped := p.Chain("")
	want := []Choice{{AgentCodex, "gpt-5"}, {AgentClaude, "opus"}, {AgentClaude, ""}}
	if !slices.Equal(got, want) || len(skipped) != 0 {
		t.Fatalf("chain %v skipped %v, want %v", got, skipped, want)
	}
	p.Global = Switches{AgentCodex: false}
	got, skipped = p.Chain("")
	if !slices.Equal(got, want[1:]) || !slices.Equal(skipped, []string{AgentCodex}) {
		t.Fatalf("chain with codex disabled: %v skipped %v", got, skipped)
	}
	p.Project = Switches{AgentClaude: false}
	if got, skipped = p.Chain(""); len(got) != 0 || !slices.Equal(skipped, []string{AgentClaude, AgentCodex}) {
		t.Fatalf("every CLI disabled: chain %v skipped %v", got, skipped)
	}
}

// Acceptance: the reviewer prefers a CLI other than the author's, and falls back to the author's only when no
// other is permitted.
func TestReviewerDifferentFromAuthor(t *testing.T) {
	t.Parallel()
	b := Binding{Role: RoleReviewer, Strategy: StrategyDifferentFromAuthor,
		Primary: Choice{Agent: AgentClaude}, Fallback: []Choice{{Agent: AgentCodex}}}
	p := Policy{Binding: b}
	if got, _ := p.Chain(AgentClaude); !slices.Equal(got, []Choice{{Agent: AgentCodex}, {Agent: AgentClaude}}) {
		t.Fatalf("author claude: %v, want codex first", got)
	}
	if got, _ := p.Chain(AgentCodex); !slices.Equal(got, []Choice{{Agent: AgentClaude}, {Agent: AgentCodex}}) {
		t.Fatalf("author codex: %v, want claude first", got)
	}
	p.Global = Switches{AgentCodex: false}
	if got, _ := p.Chain(AgentClaude); !slices.Equal(got, []Choice{{Agent: AgentClaude}}) {
		t.Fatalf("only the author's CLI is permitted: %v", got)
	}
	b.Strategy = StrategyOrdered
	if got, _ := (Policy{Binding: b}).Chain(AgentClaude); got[0].Agent != AgentClaude {
		t.Fatalf("ordered strategy reordered the chain: %v", got)
	}
}

func TestBindingCheck(t *testing.T) {
	t.Parallel()
	ok := Binding{Role: RoleImplementer, Strategy: StrategyOrdered, Primary: Choice{Agent: AgentClaude, Model: "claude-opus-4-1"},
		Fallback: []Choice{{Agent: AgentCodex, Model: "gpt-5.1-codex"}}}
	if err := ok.Check(); err != nil {
		t.Fatalf("valid binding: %v", err)
	}
	cases := map[string]struct {
		b     Binding
		field string
	}{
		"unknown role":       {Binding{Role: "architect", Primary: Choice{Agent: AgentClaude}}, "role"},
		"unknown agent":      {Binding{Role: RoleImplementer, Primary: Choice{Agent: "zcode"}}, "primary.agent"},
		"bad model":          {Binding{Role: RoleImplementer, Primary: Choice{Agent: AgentClaude, Model: "--dangerous"}}, "primary.model"},
		"duplicate fallback": {Binding{Role: RoleImplementer, Primary: Choice{Agent: AgentClaude}, Fallback: []Choice{{Agent: AgentClaude}}}, "fallback[0]"},
		"too many fallbacks": {Binding{Role: RoleImplementer, Primary: Choice{Agent: AgentClaude}, Fallback: []Choice{
			{AgentCodex, "a"}, {AgentCodex, "b"}, {AgentCodex, "c"}, {AgentCodex, "d"}, {AgentCodex, "e"}}}, "fallback"},
		"strategy on implementer": {Binding{Role: RoleImplementer, Strategy: StrategyDifferentFromAuthor, Primary: Choice{Agent: AgentClaude}}, "strategy"},
		"unknown strategy":        {Binding{Role: RoleReviewer, Strategy: "random", Primary: Choice{Agent: AgentClaude}}, "strategy"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var ve *ValidationError
			if err := c.b.Check(); !errors.As(err, &ve) || ve.Field != c.field {
				t.Fatalf("Check() = %v, want a validation error on %s", err, c.field)
			}
		})
	}
}

func TestDefaultBindings(t *testing.T) {
	t.Parallel()
	for _, r := range Roles {
		b := DefaultBinding(r)
		if err := b.Check(); err != nil {
			t.Fatalf("default binding of %s: %v", r, err)
		}
	}
	if DefaultBinding(RoleReviewer).Strategy != StrategyDifferentFromAuthor {
		t.Fatal("the default reviewer binding is not different-from-author")
	}
}

func TestBindingNormalize(t *testing.T) {
	t.Parallel()
	b := Binding{Role: " reviewer ", Primary: Choice{Agent: " claude ", Model: " opus "}, Fallback: nil}.Normalize()
	if b.Role != RoleReviewer || b.Primary != (Choice{AgentClaude, "opus"}) || b.Fallback == nil || b.Strategy != StrategyOrdered {
		t.Fatalf("normalized: %+v", b)
	}
}
