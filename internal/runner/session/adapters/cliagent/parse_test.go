package cliagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// parseFile replays a recorded stream through the dialect's parser.
func parseFile(t *testing.T, d Dialect, name string) []domain.AgentEvent {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	p := d.Parser()
	var evs []domain.AgentEvent
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		evs = append(evs, p.Parse(sc.Bytes())...)
	}
	return evs
}

// TestGolden parses the recorded CLI streams (claude/read and claude/auth were recorded with Claude Code 2.1.331;
// the others follow the documented formats) into the common event model and compares with the golden files.
func TestGolden(t *testing.T) {
	t.Parallel()
	for _, d := range []Dialect{Claude{}, Codex{}} {
		files, err := filepath.Glob(filepath.Join("testdata", d.Name(), "*.jsonl"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no recordings for %s: %v", d.Name(), err)
		}
		for _, f := range files {
			t.Run(f, func(t *testing.T) {
				t.Parallel()
				got, err := json.MarshalIndent(parseFile(t, d, f), "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				golden := strings.TrimSuffix(f, ".jsonl") + ".golden.json"
				if *update {
					if err := os.WriteFile(golden, append(got, '\n'), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("%v (run with -update to create it)", err)
				}
				if string(want) != string(got)+"\n" {
					t.Fatalf("%s differs from %s:\n%s", f, golden, got)
				}
			})
		}
	}
}

// TestRecordedOutcomes: each recording folds into the expected result; rate limits and logouts are cli_limit.
func TestRecordedOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d       Dialect
		file    string
		ok      bool
		code    domain.FailureCode
		cause   string
		session string
	}{
		{Claude{}, "claude/read.jsonl", true, "", "", "8bc06756-ac80-4061-889c-c0ee7d40571c"},
		{Claude{}, "claude/auth.jsonl", false, domain.FailureCLILimit, domain.CauseAuth, "8b3a828a-721f-406b-8805-c5cd68783388"},
		{Claude{}, "claude/rate-limit.jsonl", false, domain.FailureCLILimit, domain.CauseRateLimit, "0d2f3c1e-7a51-4b0e-9a64-3c1d2e5f6a7b"},
		{Claude{}, "claude/max-turns.jsonl", false, domain.FailureCLI, "", "6e7f0a1b-2c3d-4e5f-8a9b-0c1d2e3f4a5b"},
		{Codex{}, "codex/success.jsonl", true, "", "", "0199a213-81c0-7800-8aa1-bbab2a035a53"},
		// KIN-34: planner runs whose final answer is the plan (hand-written to the documented formats).
		{Claude{}, "claude/plan.jsonl", true, "", "", "5f0c7a2e-1b3d-4c5e-8f9a-0b1c2d3e4f50"},
		{Codex{}, "codex/plan.jsonl", true, "", "", "0199a213-81c0-7800-8aa1-bbab2a035a60"},
		{Codex{}, "codex/rate-limit.jsonl", false, domain.FailureCLILimit, domain.CauseRateLimit, "0199a213-81c0-7800-8aa1-bbab2a035a54"},
		{Codex{}, "codex/auth.jsonl", false, domain.FailureCLILimit, domain.CauseAuth, "0199a213-81c0-7800-8aa1-bbab2a035a55"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			t.Parallel()
			var tr domain.Transcript
			for _, e := range parseFile(t, c.d, filepath.Join("testdata", c.file)) {
				tr.Apply(e)
			}
			res := tr.Result(domain.End{})
			ok, reason := res.Outcome()
			if ok != c.ok || res.Session != c.session {
				t.Fatalf("outcome = %v %q session %q", ok, reason, res.Session)
			}
			if !ok && (res.Failure.Code != c.code || res.Failure.Cause != c.cause) {
				t.Fatalf("failure = %+v, want %s/%s", res.Failure, c.code, c.cause)
			}
		})
	}
}

func TestClaudeUsageAndResetTime(t *testing.T) {
	t.Parallel()
	var tr domain.Transcript
	for _, e := range parseFile(t, Claude{}, "testdata/claude/read.jsonl") {
		tr.Apply(e)
	}
	res := tr.Result(domain.End{})
	want := domain.Usage{InputTokens: 4, OutputTokens: 131, CacheReadTokens: 32918, CacheWriteTokens: 14183,
		CostUSD: 0.0032316800000000002, Turns: 2, DurationMS: 3432}
	if res.Usage != want || res.Text != "kindevai fixture" || res.Version != "2.1.331" || res.Model != "claude-haiku-5-5" {
		t.Fatalf("result = %+v", res)
	}
	evs := parseFile(t, Claude{}, "testdata/claude/rate-limit.jsonl")
	i := slices.IndexFunc(evs, func(e domain.AgentEvent) bool { return e.Kind == domain.EventFailure })
	if i < 0 || !evs[i].Failure.ResetsAt.Equal(time.Unix(1795575600, 0)) {
		t.Fatalf("rate limit failure without its reset time: %+v", evs)
	}
}

func TestCodexUsage(t *testing.T) {
	t.Parallel()
	var tr domain.Transcript
	for _, e := range parseFile(t, Codex{}, "testdata/codex/success.jsonl") {
		tr.Apply(e)
	}
	res := tr.Result(domain.End{})
	want := domain.Usage{InputTokens: 24763, CacheReadTokens: 24448, OutputTokens: 122, Turns: 1}
	if res.Usage != want || res.Text != "Updated README.md." {
		t.Fatalf("result = %+v", res)
	}
}

func TestParserBoundsFields(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", domain.MaxEventText+10)
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + long + `"},` +
		`{"type":"tool_use","name":"Write","input":{"content":"` + long + `"}}]}}`
	evs := Claude{}.Parser().Parse([]byte(line))
	if len(evs) != 2 || len(evs[0].Text) > domain.MaxEventText+3 || len(evs[1].Input) > domain.MaxToolInput+3 {
		t.Fatalf("unbounded event fields: %d events", len(evs))
	}
}
