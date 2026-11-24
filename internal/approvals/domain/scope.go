package domain

import (
	"path"
	"slices"
	"strings"
)

// validChangedPath: a repository-relative path that does not escape the repository.
func validChangedPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || !printable(p, 4096) {
		return false
	}
	c := path.Clean(p)
	return c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

// InScope reports whether file lies inside one of the approved scope entries: the entry itself, a file below an
// entry directory, "." (the whole repository) or a glob entry that matches. A path escaping the repository is
// never in scope.
func InScope(scope []string, file string) bool {
	if !validChangedPath(file) {
		return false
	}
	file = path.Clean(file)
	for _, e := range scope {
		e = path.Clean(strings.TrimSpace(e))
		switch {
		case e == ".":
			return true
		case strings.ContainsAny(e, globMetaChars):
			if matchSegs(strings.Split(e, "/"), strings.Split(file, "/"), false) {
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
			v.Outside = append(v.Outside, f)
		}
	}
	slices.Sort(v.Outside)
	v.Outside = slices.Compact(v.Outside)
	v.Sensitive = pol.match(inside, func(r Rule) bool { return !approved.AtLeast(r.Risk) })
	return v
}
