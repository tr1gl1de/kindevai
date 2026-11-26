package domain

import (
	"slices"
	"strconv"
	"strings"
)

// PlanSnapshot is a plan revision as the approvals context sees it (translated from the workflow context).
type PlanSnapshot struct {
	Summary     string
	Risk        Risk // as proposed by the planner
	MaxParallel int
	Steps       []StepSnapshot
	// Plan schema v1 lists (KIN-34): tests to run, contracts, migrations and docs the plan touches.
	Tests      []string
	Contracts  []string
	Migrations []string
	Docs       []string
}

// StepSnapshot is one plan step.
type StepSnapshot struct {
	Key          string
	Title        string
	Repository   string
	Role         string // "" (implementer) or "reviewer"
	Scope        []string
	DependsOn    []string
	Capabilities []string
}

// Targets lists every (repository, scope path) of the plan, for the Policy Evaluator.
func (p PlanSnapshot) Targets() []Target {
	var out []Target
	for _, s := range p.Steps {
		for _, sc := range s.Scope {
			out = append(out, Target{Repository: s.Repository, Path: sc})
		}
	}
	return out
}

// ChangeKind classifies a difference between revisions.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
)

// Change is one difference between two plan revisions. Field is a plan field ("summary", "risk", "maxParallel",
// "tests", "contracts", "migrations", "docs"), "step" (a whole step added or removed) or a step field ("title",
// "repository", "role", "scope", "dependsOn", "capabilities"). A list field is compared as a set: Before and After
// are its items joined with ", ", Added and Removed the items it gained and lost.
type Change struct {
	Kind    ChangeKind
	StepKey string
	Field   string
	Before  string
	After   string
	Added   []string
	Removed []string
}

// roleOf is the step's role with the implementer default spelled out, so "" and "implementer" are the same role.
func roleOf(s StepSnapshot) string {
	if s.Role == "" {
		return "implementer"
	}
	return s.Role
}

// minus returns the items of xs that are not in ys, in xs's order.
func minus(xs, ys []string) []string {
	var out []string
	for _, x := range xs {
		if !slices.Contains(ys, x) && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// DiffPlans lists what changed from prev to cur: plan fields, then modified steps in cur's order, removed steps
// in prev's order, added steps in cur's order. Deterministic, for the Approval Center's and the task's revision diff.
func DiffPlans(prev, cur PlanSnapshot) []Change {
	var out []Change
	mod := func(key, field, before, after string) {
		if before != after {
			out = append(out, Change{Kind: ChangeModified, StepKey: key, Field: field, Before: before, After: after})
		}
	}
	mod("", "summary", prev.Summary, cur.Summary)
	mod("", "risk", string(prev.Risk), string(cur.Risk))
	mod("", "maxParallel", strconv.Itoa(prev.MaxParallel), strconv.Itoa(cur.MaxParallel))
	set := func(key, field string, before, after []string) {
		added, removed := minus(after, before), minus(before, after)
		if len(added) > 0 || len(removed) > 0 {
			out = append(out, Change{Kind: ChangeModified, StepKey: key, Field: field, Before: strings.Join(before, ", "),
				After: strings.Join(after, ", "), Added: added, Removed: removed})
		}
	}
	set("", "tests", prev.Tests, cur.Tests)
	set("", "contracts", prev.Contracts, cur.Contracts)
	set("", "migrations", prev.Migrations, cur.Migrations)
	set("", "docs", prev.Docs, cur.Docs)
	find := func(steps []StepSnapshot, key string) (StepSnapshot, bool) {
		i := slices.IndexFunc(steps, func(s StepSnapshot) bool { return s.Key == key })
		if i < 0 {
			return StepSnapshot{}, false
		}
		return steps[i], true
	}
	for _, c := range cur.Steps {
		p, ok := find(prev.Steps, c.Key)
		if !ok {
			continue
		}
		mod(c.Key, "title", p.Title, c.Title)
		mod(c.Key, "repository", p.Repository, c.Repository)
		mod(c.Key, "role", roleOf(p), roleOf(c))
		set(c.Key, "scope", p.Scope, c.Scope)
		set(c.Key, "dependsOn", p.DependsOn, c.DependsOn)
		set(c.Key, "capabilities", p.Capabilities, c.Capabilities)
	}
	for _, p := range prev.Steps {
		if _, ok := find(cur.Steps, p.Key); !ok {
			out = append(out, Change{Kind: ChangeRemoved, StepKey: p.Key, Field: "step", Before: p.Title})
		}
	}
	for _, c := range cur.Steps {
		if _, ok := find(prev.Steps, c.Key); !ok {
			out = append(out, Change{Kind: ChangeAdded, StepKey: c.Key, Field: "step", After: c.Title})
		}
	}
	return out
}
