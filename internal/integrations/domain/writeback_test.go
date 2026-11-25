package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestReplaceSection(t *testing.T) {
	t.Parallel()
	doc := "# Title\n\nIntro\n\n## A\n\nold a\n\n### A.1\n\nsub\n\n## B\n\nb body\n"
	for _, tc := range []struct{ name, heading, body, want string }{
		{"replaces a section up to the next heading of its level", "A", "new a",
			"# Title\n\nIntro\n\n## A\n\nnew a\n\n## B\n\nb body\n"},
		{"replaces the last section", "B", "new b", "# Title\n\nIntro\n\n## A\n\nold a\n\n### A.1\n\nsub\n\n## B\n\nnew b\n"},
		{"appends a missing section", "C", "c body", doc + "\n## C\n\nc body\n"},
		{"matches a subsection", "A.1", "x", "# Title\n\nIntro\n\n## A\n\nold a\n\n### A.1\n\nx\n\n## B\n\nb body\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ReplaceSection(doc, tc.heading, tc.body); got != tc.want {
				t.Fatalf("got\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
	once := ReplaceSection(doc, "C", "c body")
	if ReplaceSection(once, "C", "c body") != once {
		t.Fatal("replacing a section with the same body changed the article")
	}
	if got := ReplaceSection("", "S", "b"); got != "## S\n\nb\n" {
		t.Fatalf("empty article: %q", got)
	}
}

func TestStateMap(t *testing.T) {
	t.Parallel()
	m, err := NewStateMap(map[string]string{"RUNNING": " In Progress ", "COMPLETED": "Fixed", "FAILED": ""})
	if err != nil || len(m) != 2 || m["RUNNING"] != "In Progress" {
		t.Fatalf("map = %v, %v", m, err)
	}
	if got := m.Command("RUNNING"); got != "State {In Progress}" {
		t.Fatalf("command = %q", got)
	}
	if m.Command("READY") != "" {
		t.Fatal("an unmapped state has a command")
	}
	for _, bad := range []map[string]string{
		{"running": "x"}, {"WEATHER": "x"}, {"RUNNING": "a}b"}, {"RUNNING": "a{b"}, {"RUNNING": "a\nb"},
		{"RUNNING": string(make([]byte, 81))},
	} {
		if _, err := NewStateMap(bad); !errors.As(err, new(*ValidationError)) {
			t.Fatalf("%v accepted: %v", bad, err)
		}
	}
}

func TestIssueWriteMarker(t *testing.T) {
	t.Parallel()
	a, b := WriteMarker("task/t1/v3/plan"), WriteMarker("task/t1/v4/plan")
	if a == b || len(a) != len("kindev-")+16 || a != WriteMarker("task/t1/v3/plan") {
		t.Fatalf("markers %q %q", a, b)
	}
}

// Review findings: headings in code fences do not end a section, "C#" is a heading text, and a body's own headings
// are demoted so a repeated write stays idempotent.
func TestReplaceSectionEdgeCases(t *testing.T) {
	t.Parallel()
	doc := "## A\n\n```\n## not a heading\n```\n\n## C#\n\nold\n\n## D\n\nd\n"
	got := ReplaceSection(doc, "A", "new")
	if got != "## A\n\nnew\n\n## C#\n\nold\n\n## D\n\nd\n" {
		t.Fatalf("fenced: %q", got)
	}
	if got := ReplaceSection(doc, "C#", "x"); !strings.Contains(got, "## C#\n\nx\n\n## D") || strings.Count(got, "C#") != 1 {
		t.Fatalf("C#: %q", got)
	}
	body := "intro\n\n## Sub\n\ntext"
	once := ReplaceSection(doc, "D", body)
	if !strings.Contains(once, "### Sub") || ReplaceSection(once, "D", body) != once {
		t.Fatalf("not idempotent: %q", once)
	}
	appended := ReplaceSection("# T\n", "N", body)
	if ReplaceSection(appended, "N", body) != appended {
		t.Fatalf("append not idempotent: %q", appended)
	}
}
