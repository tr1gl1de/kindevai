package domain

import (
	"path"
	"slices"
	"strconv"
	"strings"
)

// MaxChangedPathLen bounds one reported path (bytes).
const MaxChangedPathLen = 1024

// ValidChangedPath: a printable repository-relative path of at most MaxPathSegments segments that does not
// escape the repository.
func ValidChangedPath(p string) bool {
	if p == "" || len(p) > MaxChangedPathLen || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") ||
		!printable(p, MaxChangedPathLen) || strings.ContainsAny(p, "\n\t") {
		return false
	}
	c := path.Clean(p)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../") && strings.Count(c, "/") < MaxPathSegments
}

// DisplayPath is a reported path safe to store and show: valid paths cleaned, anything else quoted and shortened.
func DisplayPath(p string) string {
	if ValidChangedPath(p) {
		return path.Clean(p)
	}
	if len(p) > 200 {
		p = p[:200]
	}
	return strconv.QuoteToASCII(p)
}

// InScope reports whether file lies inside one of the approved scope entries: the entry itself, a file below an
// entry directory, "." (the whole repository) or a glob entry that matches. A path escaping the repository is
// never in scope.
func InScope(scope []string, file string) bool {
	if !ValidChangedPath(file) {
		return false
	}
	file = path.Clean(file)
	for _, e := range scope {
		e = path.Clean(strings.TrimSpace(e))
		switch {
		case e == ".":
			return true
		case strings.ContainsAny(e, globMetaChars):
			if matchGlob(strings.Split(e, "/"), strings.Split(file, "/"), false) { // the whole glob must match
				return true
			}
		case file == e || strings.HasPrefix(file, e+"/"):
			return true
		}
	}
	return false
}

// Violation is what a step changed beyond its approval: files outside the approved scope, and files inside it
// that the policy rates above the approved risk.
type Violation struct {
	Outside   []string
	Sensitive []Finding
}

// Empty reports no violation.
func (v Violation) Empty() bool { return len(v.Outside) == 0 && len(v.Sensitive) == 0 }

// Paths lists every file of the violation (outside and sensitive), sorted and unique.
func (v Violation) Paths() []string {
	out := slices.Clone(v.Outside)
	for _, f := range v.Sensitive {
		out = append(out, f.Path)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// CheckChanges compares the files a step changed in repository with the step's approved scope and the revision's
// approved risk. Any violation stops the step until a new approval (KIN-A-14: changes beyond the approved scope
// are stopped).
func CheckChanges(scope []string, approved Risk, pol Policy, repository string, changed []string) Violation {
	var v Violation
	var inside []Target
	for _, f := range changed {
		if InScope(scope, f) {
			inside = append(inside, Target{Repository: repository, Path: path.Clean(f)})
		} else {
			v.Outside = append(v.Outside, DisplayPath(f))
		}
	}
	slices.Sort(v.Outside)
	v.Outside = slices.Compact(v.Outside)
	v.Sensitive = pol.match(inside, func(r Rule) bool { return !approved.AtLeast(r.Risk) })
	return v
}
