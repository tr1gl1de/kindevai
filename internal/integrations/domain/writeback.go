package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// YouTrack write-back (KIN-38): comments, state changes and Knowledge Base sections the platform writes as the
// integration user. Each write is an outbox message; IssueWrite tracks it for the task page ("awaiting publication").

// WriteKind is what a write changes.
type WriteKind string

const (
	WriteComment WriteKind = "comment"
	WriteState   WriteKind = "state"
	WriteArticle WriteKind = "article"
)

// WriteStatus is where a write stands.
type WriteStatus string

const (
	WritePending   WriteStatus = "pending"   // enqueued or being retried
	WritePublished WriteStatus = "published" // YouTrack accepted it
	WriteFailed    WriteStatus = "failed"    // given up; Error says why
)

// IssueWrite is one write-back message of a task. Key is the outbox idempotency key (the correlation id).
type IssueWrite struct {
	Key       string
	TaskID    string
	IssueID   string
	Kind      WriteKind
	Target    string // the article id of an article write
	Status    WriteStatus
	Attempts  int
	Error     string // last error code ("http_503", "conflict", ...)
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Publication sums a task's writes for the task page.
type Publication struct {
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	Published int    `json:"published"`
	LastError string `json:"lastError,omitempty"` // of the newest pending or failed write that has one
}

// WriteMarker is the token a comment carries so that a retry after a lost response finds it instead of posting it
// again: "kindev-" and 16 hex digits of the key's hash.
func WriteMarker(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "kindev-" + hex.EncodeToString(sum[:8])
}

// TaskStates are the workflow's task states (KB KIN-A-14) a project may map to YouTrack states.
var TaskStates = []string{"CREATED", "ANALYZING", "READY", "AWAITING_APPROVAL", "RUNNING", "VERIFYING", "COMPLETED",
	"BLOCKED", "FAILED", "CANCELLED"}

const maxStateValue = 80

// StateMap maps task states to the YouTrack State a transition sets. Empty (the default) means comments only.
type StateMap map[string]string

// NewStateMap validates a mapping; empty values are dropped. Values become a YouTrack command ("State {v}"), so
// braces and control characters are refused.
func NewStateMap(in map[string]string) (StateMap, error) {
	out := StateMap{}
	for k, v := range in {
		if !slices.Contains(TaskStates, k) {
			return nil, invalid("states", "unknown_task_state")
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) > maxStateValue || strings.ContainsAny(v, "{}") || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return nil, invalid("states", "state_value")
		}
		out[k] = v
	}
	return out, nil
}

// Command is the YouTrack command that sets the state mapped to taskState, or "" when it is not mapped.
func (m StateMap) Command(taskState string) string {
	if v := m[taskState]; v != "" {
		return "State {" + v + "}"
	}
	return ""
}

var headingRe = regexp.MustCompile(`^(#{1,6})[ \t]+(.*?)[ \t#]*$`)

// ReplaceSection replaces the body of the first Markdown section whose heading text is heading (up to the next
// heading of the same or a higher level), or appends "## heading" with body. Replacing with the same body changes
// nothing, so a repeated write is idempotent.
func ReplaceSection(content, heading, body string) string {
	body = strings.Trim(body, "\n")
	lines := strings.Split(content, "\n")
	start, level := -1, 0
	for i, l := range lines {
		m := headingRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		if start < 0 {
			if strings.TrimSpace(m[2]) == heading {
				start, level = i, len(m[1])
			}
			continue
		}
		if len(m[1]) <= level {
			out := append(slices.Clone(lines[:start+1]), "", body, "")
			return strings.Join(append(out, lines[i:]...), "\n")
		}
	}
	if start >= 0 {
		out := append(slices.Clone(lines[:start+1]), "", body, "")
		return strings.Join(out, "\n")
	}
	if strings.TrimSpace(content) == "" {
		return "## " + heading + "\n\n" + body + "\n"
	}
	return strings.TrimRight(content, "\n") + "\n\n## " + heading + "\n\n" + body + "\n"
}
