package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// maxGuardBody bounds a request body the guard inspects; bigger write bodies are refused (none of the platform's
// writes come close).
const maxGuardBody = 1 << 20

// writeRule is one write the platform may send; everything else that is not a read is refused (default deny).
type writeRule struct {
	methods string         // space-separated
	path    *regexp.Regexp // matched against the path below the API base, case-insensitively
	branch  int            // submatch holding a branch name in the path, 0 for none
	field   string         // JSON body field naming the target branch or ref, "" for none
	require bool           // the field must be present: without it GitHub writes to the default branch
	merge   bool           // a pull request merge: allowed only with a base checked by AllowMerge
}

var writeRules = []writeRule{
	{methods: "POST", path: regexp.MustCompile(`(?i)^/app/installations/\d+/access_tokens$`)},
	{methods: "POST", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/pulls$`), field: "base", require: true},
	{methods: "PATCH", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/pulls/\d+$`), field: "base"},
	{methods: "PUT", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/pulls/\d+/merge$`), merge: true},
	{methods: "POST", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/merges$`), field: "base", require: true},
	{methods: "POST", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/git/refs$`), field: "ref", require: true},
	{methods: "PATCH DELETE", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/git/refs/heads/(.+)$`), branch: 1},
	{methods: "PUT DELETE", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/contents/.+$`), field: "branch", require: true},
	{methods: "POST", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/git/(blobs|trees|commits)$`)},
	{methods: "POST", path: regexp.MustCompile(`(?i)^/repos/[^/]+/[^/]+/(issues|pulls)/\d+/comments$`)},
}

type mergeKey struct{}

// AllowMerge marks ctx for merging a pull request whose base branch the caller has just read from GitHub: the
// merge request itself does not name the base, so the guard lets a merge through only with this mark, and only for
// a base that is not main/master.
func AllowMerge(ctx context.Context, base string) (context.Context, error) {
	if err := domain.GuardBranch(base); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, mergeKey{}, base), nil
}

// Guard is the transport of every GitHub client (KIN-A-20: main/master are Owner-only). Reads pass. A write passes
// only when it is one of the platform's known writes (default deny: GraphQL, settings, branch protection and any
// unknown endpoint are refused) and it does not create, move, delete or commit to main/master, open or retarget a
// pull request to them, or merge into them. Refused requests never leave the process.
type Guard struct {
	Next   http.RoundTripper
	Prefix string // path of the API base URL (GitHub Enterprise Server: "/api/v3"), stripped before matching
}

// RoundTrip implements http.RoundTripper.
func (g Guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return g.Next.RoundTrip(r)
	}
	path, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil || strings.Contains(path, "//") || strings.Contains(path, "/./") || strings.Contains(path, "/../") ||
		strings.HasSuffix(path, "/..") || strings.HasSuffix(path, "/.") {
		return nil, refuse(r)
	}
	prefix := strings.TrimRight(g.Prefix, "/")
	if prefix != "" {
		if !strings.HasPrefix(strings.ToLower(path), strings.ToLower(prefix)+"/") {
			return nil, refuse(r)
		}
		path = path[len(prefix):]
	}
	for _, rule := range writeRules {
		m := rule.path.FindStringSubmatch(path)
		if m == nil || !strings.Contains(" "+rule.methods+" ", " "+r.Method+" ") {
			continue
		}
		return g.check(r, rule, m)
	}
	return nil, refuse(r)
}

func (g Guard) check(r *http.Request, rule writeRule, m []string) (*http.Response, error) {
	if rule.branch > 0 && domain.IsProtectedBranch(m[rule.branch]) {
		return nil, refuse(r)
	}
	if rule.merge {
		if base, ok := r.Context().Value(mergeKey{}).(string); !ok || domain.IsProtectedBranch(base) {
			return nil, refuse(r)
		}
	}
	if rule.field == "" {
		return g.Next.RoundTrip(r)
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxGuardBody+1))
		_ = r.Body.Close()
		if err != nil || len(body) > maxGuardBody {
			return nil, refuse(r)
		}
	}
	var fields map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, refuse(r)
		}
	}
	v, ok := fields[rule.field].(string)
	if (rule.require && (!ok || strings.TrimSpace(v) == "")) || (ok && domain.IsProtectedBranch(v)) {
		return nil, refuse(r)
	}
	if _, present := fields[rule.field]; present && !ok { // a non-string target cannot be checked
		return nil, refuse(r)
	}
	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return g.Next.RoundTrip(r)
}

func refuse(r *http.Request) error {
	return fmt.Errorf("github %s %s: %w", r.Method, r.URL.Path, domain.ErrProtectedBranch)
}
