package cliagent

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Claude is the Claude Code dialect (verified against Claude Code 2.1.331 `--help` and recorded output):
//
//	claude -p --output-format stream-json --verbose <permissions> [--model M] [--resume ID]   (prompt on stdin)
//
// Safe mode is `--permission-mode acceptEdits --setting-sources user`: edits in the working directory are allowed,
// anything that would prompt (shell commands, network tools) is denied in print mode, and the repository's own
// .claude/settings*.json are not loaded, so untrusted repository settings can neither allow tools nor run hooks on
// the host (CLAUDE.md, skills and agents still apply); `--settings {"disableAllHooks":true}` also stops hooks that
// a repository skill or agent could declare in its frontmatter. The bypass is `--dangerously-skip-permissions`, only inside
// the strict-egress sandbox, where the repository's settings load as usual. Read-only (a reviewer, KIN-35) is
// `--permission-mode default --disallowedTools Edit,MultiEdit,Write,NotebookEdit,Bash` with the safe mode's settings.
type Claude struct{}

// safeSettings is the settings JSON of safe mode: no hooks run on the host.
const safeSettings = `{"disableAllHooks":true}`

// readOnlyDenied are the tools a read-only run (a reviewer, KIN-35) may not use even if a setting allowed them.
const readOnlyDenied = "Edit,MultiEdit,Write,NotebookEdit,Bash"

// Name is the agent name.
func (Claude) Name() string { return domain.AgentClaude }

// Args builds the command line. Every value is validated by domain.AgentRequest.Check.
func (Claude) Args(r domain.AgentRequest, p domain.Permissions) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	switch p {
	case domain.PermissionsBypass:
		args = append(args, "--dangerously-skip-permissions")
	case domain.PermissionsReadOnly:
		// Default mode in print mode denies whatever would prompt; the write and shell tools are also disallowed.
		args = append(args, "--permission-mode", "default", "--disallowedTools", readOnlyDenied, "--setting-sources", "user",
			"--settings", safeSettings)
	default:
		args = append(args, "--permission-mode", "acceptEdits", "--setting-sources", "user", "--settings", safeSettings)
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Resume != "" {
		args = append(args, "--resume", r.Resume)
	}
	return args
}

// Env is the dialect's own environment: no self-update while a run uses the binary.
func (Claude) Env() []string { return []string{"DISABLE_AUTOUPDATER=1"} }

// Parser returns a parser for one run's stream.
func (Claude) Parser() Parser { return &claudeParser{} }

type claudeParser struct{}

type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Version   string `json:"claude_code_version"`
	// assistant
	Message *struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Error json.RawMessage `json:"error"` // assistant: "rate_limit", "authentication_failed", …; api_retry: a string too
	// rate_limit_event
	RateLimit *struct {
		Status   string `json:"status"`
		ResetsAt int64  `json:"resetsAt"`
		Type     string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
	// result
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	Usage      *struct {
		Input      int64 `json:"input_tokens"`
		Output     int64 `json:"output_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// claudeErrors maps the assistant message's error code to a typed failure.
var claudeErrors = map[string]string{
	"rate_limit":            domain.CauseRateLimit,
	"authentication_failed": domain.CauseAuth,
	"billing_error":         domain.CauseBilling,
}

func (*claudeParser) Parse(line []byte) []domain.AgentEvent {
	var l claudeLine
	if json.Unmarshal(line, &l) != nil {
		return nil // not an event (the raw output is streamed anyway)
	}
	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			return []domain.AgentEvent{{Kind: domain.EventSession, Session: l.SessionID, Model: l.Model, Version: l.Version}}
		case "api_retry":
			return []domain.AgentEvent{{Kind: domain.EventRetrying, Session: l.SessionID,
				Text: domain.Truncate(strings.Trim(string(l.Error), `"`), domain.MaxErrorMessage)}}
		}
	case "assistant":
		return claudeAssistant(l)
	case "rate_limit_event":
		if l.RateLimit != nil && l.RateLimit.Status == "rejected" {
			f := &domain.AgentFailure{Code: domain.FailureCLILimit, Cause: domain.CauseRateLimit,
				Message: "rate limit reached: " + l.RateLimit.Type}
			if l.RateLimit.ResetsAt > 0 {
				f.ResetsAt = time.Unix(l.RateLimit.ResetsAt, 0).UTC()
			}
			return []domain.AgentEvent{{Kind: domain.EventFailure, Session: l.SessionID, Failure: f}}
		}
	case "result":
		return claudeResult(l)
	}
	return nil
}

func claudeAssistant(l claudeLine) []domain.AgentEvent {
	if l.Message == nil {
		return nil
	}
	var code string
	_ = json.Unmarshal(l.Error, &code)
	if code != "" { // a synthetic message carrying an API error: its text is the error
		var text []string
		for _, c := range l.Message.Content {
			if c.Type == "text" {
				text = append(text, c.Text)
			}
		}
		msg := domain.Truncate(strings.Join(text, " "), domain.MaxErrorMessage)
		f := &domain.AgentFailure{Code: domain.FailureCLI, Message: msg}
		if cause, ok := claudeErrors[code]; ok {
			f.Code, f.Cause = domain.FailureCLILimit, cause
		} else if f.Code, f.Cause = domain.ClassifyCLIError(msg); f.Code == domain.FailureCLI {
			f.Message = domain.Truncate(code+": "+msg, domain.MaxErrorMessage)
		}
		return []domain.AgentEvent{{Kind: domain.EventFailure, Session: l.SessionID, Failure: f}}
	}
	var evs []domain.AgentEvent
	for _, c := range l.Message.Content {
		switch c.Type {
		case "text":
			evs = append(evs, domain.AgentEvent{Kind: domain.EventText, Session: l.SessionID,
				Text: domain.Truncate(c.Text, domain.MaxEventText)})
		case "tool_use":
			evs = append(evs, domain.AgentEvent{Kind: domain.EventToolUse, Session: l.SessionID,
				Tool: domain.Truncate(c.Name, 128), Input: domain.Truncate(string(c.Input), domain.MaxToolInput)})
		}
	}
	return evs
}

func claudeResult(l claudeLine) []domain.AgentEvent {
	u := domain.Usage{CostUSD: l.CostUSD, Turns: l.NumTurns, DurationMS: l.DurationMS}
	if l.Usage != nil {
		u.InputTokens, u.OutputTokens = l.Usage.Input, l.Usage.Output
		u.CacheReadTokens, u.CacheWriteTokens = l.Usage.CacheRead, l.Usage.CacheWrite
	}
	res := domain.AgentEvent{Kind: domain.EventResult, Session: l.SessionID, Text: domain.Truncate(l.Result, domain.MaxEventText),
		Usage: &u}
	if !l.IsError && !strings.HasPrefix(l.Subtype, "error") {
		return []domain.AgentEvent{res}
	}
	msg := l.Result
	if msg == "" {
		msg = l.Subtype
	}
	msg = domain.Truncate(msg, domain.MaxErrorMessage)
	code, cause := domain.ClassifyCLIError(msg)
	return []domain.AgentEvent{
		{Kind: domain.EventFailure, Session: l.SessionID, Failure: &domain.AgentFailure{Code: code, Cause: cause, Message: msg}},
		res,
	}
}
