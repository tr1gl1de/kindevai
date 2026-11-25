package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Risk is the autonomy/risk level of a plan (KIN-A-14 "Риски").
type Risk string

const (
	RiskL0 Risk = "L0"
	RiskL1 Risk = "L1"
	RiskL2 Risk = "L2"
	RiskL3 Risk = "L3"
)

// ParseRisk accepts exactly L0..L3.
func ParseRisk(s string) (Risk, error) {
	if r := Risk(s); r.Valid() {
		return r, nil
	}
	return "", invalid("risk", "risk_level")
}

// Valid reports a known level.
func (r Risk) Valid() bool { return r == RiskL0 || r == RiskL1 || r == RiskL2 || r == RiskL3 }

// AtLeast reports r >= o.
func (r Risk) AtLeast(o Risk) bool { return r >= o } // "L0" < "L1" < "L2" < "L3" lexically

// RequiresApproval: L2/L3 plans need the Owner's approval of the revision before running.
func (r Risk) RequiresApproval() bool { return r == RiskL2 || r == RiskL3 }

// IntegrateDevAutoApproved: L0/L1 merge into dev on policy; L2/L3 need a separate integrate_dev approval
// (owner decision, KIN-A-11).
func (r Risk) IntegrateDevAutoApproved() bool { return r == RiskL0 || r == RiskL1 }

// Plan limits.
const (
	MaxPlanSteps     = 100
	MaxParallelLimit = 8 // platform cap for a plan's maxParallel
	MaxPlanBytes     = 256 << 10
	maxSummaryLen    = 4000
	maxListItems     = 64
	maxImpactItemLen = 500 // runes of one impact description
)

// Plan is the planner's output for a task: a DAG of steps. Fields are declared in key order so that the JSON
// encoding of a normalized plan is canonical (sorted keys, no insignificant whitespace).
//
// The impact lists (KIN-34, schema v1: API/protocol contracts the plan changes, documentation it affects,
// database migrations it adds, tests that prove it) are sets of short descriptions. They are omitted when empty,
// so plans without them keep their canonical JSON and hash.
type Plan struct {
	Contracts   []string   `json:"contracts,omitempty"`
	Docs        []string   `json:"docs,omitempty"`
	MaxParallel int        `json:"maxParallel"` // steps that may hold a runner at once; 0 means 1
	Migrations  []string   `json:"migrations,omitempty"`
	Risk        Risk       `json:"risk"`  // the risk the planner proposed; policy may only raise it
	Steps       []PlanStep `json:"steps"` // presentation order; execution order comes from dependsOn
	Summary     string     `json:"summary"`
	Tests       []string   `json:"tests,omitempty"`
}

// PlanStep is one node of the plan DAG.
type PlanStep struct {
	Capabilities []string `json:"capabilities"` // runner capabilities the step needs (a set)
	DependsOn    []string `json:"dependsOn"`    // keys of steps that must succeed first (a set)
	Key          string   `json:"key"`
	Repository   string   `json:"repository"` // a repository of the task's project
	// Role is the platform role that runs the step (KIN-33): "" (implementer) or "reviewer". Omitted when empty,
	// so plans without roles keep their canonical JSON and hash.
	Role  string   `json:"role,omitempty"`
	Scope []string `json:"scope"` // repository-relative paths the step may change (a set)
	Title string   `json:"title"`
}

// Step roles (KIN-33): the platform role whose CLI binding runs a step.
const (
	StepRoleImplementer = "implementer"
	StepRoleReviewer    = "reviewer"
)

// StepRole is the step's role, implementer when the plan names none.
func (s PlanStep) StepRole() string {
	if s.Role == "" {
		return StepRoleImplementer
	}
	return s.Role
}

func normSet(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, strings.TrimSpace(x))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Normalize returns a deep copy with trimmed strings, sorted de-duplicated sets, non-nil slices and maxParallel
// 0 replaced by 1. Equivalent plans normalize to the same value.
func (p Plan) Normalize() Plan {
	n := Plan{MaxParallel: p.MaxParallel, Risk: p.Risk, Summary: strings.TrimSpace(p.Summary),
		Steps: make([]PlanStep, 0, len(p.Steps)), Contracts: normSet(p.Contracts), Docs: normSet(p.Docs),
		Migrations: normSet(p.Migrations), Tests: normSet(p.Tests)}
	if n.MaxParallel == 0 {
		n.MaxParallel = 1
	}
	for _, s := range p.Steps {
		n.Steps = append(n.Steps, PlanStep{
			Capabilities: normSet(s.Capabilities), DependsOn: normSet(s.DependsOn), Key: strings.TrimSpace(s.Key),
			Repository: strings.TrimSpace(s.Repository), Role: strings.TrimSpace(s.Role), Scope: normSet(s.Scope),
			Title: strings.TrimSpace(s.Title),
		})
	}
	return n
}

// CanonicalJSON is the normalized plan as compact JSON with sorted keys and unescaped HTML characters.
func (p Plan) CanonicalJSON() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p.Normalize()); err != nil {
		return nil, fmt.Errorf("encoding plan: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Hash is planHash: the lowercase hex SHA-256 of CanonicalJSON.
func (p Plan) Hash() (string, error) {
	c, err := p.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// ParsePlan decodes one JSON plan object strictly: bounded size, no unknown fields, no trailing data.
func ParsePlan(data []byte) (Plan, error) {
	bad := invalid("plan", "plan_json")
	if len(data) > MaxPlanBytes {
		return Plan{}, invalid("plan", "plan_too_large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return Plan{}, bad
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Plan{}, bad
	}
	return p, nil
}

var (
	stepKeyRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	capabilityRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
)

func validScopePath(s string) bool {
	if s == "" || len(s) > 512 || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") || !printable(s, 512) {
		return false
	}
	c := path.Clean(s)
	return c != ".." && !strings.HasPrefix(c, "../")
}

// ValidatePlan checks the normalized plan: step fields, unique keys, dependencies that exist and are not the step
// itself, no cycles, every repository among the project's repositories and maxParallel within 1..MaxParallelLimit.
func ValidatePlan(p Plan, projectRepos []string) error {
	n := p.Normalize()
	switch {
	case !n.Risk.Valid():
		return invalid("risk", "risk_level")
	case !printable(n.Summary, maxSummaryLen):
		return invalid("summary", "summary_format")
	case n.MaxParallel < 1 || n.MaxParallel > MaxParallelLimit:
		return invalid("maxParallel", "parallelism_limit")
	case len(n.Steps) == 0:
		return invalid("steps", "steps_required")
	case len(n.Steps) > MaxPlanSteps:
		return invalid("steps", "too_many_steps")
	}
	for _, l := range []struct {
		field string
		xs    []string
	}{{"contracts", n.Contracts}, {"docs", n.Docs}, {"migrations", n.Migrations}, {"tests", n.Tests}} {
		field, xs := l.field, l.xs
		if len(xs) > maxListItems || slices.ContainsFunc(xs, func(x string) bool {
			return x == "" || strings.ContainsAny(x, "\n\t") || !printable(x, maxImpactItemLen)
		}) {
			return invalid(field, "impact_format")
		}
	}
	keys := make(map[string]bool, len(n.Steps))
	for i, s := range n.Steps {
		f := func(name string) string { return fmt.Sprintf("steps[%d].%s", i, name) }
		switch {
		case !stepKeyRe.MatchString(s.Key):
			return invalid(f("key"), "key_format")
		case keys[s.Key]:
			return invalid(f("key"), "duplicate_key")
		case s.Title == "" || strings.ContainsAny(s.Title, "\n\t") || !printable(s.Title, MaxTitleLen):
			return invalid(f("title"), "title_format")
		case s.Repository == "" || !slices.Contains(projectRepos, s.Repository):
			return invalid(f("repository"), "repository_not_in_project")
		case len(s.Capabilities) > maxListItems || slices.ContainsFunc(s.Capabilities, func(c string) bool { return !capabilityRe.MatchString(c) }):
			return invalid(f("capabilities"), "capability_format")
		case s.Role != "" && s.Role != StepRoleImplementer && s.Role != StepRoleReviewer:
			return invalid(f("role"), "role_unknown")
		case len(s.Scope) > maxListItems || slices.ContainsFunc(s.Scope, func(c string) bool { return !validScopePath(c) }):
			return invalid(f("scope"), "scope_path")
		}
		keys[s.Key] = true
	}
	for i, s := range n.Steps {
		for _, d := range s.DependsOn {
			if d == s.Key {
				return invalid(fmt.Sprintf("steps[%d].dependsOn", i), "self_dependency")
			}
			if !keys[d] {
				return invalid(fmt.Sprintf("steps[%d].dependsOn", i), "unknown_dependency")
			}
		}
	}
	if hasCycle(n.Steps) {
		return invalid("steps", "cycle")
	}
	// The stored canonical form must parse again (ParsePlan's size cap), or the revision could never be loaded.
	if c, err := n.CanonicalJSON(); err != nil || len(c) > MaxPlanBytes {
		return invalid("plan", "plan_too_large")
	}
	return nil
}

// hasCycle runs Kahn's algorithm: a DAG removes every node.
func hasCycle(steps []PlanStep) bool {
	indegree := make(map[string]int, len(steps))
	dependents := make(map[string][]string, len(steps))
	for _, s := range steps {
		indegree[s.Key] += 0
		for _, d := range s.DependsOn {
			indegree[s.Key]++
			dependents[d] = append(dependents[d], s.Key)
		}
	}
	var queue []string
	for _, s := range steps {
		if indegree[s.Key] == 0 {
			queue = append(queue, s.Key)
		}
	}
	removed := 0
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		removed++
		for _, d := range dependents[k] {
			if indegree[d]--; indegree[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	return removed != len(steps)
}

// PlanRevision is one immutable version of a task's plan. Approvals bind to (task, number, hash): a plan chat
// (KIN-43) creates a new revision instead of changing one.
type PlanRevision struct {
	taskID    string
	number    int
	plan      Plan
	hash      string
	risk      Risk
	createdBy string
	createdAt time.Time
}

// NewPlanRevision validates the plan against the project's repositories and freezes its normalized form. risk is
// the effective level after policy, which may only raise the planner's proposal.
func NewPlanRevision(taskID string, number int, p Plan, risk Risk, projectRepos []string, createdBy string,
	now time.Time) (PlanRevision, error) {
	if number < 1 {
		return PlanRevision{}, invalid("planRevision", "revision_number")
	}
	if err := ValidatePlan(p, projectRepos); err != nil {
		return PlanRevision{}, err
	}
	if !risk.Valid() || !risk.AtLeast(p.Risk) {
		return PlanRevision{}, invalid("risk", "risk_below_proposed")
	}
	n := p.Normalize()
	h, err := n.Hash()
	if err != nil {
		return PlanRevision{}, err
	}
	return PlanRevision{taskID: taskID, number: number, plan: n, hash: h, risk: risk, createdBy: createdBy,
		createdAt: now}, nil
}

// RestorePlanRevision rebuilds a stored revision and verifies that the plan still hashes to hash.
func RestorePlanRevision(taskID string, number int, planJSON []byte, hash string, risk Risk, createdBy string,
	createdAt time.Time) (PlanRevision, error) {
	p, err := ParsePlan(planJSON)
	if err != nil {
		return PlanRevision{}, fmt.Errorf("plan revision %s/%d: %w", taskID, number, err)
	}
	n := p.Normalize()
	got, err := n.Hash()
	if err != nil {
		return PlanRevision{}, err
	}
	if got != hash {
		return PlanRevision{}, fmt.Errorf("plan revision %s/%d: %w", taskID, number, ErrPlanHashMismatch)
	}
	return PlanRevision{taskID: taskID, number: number, plan: n, hash: hash, risk: risk, createdBy: createdBy,
		createdAt: createdAt}, nil
}

func (r PlanRevision) TaskID() string       { return r.taskID }
func (r PlanRevision) Number() int          { return r.number }
func (r PlanRevision) Hash() string         { return r.hash }
func (r PlanRevision) Risk() Risk           { return r.risk }
func (r PlanRevision) CreatedBy() string    { return r.createdBy }
func (r PlanRevision) CreatedAt() time.Time { return r.createdAt }

// Plan returns a deep copy of the frozen plan.
func (r PlanRevision) Plan() Plan { return r.plan.Normalize() }
