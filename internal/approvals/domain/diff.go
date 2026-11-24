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
}

// StepSnapshot is one plan step.
type StepSnapshot struct {
	Key          string
	Title        string
	Repository   string
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

// Change is one difference between two plan revisions. Field is "summary", "risk", "maxParallel", "step" (a whole
// step added or removed) or a step field ("title", "repository", "scope", "dependsOn", "capabilities").
type Change struct {
	Kind    ChangeKind
	StepKey string
	Field   string
	Before  string
	After   string
}

// DiffPlans lists what changed from prev to cur: plan fields, then modified steps in cur's order, removed steps
// in prev's order, added steps in cur's order. Deterministic, for the Approval Center's revision diff.
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
	find := func(steps []StepSnapshot, key string) (StepSnapshot, bool) {
		i := slices.IndexFunc(steps, func(s StepSnapshot) bool { return s.Key == key })
		if i < 0 {
			return StepSnapshot{}, false
		}
		return steps[i], true
	}
	list := func(xs []string) string { return strings.Join(xs, ", ") }
	for _, c := range cur.Steps {
		p, ok := find(prev.Steps, c.Key)
		if !ok {
			continue
		}
		mod(c.Key, "title", p.Title, c.Title)
		mod(c.Key, "repository", p.Repository, c.Repository)
		mod(c.Key, "scope", list(p.Scope), list(c.Scope))
		mod(c.Key, "dependsOn", list(p.DependsOn), list(c.DependsOn))
		mod(c.Key, "capabilities", list(p.Capabilities), list(c.Capabilities))
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
