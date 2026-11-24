// Package domain is the approvals bounded context: the deterministic risk Policy Evaluator, the approved-scope
// check, Approval requests and decisions bound to one plan revision, and the diff between plan revisions
// (KIN-A-14 "Риски", "Approval"; KIN-A-12 Approval Center). Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotFound: no such approval, task or project (or the caller may not see it).
	ErrNotFound = errors.New("not found")
	// ErrForbidden: the caller may not perform the action (only the Owner decides approvals).
	ErrForbidden = errors.New("forbidden")
	// ErrStaleRevision: the plan revision/hash is not the one the approval binds to, or a newer revision exists.
	ErrStaleRevision = errors.New("plan revision is not current")
	// ErrAlreadyDecided: the approval was approved or rejected before.
	ErrAlreadyDecided = errors.New("approval already decided")
	// ErrNotReady: the task is not in a state where this approval can take effect (e.g. integrate_dev before
	// verification, or the task was closed).
	ErrNotReady = errors.New("task is not ready for this decision")
)

// ValidationError reports invalid input for one field with a stable code.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

func invalid(field, code string) error { return &ValidationError{Field: field, Code: code} }

// Risk is the autonomy/risk level L0..L3 (KIN-A-14). The approvals context keeps its own value type; the workflow
// context's levels are translated at the boundary.
type Risk string

const (
	RiskL0 Risk = "L0"
	RiskL1 Risk = "L1"
	RiskL2 Risk = "L2"
	RiskL3 Risk = "L3"
)

// AllRisks lists the levels, lowest first.
var AllRisks = []Risk{RiskL0, RiskL1, RiskL2, RiskL3}

// Valid reports a known level.
func (r Risk) Valid() bool { return slices.Contains(AllRisks, r) }

// AtLeast reports r >= o (an unknown level ranks below L0).
func (r Risk) AtLeast(o Risk) bool { return slices.Index(AllRisks, r) >= slices.Index(AllRisks, o) }

// RequiresApproval: L2/L3 plans wait for the Owner's execute approval (task AWAITING_APPROVAL).
func (r Risk) RequiresApproval() bool { return r == RiskL2 || r == RiskL3 }

// IntegrateDevAutoApproved: L0/L1 merge into dev on policy; L2/L3 need a separate Owner integrate_dev approval
// (owner decision 24.11.2026, KIN-A-11).
func (r Risk) IntegrateDevAutoApproved() bool { return r == RiskL0 || r == RiskL1 }

func maxRisk(a, b Risk) Risk {
	if a.AtLeast(b) {
		return a
	}
	return b
}

// Category groups the policy's path rules (KIN-24: paths, migrations, auth/security, DB schema).
type Category string

const (
	CategoryPaths      Category = "paths"
	CategoryMigrations Category = "migrations"
	CategorySecurity   Category = "security"
	CategoryDBSchema   Category = "db_schema"
)

// AllCategories lists the categories in display order.
var AllCategories = []Category{CategoryPaths, CategoryMigrations, CategorySecurity, CategoryDBSchema}

// Limits of a project policy.
const (
	MaxPatterns      = 32
	MaxPatternLen    = 256
	maxFindings      = 100
	globMetaChars    = "*?["
	recursiveSegment = "**"
)

// Rule raises any plan touching one of Patterns to at least Risk.
type Rule struct {
	Category Category
	Risk     Risk
	Patterns []string // repository-relative globs; "**" matches any number of path segments
}

// Policy is a project's deterministic risk policy: exactly one rule per category, in AllCategories order.
type Policy struct {
	Rules []Rule
}

// DefaultPolicy is used for a project that has not configured its own.
func DefaultPolicy() Policy {
	p, _ := NewPolicy([]Rule{
		{Category: CategoryPaths, Risk: RiskL2, Patterns: []string{".github/workflows/**", "**/Dockerfile",
			"deploy/**", "**/*.tf"}},
		{Category: CategoryMigrations, Risk: RiskL2, Patterns: []string{"**/migrations/**", "**/migrate/**", "**/*.sql"}},
		{Category: CategorySecurity, Risk: RiskL3, Patterns: []string{"**/auth/**", "**/security/**", "**/crypto/**",
			"**/secrets/**", "**/*secret*", "**/*.pem", "**/*.key"}},
		{Category: CategoryDBSchema, Risk: RiskL2, Patterns: []string{"**/schema.*", "**/schema/**", "**/*.prisma"}},
	})
	return p
}

// ValidPattern reports a repository-relative glob: no absolute path, no "..", valid path.Match syntax per segment.
func ValidPattern(p string) bool {
	if p == "" || len(p) > MaxPatternLen || !printable(p, MaxPatternLen) || strings.HasPrefix(p, "/") ||
		strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == ".." || seg == "." {
			return false
		}
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}

// NewPolicy validates and normalizes rules: trimmed, sorted, de-duplicated patterns; a category without a rule
// gets an empty L0 rule.
func NewPolicy(rules []Rule) (Policy, error) {
	byCat := make(map[Category]Rule, len(AllCategories))
	for i, r := range rules {
		f := func(name string) string { return fmt.Sprintf("rules[%d].%s", i, name) }
		if !slices.Contains(AllCategories, r.Category) {
			return Policy{}, invalid(f("category"), "category_unknown")
		}
		if _, dup := byCat[r.Category]; dup {
			return Policy{}, invalid(f("category"), "duplicate_category")
		}
		if !r.Risk.Valid() {
			return Policy{}, invalid(f("risk"), "risk_level")
		}
		pats := make([]string, 0, len(r.Patterns))
		for _, p := range r.Patterns {
			p = strings.TrimSpace(p)
			if !ValidPattern(p) {
				return Policy{}, invalid(f("patterns"), "pattern_format")
			}
			pats = append(pats, p)
		}
		slices.Sort(pats)
		pats = slices.Compact(pats)
		if len(pats) > MaxPatterns {
			return Policy{}, invalid(f("patterns"), "too_many_patterns")
		}
		byCat[r.Category] = Rule{Category: r.Category, Risk: r.Risk, Patterns: pats}
	}
	out := Policy{Rules: make([]Rule, 0, len(AllCategories))}
	for _, c := range AllCategories {
		r, ok := byCat[c]
		if !ok {
			r = Rule{Category: c, Risk: RiskL0, Patterns: []string{}}
		}
		out.Rules = append(out.Rules, r)
	}
	return out, nil
}

// Rule returns the rule of category c (an empty L0 rule if absent).
func (p Policy) Rule(c Category) Rule {
	for _, r := range p.Rules {
		if r.Category == c {
			return r
		}
	}
	return Rule{Category: c, Risk: RiskL0}
}

// Target is one path a plan step may change, relative to its repository.
type Target struct {
	Repository string
	Path       string
}

// Finding is one rule that matched a target.
type Finding struct {
	Category   Category
	Risk       Risk
	Pattern    string
	Repository string
	Path       string
}

// Assessment is the Policy Evaluator's result: the effective risk is never below the proposed one.
type Assessment struct {
	Proposed Risk
	Risk     Risk
	Findings []Finding // sorted, at most maxFindings
}

// Evaluate computes the effective risk of a plan proposed at `proposed` that may change targets. The AI's
// proposal is only ever raised: an unknown proposal counts as L3. Findings are deterministic (sorted).
func (p Policy) Evaluate(proposed Risk, targets []Target) Assessment {
	risk := proposed
	if !risk.Valid() {
		risk = RiskL3
	}
	findings := p.match(targets, func(Rule) bool { return true })
	for _, f := range findings {
		risk = maxRisk(risk, f.Risk)
	}
	if len(findings) > maxFindings {
		findings = findings[:maxFindings]
	}
	return Assessment{Proposed: proposed, Risk: risk, Findings: findings}
}

func (p Policy) match(targets []Target, keep func(Rule) bool) []Finding {
	var out []Finding
	for _, t := range targets {
		for _, r := range p.Rules {
			if !keep(r) {
				continue
			}
			for _, pat := range r.Patterns {
				if MatchPattern(pat, t.Path) {
					out = append(out, Finding{Category: r.Category, Risk: r.Risk, Pattern: pat,
						Repository: t.Repository, Path: t.Path})
					break // one finding per rule and target
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b Finding) int {
		return strings.Compare(a.Repository+"\x00"+a.Path+"\x00"+string(a.Category),
			b.Repository+"\x00"+b.Path+"\x00"+string(b.Category))
	})
	return slices.Compact(out)
}

// MatchPattern reports whether a repository path is covered by pattern. The path may be a file or a directory
// from a plan's scope: it matches when the whole pattern matches it, or when it is a directory on the pattern's
// literal prefix (so "internal/db/migrations" is covered by "**/migrations/*.sql" and "web" by "web/src/auth/**").
// A directory reached only through wildcards ("internal" for "**/auth/**") is not covered: the runtime scope
// check rates every changed file again (CheckChanges).
func MatchPattern(pattern, p string) bool {
	p = path.Clean(p)
	if p == "." {
		return pattern == recursiveSegment
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"), false)
}

// matchSegs matches pattern segments against path segments; literal records that a non-wildcard pattern segment
// matched, which lets a path that ends early count as a covering directory.
func matchSegs(pat, segs []string, literal bool) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if len(segs) == 0 {
		if literal {
			return true
		}
		for _, s := range pat {
			if s != recursiveSegment {
				return false
			}
		}
		return true
	}
	if pat[0] == recursiveSegment {
		// zero segments, or consume one and stay on "**"
		return matchSegs(pat[1:], segs, literal) || matchSegs(pat, segs[1:], literal)
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:], literal || !strings.ContainsAny(pat[0], globMetaChars))
}

// printable reports text of at most maxLen runes without control characters other than newline and tab.
func printable(s string, maxLen int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' })
}
