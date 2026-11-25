package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// YouTrack write-back (KIN-38, KB KIN-A-19): what the platform tells the task's issue — the plan with the approval
// link, and the outcome with the pull requests, the agent time and the documentation decision. The texts are built
// here; the integrations context delivers them through the outbox.

// Bounds of the implementer's article changes (untrusted model output).
const (
	maxArticleChanges = 8
	maxArticleSection = 200
	maxArticleContent = 6 << 10 // runes: at most 24 KiB, under the write-back's limit
)

// ArticleChange replaces (or adds) one section of a Knowledge Base article: the heading's text and the section's
// new Markdown body.
type ArticleChange struct {
	Article string `json:"article"`
	Section string `json:"section"`
	Content string `json:"content"`
}

func boundArticles(cs []ArticleChange) []ArticleChange {
	var out []ArticleChange
	for _, c := range cs {
		if len(out) == maxArticleChanges {
			break
		}
		c.Article = boundText(c.Article, maxHandoffItem)
		c.Section = strings.Join(strings.Fields(boundText(c.Section, maxArticleSection)), " ")
		c.Content = boundText(c.Content, maxArticleContent)
		if c.Article != "" && c.Section != "" && c.Content != "" {
			out = append(out, c)
		}
	}
	return out
}

// DocsStatus is the documentation decision of a completed run.
type DocsStatus string

const (
	DocsUpdated       DocsStatus = "updated"        // article sections are written
	DocsNotApplicable DocsStatus = "not_applicable" // DOCS_NOT_APPLICABLE, with a reason
	DocsUnstated      DocsStatus = "unstated"       // the implementer gave neither changes nor a reason
)

// DocsNotApplicableCode is how the decision is shown (KB KIN-A-19).
const DocsNotApplicableCode = "DOCS_NOT_APPLICABLE"

// ErrDocsReasonRequired: DOCS_NOT_APPLICABLE without a reason.
var ErrDocsReasonRequired = errors.New("DOCS_NOT_APPLICABLE requires a reason")

// DocsDecision is stored per task run. Rejected lists the articles the implementer wanted to change outside the
// approved scope (not written).
type DocsDecision struct {
	TaskID      string
	ExecutionID string
	Status      DocsStatus
	Reason      string
	Articles    []ArticleChange
	Rejected    []string
}

// NewDocsNotApplicable is DOCS_NOT_APPLICABLE with its (required) reason.
func NewDocsNotApplicable(taskID, executionID, reason string) (DocsDecision, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DocsDecision{}, ErrDocsReasonRequired
	}
	return DocsDecision{TaskID: taskID, ExecutionID: executionID, Status: DocsNotApplicable, Reason: reason}, nil
}

var articleIDRe = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{0,19}-A-[1-9][0-9]{0,9}\b`)

// ValidArticleID reports a readable Knowledge Base article id ("KIN-A-19").
func ValidArticleID(s string) bool {
	m := articleIDRe.FindString(s)
	return m != "" && m == s
}

// DecideDocs takes the implementer's handoff: article changes count only for articles the approved plan names in
// its documentation impact (normative rules must not change outside the approved scope); without any, the decision
// is DOCS_NOT_APPLICABLE with the handoff's reason, or unstated.
func DecideDocs(taskID, executionID string, h Handoff, scope []string) DocsDecision {
	allowed := map[string]bool{}
	for _, d := range scope {
		for _, id := range articleIDRe.FindAllString(d, -1) {
			allowed[id] = true
		}
	}
	d := DocsDecision{TaskID: taskID, ExecutionID: executionID, Status: DocsUnstated}
	at := map[[2]string]int{} // (article, section) -> index: a later change of the same section wins
	for _, c := range h.Articles {
		switch k, seen := [2]string{c.Article, c.Section}, false; {
		case !ValidArticleID(c.Article) || !allowed[c.Article]:
			if !slices.Contains(d.Rejected, c.Article) && len(d.Rejected) < maxArticleChanges {
				d.Rejected = append(d.Rejected, c.Article)
			}
		default:
			var i int
			if i, seen = at[k]; seen {
				d.Articles[i] = c
			} else if len(d.Articles) < maxArticleChanges {
				at[k] = len(d.Articles)
				d.Articles = append(d.Articles, c)
			}
		}
	}
	if len(d.Articles) > 0 {
		d.Status = DocsUpdated
		return d
	}
	if na, err := NewDocsNotApplicable(taskID, executionID, h.DocsNotApplicable); err == nil {
		na.Rejected = d.Rejected
		return na
	}
	return d
}

// PlanNote is what the plan comment says.
type PlanNote struct {
	Revision      int
	Summary       string
	Risk          Risk
	Steps         int
	NeedsApproval bool
	Link          string // the task page; "" when the public URL is not configured
}

const maxCommentSummary = 600

// PlanComment is the comment posted when a plan is accepted: a short summary and, when it waits for approval, where
// to approve it. The plan's hash stays on the platform (KB KIN-A-19).
func PlanComment(n PlanNote) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**kindevai:** plan revision %d (risk %s, %d steps)", n.Revision, n.Risk, n.Steps)
	if n.NeedsApproval {
		b.WriteString(" is waiting for approval")
	} else {
		b.WriteString(" is accepted")
	}
	b.WriteString(".\n\n> " + strings.ReplaceAll(noMentions(boundText(n.Summary, maxCommentSummary)), "\n", "\n> "))
	if n.Link != "" {
		if n.NeedsApproval {
			fmt.Fprintf(&b, "\n\nReview and approve: %s", n.Link)
		} else {
			fmt.Fprintf(&b, "\n\nTask: %s", n.Link)
		}
	}
	return b.String()
}

// FinalNote is what the final comment says.
type FinalNote struct {
	State        TaskState
	Reason       string
	Link         string
	PullRequests []string
	AgentTime    AgentTime
	Docs         *DocsDecision // nil: no documentation step ran (the task did not complete)
}

// FinalComment is the comment posted when the task ends.
func FinalComment(n FinalNote) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**kindevai:** task %s", n.State)
	if r := noMentions(boundText(n.Reason, maxHandoffItem)); r != "" {
		b.WriteString(": " + r)
	}
	b.WriteString(".\n")
	if len(n.PullRequests) > 0 {
		b.WriteString("\nPull requests:\n")
		for _, u := range n.PullRequests {
			b.WriteString("- " + u + "\n")
		}
	}
	fmt.Fprintf(&b, "\nAgent time: %s", formatAgentTime(n.AgentTime))
	if d := n.Docs; d != nil {
		b.WriteString("\nDocumentation: ")
		switch d.Status {
		case DocsUpdated:
			ids := make([]string, 0, len(d.Articles))
			for _, a := range d.Articles {
				ids = append(ids, a.Article+" ("+a.Section+")")
			}
			b.WriteString("updated " + strings.Join(ids, ", "))
		case DocsNotApplicable:
			b.WriteString(DocsNotApplicableCode + " — " + d.Reason)
		default:
			b.WriteString("not stated by the implementer")
		}
		if len(d.Rejected) > 0 {
			b.WriteString("; not written (outside the approved scope): " + strings.Join(d.Rejected, ", "))
		}
	}
	if n.Link != "" {
		fmt.Fprintf(&b, "\n\nTask: %s", n.Link)
	}
	return b.String()
}

// formatAgentTime is "1h 30m" (CLI process time only, KIN-25), marked when a period is missing or still running.
func formatAgentTime(t AgentTime) string {
	d := t.Total.Round(time.Minute)
	s := fmt.Sprintf("%dm", int(d.Minutes()))
	if h := int(d.Hours()); h > 0 {
		s = fmt.Sprintf("%dh %dm", h, int(d.Minutes())%60)
	}
	switch t.Status {
	case WorkIncomplete:
		s += " (incomplete: a period has no usable time)"
	case WorkRunning:
		s += " (still running)"
	case WorkNone, "":
		s = "none"
	}
	return s
}

// noMentions keeps untrusted text (the planner's summary, a CLI's error) from mentioning YouTrack users: a zero-width
// space after "@" leaves it readable but inert.
func noMentions(s string) string { return strings.ReplaceAll(s, "@", "@\u200b") }
