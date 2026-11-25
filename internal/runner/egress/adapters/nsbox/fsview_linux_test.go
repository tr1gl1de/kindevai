//go:build linux

package nsbox

import (
	"os"
	"path/filepath"
	"testing"

	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// A session store bind (KIN-86) goes below HOME only, never over the CLI's login copies, and only from a plain
// directory inside the runner's data directory.
func TestCheckHomeBind(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	store := filepath.Join(data, "cli-sessions", "p", "t", "claude")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(data, "link")
	if err := os.Symlink(store, link); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, tc := range []struct {
		name string
		bind egress.HomeBind
		ok   bool
	}{
		{"claude transcripts", egress.HomeBind{Source: store, Rel: ".claude/projects"}, true},
		{"codex rollouts", egress.HomeBind{Source: store, Rel: ".codex/sessions"}, true},
		{"absolute target", egress.HomeBind{Source: store, Rel: "/etc"}, false},
		{"escapes HOME", egress.HomeBind{Source: store, Rel: "../x"}, false},
		{"HOME itself", egress.HomeBind{Source: store, Rel: "."}, false},
		{"unclean", egress.HomeBind{Source: store, Rel: ".claude//projects"}, false},
		{"over the login directory", egress.HomeBind{Source: store, Rel: ".claude"}, false},
		{"over a login file", egress.HomeBind{Source: store, Rel: ".codex/auth.json"}, false},
		{"store outside the data directory", egress.HomeBind{Source: outside, Rel: ".claude/projects"}, false},
		{"store through a symlink", egress.HomeBind{Source: link, Rel: ".claude/projects"}, false},
		{"missing store", egress.HomeBind{Source: filepath.Join(data, "none"), Rel: ".claude/projects"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := checkHomeBind(tc.bind, []string{data}); (err == nil) != tc.ok {
				t.Fatalf("checkHomeBind(%+v) = %v, want ok %v", tc.bind, err, tc.ok)
			}
		})
	}
}
