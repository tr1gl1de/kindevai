package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxPromptBytes bounds the prompt/context package of a step (the runner's limit, protocol.MaxPromptBytes).
const MaxPromptBytes = 32 << 10

// StepPrompt is the prompt/context package an AI CLI gets for a plan step (KIN-79): the task, the approved plan's
// summary, the step and the scope it may change. The plan summary is cut so the package stays within
// MaxPromptBytes. (Richer context — YouTrack text, KB articles, artifacts — comes with the planning flow, KIN-43.)
func StepPrompt(t Task, p Plan, s PlanStep) string {
	var head, tail strings.Builder
	fmt.Fprintf(&head, "# Task: %s\n", t.Title)
	if t.YouTrackIssue != "" {
		fmt.Fprintf(&head, "YouTrack issue: %s\n", t.YouTrackIssue)
	}
	fmt.Fprintf(&tail, "\n## Your step: %s\n", s.Title)
	fmt.Fprintf(&tail, "Step key: %s\nRepository: %s (the working directory is its checkout)\n", s.Key, s.Repository)
	if len(s.Scope) > 0 {
		tail.WriteString("Change only these paths (the approved scope; changes elsewhere stop the step):\n")
		for _, sc := range s.Scope {
			fmt.Fprintf(&tail, "- %s\n", sc)
		}
	}
	tail.WriteString("\nWork in the repository, keep the change minimal and leave it committed or staged in the worktree.\n")
	summary := ""
	if p.Summary != "" {
		summary = "\n## Approved plan\n" + p.Summary + "\n"
	}
	if room := MaxPromptBytes - head.Len() - tail.Len(); len(summary) > room {
		summary = cutUTF8(summary, max(room-len("…\n"), 0)) + "…\n"
	}
	return cutUTF8(head.String()+summary+tail.String(), MaxPromptBytes)
}

func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
