// Package domain holds the agent router's rules (KIN-33, KB KIN-A-15 "Маршрутизация"): which AI CLIs are enabled
// globally and in a project, which CLI and model serve each platform role (primary + ordered fallback), and the
// permitted chain of choices for one step. A CLI disabled at any level is never part of a chain: there is no
// silent fallback to a forbidden agent.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The AI CLIs the platform routes to (ZCode is post-MVP, KB KIN-A-11). The names equal the runner capability
// (tool) names and the execution.assign adapter values.
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// Agents lists the known CLIs in display order.
var Agents = []string{AgentClaude, AgentCodex}

// KnownAgent reports a CLI the platform can route to.
func KnownAgent(a string) bool { return slices.Contains(Agents, a) }

// Role is a platform role a CLI may be bound to (MVP set: KB KIN-A-15 lists more, they map onto these three).
type Role string

const (
	RolePlanner     Role = "planner"
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
)

// Roles lists the roles in display order.
var Roles = []Role{RolePlanner, RoleImplementer, RoleReviewer}

// Valid reports a known role.
func (r Role) Valid() bool { return slices.Contains(Roles, r) }

// Strategy orders a binding's chain.
type Strategy string

const (
	// StrategyOrdered: primary, then the fallbacks in order.
	StrategyOrdered Strategy = "ordered"
	// StrategyDifferentFromAuthor (reviewer only): choices whose CLI differs from the author's come first, in
	// binding order; the author's CLI is used only when no other is permitted (independent review when possible).
	StrategyDifferentFromAuthor Strategy = "different-from-author"
)

// MaxFallbacks bounds a binding's fallback list.
const MaxFallbacks = 4

var (
	// ErrNotFound: no such project.
	ErrNotFound = errors.New("not found")
	// ErrForbidden: the principal may not manage this scope.
	ErrForbidden = errors.New("forbidden")
	// ErrDisabledGlobally: a project switch cannot enable a CLI the Owner disabled globally.
	ErrDisabledGlobally = errors.New("the CLI is disabled globally")
)

// ValidationError is a rejected input field with a stable code.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

func invalid(field, code string) error { return &ValidationError{Field: field, Code: code} }

// modelRe is the runner's model check (runner session domain): the value reaches the CLI's argv.
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,127}$`)

// Choice is one CLI with its model ("" = the CLI's default model).
type Choice struct {
	Agent string
	Model string
}

func (c Choice) check(field string) error {
	switch {
	case !KnownAgent(c.Agent):
		return invalid(field+".agent", "agent_unknown")
	case c.Model != "" && !modelRe.MatchString(c.Model):
		return invalid(field+".model", "model_format")
	}
	return nil
}

// Binding says which CLIs serve a role: the primary, then the ordered fallbacks.
type Binding struct {
	Role     Role
	Primary  Choice
	Fallback []Choice
	Strategy Strategy
}

// Normalize trims the fields and fills the defaults (no fallback, ordered strategy).
func (b Binding) Normalize() Binding {
	trim := func(c Choice) Choice {
		return Choice{Agent: strings.TrimSpace(c.Agent), Model: strings.TrimSpace(c.Model)}
	}
	n := Binding{Role: Role(strings.TrimSpace(string(b.Role))), Primary: trim(b.Primary),
		Fallback: make([]Choice, 0, len(b.Fallback)), Strategy: Strategy(strings.TrimSpace(string(b.Strategy)))}
	for _, c := range b.Fallback {
		n.Fallback = append(n.Fallback, trim(c))
	}
	if n.Strategy == "" {
		n.Strategy = StrategyOrdered
	}
	return n
}

// Check validates a binding: known role, CLIs and model names, at most MaxFallbacks distinct fallbacks, and a
// strategy the role supports.
func (b Binding) Check() error {
	if !b.Role.Valid() {
		return invalid("role", "role_unknown")
	}
	if err := b.Primary.check("primary"); err != nil {
		return err
	}
	if len(b.Fallback) > MaxFallbacks {
		return invalid("fallback", "too_many_fallbacks")
	}
	seen := []Choice{b.Primary}
	for i, c := range b.Fallback {
		f := fmt.Sprintf("fallback[%d]", i)
		if err := c.check(f); err != nil {
			return err
		}
		if slices.Contains(seen, c) {
			return invalid(f, "duplicate_choice")
		}
		seen = append(seen, c)
	}
	switch b.Strategy {
	case "", StrategyOrdered:
	case StrategyDifferentFromAuthor:
		if b.Role != RoleReviewer {
			return invalid("strategy", "strategy_reviewer_only")
		}
	default:
		return invalid("strategy", "strategy_unknown")
	}
	return nil
}

// Choices is the binding's chain in binding order.
func (b Binding) Choices() []Choice { return append([]Choice{b.Primary}, b.Fallback...) }

// DefaultBinding is a role's binding until the Owner sets one: Claude Code first, Codex as fallback; the reviewer
// reviews with a CLI other than the author's when possible.
func DefaultBinding(r Role) Binding {
	b := Binding{Role: r, Primary: Choice{Agent: AgentClaude}, Fallback: []Choice{{Agent: AgentCodex}},
		Strategy: StrategyOrdered}
	if r == RoleReviewer {
		b.Primary, b.Fallback = Choice{Agent: AgentCodex}, []Choice{{Agent: AgentClaude}}
		b.Strategy = StrategyDifferentFromAuthor
	}
	return b
}

// Switches are the stored enable/disable switches of one level (agent → enabled). A CLI without a switch is
// enabled; an unknown CLI never is.
type Switches map[string]bool

// Enabled reports whether the level enables agent.
func (s Switches) Enabled(agent string) bool {
	if !KnownAgent(agent) {
		return false
	}
	on, ok := s[agent]
	return !ok || on
}

// CheckProjectSwitch refuses to enable in a project a CLI the Owner disabled globally (disabling is always allowed).
func CheckProjectSwitch(global Switches, agent string, enabled bool) error {
	if !KnownAgent(agent) {
		return invalid("agent", "agent_unknown")
	}
	if enabled && !global.Enabled(agent) {
		return ErrDisabledGlobally
	}
	return nil
}

// Policy is everything routing needs for one role of one project: System switches → Project switches → Role
// binding.
type Policy struct {
	Global  Switches
	Project Switches
	Binding Binding
}

// Allowed reports a CLI enabled both globally and in the project.
func (p Policy) Allowed(agent string) bool {
	return p.Global.Enabled(agent) && p.Project.Enabled(agent)
}

// Chain returns the permitted choices for the role in order, and the CLIs of the binding that were left out
// because they are disabled (sorted, distinct) — for an explicit BLOCKED reason. author is the CLI that did the
// work under review ("" when unknown); only the different-from-author strategy uses it.
func (p Policy) Chain(author string) (chain []Choice, skipped []string) {
	for _, c := range p.Binding.Choices() {
		if !p.Allowed(c.Agent) {
			if !slices.Contains(skipped, c.Agent) {
				skipped = append(skipped, c.Agent)
			}
			continue
		}
		chain = append(chain, c)
	}
	slices.Sort(skipped)
	if p.Binding.Strategy == StrategyDifferentFromAuthor && author != "" {
		slices.SortStableFunc(chain, func(a, b Choice) int {
			return boolRank(a.Agent == author) - boolRank(b.Agent == author)
		})
	}
	return chain, skipped
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
