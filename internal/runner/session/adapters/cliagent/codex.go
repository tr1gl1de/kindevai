package cliagent

import (
	"encoding/json"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Codex is the Codex CLI dialect:
//
//	codex exec --json --skip-git-repo-check <permissions> [--model M] [resume ID] -   (prompt on stdin: "-")
//
// Codex is not installed on the development runner, so the flags and the JSONL event format (thread.started,
// item.started/completed with agent_message, command_execution, file_change, mcp_tool_call, web_search,
// turn.completed with usage, turn.failed, error) follow the Codex CLI documentation of `codex exec`; the recorded
// streams in testdata are hand-written to it. Safe mode is `--sandbox workspace-write` (Codex's own sandbox: writes
// only in the workspace, no network; exec never asks for approval). The bypass is
// `--dangerously-bypass-approvals-and-sandbox`, only inside the strict-egress sandbox. `--full-auto` is not used. Read-only (a reviewer, KIN-35) is `--sandbox read-only`.
type Codex struct{}

// Name is the agent name.
func (Codex) Name() string { return domain.AgentCodex }

// Args builds the command line. Every value is validated by domain.AgentRequest.Check.
func (Codex) Args(r domain.AgentRequest, p domain.Permissions) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check"}
	switch p {
	case domain.PermissionsBypass:
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	case domain.PermissionsReadOnly:
		args = append(args, "--sandbox", "read-only")
	default:
		args = append(args, "--sandbox", "workspace-write")
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Resume != "" {
		args = append(args, "resume", r.Resume)
	}
	return append(args, "-")
}

// Env is the dialect's own environment (none: CODEX_HOME, when not ~/.codex, comes from the runner's config).
func (Codex) Env() []string { return nil }

// Parser returns a parser for one run's stream.
func (Codex) Parser() Parser { return &codexParser{} }

type codexParser struct {
	session string
	last    string // the last agent message: the run's result text
	usage   domain.Usage
}

type codexLine struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Command string          `json:"command"`
		Changes json.RawMessage `json:"changes"`
		Server  string          `json:"server"`
		Tool    string          `json:"tool"`
		Query   string          `json:"query"`
	} `json:"item"`
	Usage *struct {
		Input  int64 `json:"input_tokens"`
		Cached int64 `json:"cached_input_tokens"`
		Output int64 `json:"output_tokens"`
	} `json:"usage"`
}

func (p *codexParser) Parse(line []byte) []domain.AgentEvent {
	var l codexLine
	if json.Unmarshal(line, &l) != nil {
		return nil
	}
	ev := func(e domain.AgentEvent) []domain.AgentEvent {
		e.Session = p.session
		return []domain.AgentEvent{e}
	}
	switch l.Type {
	case "thread.started":
		p.session = l.ThreadID
		return ev(domain.AgentEvent{Kind: domain.EventSession})
	case "item.started":
		if l.Item != nil && l.Item.Type == "command_execution" {
			return ev(tool("command", l.Item.Command))
		}
	case "item.completed":
		return p.item(l)
	case "turn.completed":
		u := domain.Usage{Turns: 1}
		if l.Usage != nil {
			u.InputTokens, u.CacheReadTokens, u.OutputTokens = l.Usage.Input, l.Usage.Cached, l.Usage.Output
		}
		p.usage.Add(u)
		total := p.usage
		return ev(domain.AgentEvent{Kind: domain.EventResult, Text: domain.Truncate(p.last, domain.MaxEventText), Usage: &total})
	case "turn.failed":
		msg := l.Message
		if l.Error != nil {
			msg = l.Error.Message
		}
		return ev(failure(msg))
	case "error":
		// Codex also reports transient trouble (reconnecting) as error; only a limit or logout fails the run here,
		// anything else fails it with turn.failed.
		if code, _ := domain.ClassifyCLIError(l.Message); code == domain.FailureCLILimit {
			return ev(failure(l.Message))
		}
		return ev(domain.AgentEvent{Kind: domain.EventRetrying, Text: domain.Truncate(l.Message, domain.MaxErrorMessage)})
	}
	return nil
}

func (p *codexParser) item(l codexLine) []domain.AgentEvent {
	if l.Item == nil {
		return nil
	}
	var e domain.AgentEvent
	switch l.Item.Type {
	case "agent_message":
		p.last = l.Item.Text
		e = domain.AgentEvent{Kind: domain.EventText, Text: domain.Truncate(l.Item.Text, domain.MaxEventText)}
	case "file_change":
		e = tool("file_change", string(l.Item.Changes))
	case "mcp_tool_call":
		e = tool("mcp:"+l.Item.Server+"/"+l.Item.Tool, "")
	case "web_search":
		e = tool("web_search", l.Item.Query)
	default: // reasoning, command_execution (reported when it started), todo_list, …
		return nil
	}
	e.Session = p.session
	return []domain.AgentEvent{e}
}

func tool(name, input string) domain.AgentEvent {
	return domain.AgentEvent{Kind: domain.EventToolUse, Tool: domain.Truncate(name, 128), Input: domain.Truncate(input, domain.MaxToolInput)}
}

func failure(msg string) domain.AgentEvent {
	msg = domain.Truncate(strings.TrimSpace(msg), domain.MaxErrorMessage)
	code, cause := domain.ClassifyCLIError(msg)
	return domain.AgentEvent{Kind: domain.EventFailure, Failure: &domain.AgentFailure{Code: code, Cause: cause, Message: msg}}
}
