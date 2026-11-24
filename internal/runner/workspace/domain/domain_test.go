package domain

import (
	"errors"
	"slices"
	"testing"
)

func TestWorkBranchIsAFeatureBranch(t *testing.T) {
	t.Parallel()
	b, err := WorkBranch("KIN-29", "step_1")
	if err != nil || b != "feature/KIN-29--step_1" {
		t.Fatalf("%q %v", b, err)
	}
	for _, tc := range [][2]string{{"", "s"}, {"t", ""}, {"a/b", "s"}, {"t", "x..y"}, {"t", "s.lock"}, {"-t", "s"},
		// "--" separates task and step: ids that could make two attempts share a branch are refused.
		{"A-", "B"}, {"A", "-B"}, {"A--B", "C"}, {"A", "B--C"}} {
		if _, err := WorkBranch(tc[0], tc[1]); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("%v: %v", tc, err)
		}
	}
}

func TestCheckWorkBranchRejectsProtectedBranches(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"main", "master", "Main", "refs/heads/main", "feature/../main", "dev", "feature/",
		"feature/a b", "feature/x.lock", "feature//x", "feature/x/", "-feature/x", "feature/x@{1}", "feature/main/../../master"} {
		if err := CheckWorkBranch(b); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
	for _, b := range []string{"main", "master"} {
		if err := CheckWorkBranch(b); !errors.Is(err, ErrProtectedBranch) {
			t.Errorf("%q: %v", b, err)
		}
	}
	if err := CheckWorkBranch("feature/KIN-29-workspace"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBase(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"dev", "release/1.2", "main"} {
		if err := CheckBase(b); err != nil {
			t.Errorf("%q: %v", b, err)
		}
	}
	for _, b := range []string{"", "-dev", "a..b", "a b", "a:b", "refs/heads/../x", "x.lock", "/x", "x/", "a~1", "a^", "@", "a\\b"} {
		if err := CheckBase(b); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("%q accepted", b)
		}
	}
}

func TestSplitRepository(t *testing.T) {
	t.Parallel()
	o, n, err := SplitRepository("acme/api.v2")
	if err != nil || o != "acme" || n != "api.v2" {
		t.Fatalf("%q %q %v", o, n, err)
	}
	for _, r := range []string{"", "acme", "acme/..", "../x", "a/b/c", "acme/.git", ".hidden/x", "a/b c"} {
		if _, _, err := SplitRepository(r); err == nil {
			t.Errorf("%q accepted", r)
		}
	}
}

func TestInstructionPaths(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"AGENTS.md", "CLAUDE.md", "pkg/sub/AGENTS.md", "web/CLAUDE.md", "CLAUDE.local.md",
		"AGENTS.override.md", ".claude/settings.json", ".claude/settings.local.json", ".claude/agents/reviewer.md",
		".claude/skills/task/SKILL.md", ".claude/rules/go.md", ".claude/commands/x.md", ".codex/config.toml",
		".codex/prompts/a.md", ".mcp.json", "sub/.claude/agents/a.md"} {
		if !IsInstruction(p) {
			t.Errorf("%q not discovered", p)
		}
	}
	for _, p := range []string{"README.md", "docs/claude.md", ".claude/other.txt", "x/.mcp.json", "agents.md.bak"} {
		if IsInstruction(p) {
			t.Errorf("%q discovered", p)
		}
	}
	for _, d := range []string{".git", "node_modules", "a/node_modules", "vendor", "a/.git"} {
		if Descend(d) {
			t.Errorf("descends into %q", d)
		}
	}
	if !Descend(".claude") || !Descend("internal/x") {
		t.Fatal("must descend into ordinary directories")
	}
}

func TestInstructionChanges(t *testing.T) {
	t.Parallel()
	before := []Instruction{{Path: "AGENTS.md", SHA256: "a"}, {Path: "CLAUDE.md", SHA256: "c"}, {Path: ".mcp.json", SHA256: "m"}}
	after := []Instruction{{Path: "AGENTS.md", SHA256: "a"}, {Path: "CLAUDE.md", SHA256: "c2"}, {Path: ".claude/rules/x.md", SHA256: "x"}}
	got := InstructionChanges(before, after)
	want := []InstructionChange{{Path: ".claude/rules/x.md", After: "x"}, {Path: ".mcp.json", Before: "m"},
		{Path: "CLAUDE.md", Before: "c", After: "c2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
}

func TestParseNameStatus(t *testing.T) {
	t.Parallel()
	got, err := ParseNameStatus([]byte("M\x00a.go\x00A\x00dir/new file.txt\x00D\x00old\x00T\x00link\x00"))
	want := []Change{{"a.go", "M"}, {"dir/new file.txt", "A"}, {"old", "D"}, {"link", "T"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ParseNameStatus([]byte("M\x00")); err == nil {
		t.Fatal("truncated output accepted")
	}
}

func TestFit(t *testing.T) {
	t.Parallel()
	got, cut := Fit([]string{"aa", "bb", "cc"}, func(s string) int { return len(s) }, 5)
	if !slices.Equal(got, []string{"aa", "bb"}) || !cut {
		t.Fatalf("%v %v", got, cut)
	}
	if got, cut := Fit([]string{"a"}, func(string) int { return 1 }, 1); len(got) != 1 || cut {
		t.Fatalf("%v %v", got, cut)
	}
}
