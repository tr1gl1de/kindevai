// Package httpapi exposes the task API of the workflow context (KIN-23, KIN-25): the task list filtered by project
// and state with agent time, details of a task with its plan revision, latest execution, steps and attempts and
// their agent time, an attempt's recorded events in bounded pages and its workspace reports (KIN-70), creating a manual task, starting a mock run,
// cancelling and retrying. Members see their projects' tasks; the Owner and project admins manage them.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Handler serves /api/tasks.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// TaskView is a task in the list and the details.
type TaskView struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"projectId"`
	Title         string    `json:"title"`
	YouTrackIssue string    `json:"youtrackIssue,omitempty"`
	State         string    `json:"state"`
	BlockReason   string    `json:"blockReason,omitempty"` // runner | network | approval | cli_limit (BLOCKED only)
	Reason        string    `json:"reason,omitempty"`
	PlanRevision  int       `json:"planRevision"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// AgentTimeView is a sum of agent time: milliseconds and none | running | complete | incomplete.
type AgentTimeView struct {
	Ms     int64  `json:"ms"`
	Status string `json:"status"`
}

func agentTimeView(t domain.AgentTime) AgentTimeView {
	return AgentTimeView{Ms: t.Total.Milliseconds(), Status: string(t.Status)}
}

// TimeView is a task's agent time and, separately, its calendar duration.
type TimeView struct {
	AgentMs     int64  `json:"agentMs"`
	AgentStatus string `json:"agentStatus"`
	CalendarMs  int64  `json:"calendarMs"`
	Running     bool   `json:"running"` // an execution runs: the calendar duration counts up to now
	Started     bool   `json:"started"` // the task has an execution
}

func timeView(t app.TaskTime) TimeView {
	return TimeView{AgentMs: t.Agent.Total.Milliseconds(), AgentStatus: string(t.Agent.Status),
		CalendarMs: t.Calendar.Milliseconds(), Running: t.Running, Started: t.Started}
}

// AttemptView is one runner launch of a step, with its agent and work period (runner clock).
type AttemptView struct {
	Number        int        `json:"number"`
	RunnerID      string     `json:"runnerId"`
	Fence         int64      `json:"fence"`
	State         string     `json:"state"`
	Error         string     `json:"error,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	Agent         string     `json:"agent,omitempty"`
	WorkStartedAt *time.Time `json:"workStartedAt"`
	WorkEndedAt   *time.Time `json:"workEndedAt"`
	WorkStatus    string     `json:"workStatus"`
	WorkMs        int64      `json:"workMs"`
	// What the AI CLI reported (KIN-79): its native session, model, CLI version and usage; omitted for the mock
	// agent.
	Session    string     `json:"session,omitempty"`
	Model      string     `json:"model,omitempty"`
	CLIVersion string     `json:"cliVersion,omitempty"`
	Usage      *UsageView `json:"usage,omitempty"`
}

// UsageView is AI CLI usage: tokens, cost in USD (0 when the CLI reports none, e.g. Codex) and turns.
type UsageView struct {
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	Turns            int     `json:"turns"`
}

func usageView(u domain.AgentUsage) *UsageView {
	if u == (domain.AgentUsage{}) {
		return nil
	}
	return &UsageView{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens, CostUSD: u.CostUSD, Turns: u.Turns}
}

// StepView is one step of an execution.
type StepView struct {
	ID         string        `json:"id"`
	Key        string        `json:"key"`
	Title      string        `json:"title"`
	Repository string        `json:"repository"`
	DependsOn  []string      `json:"dependsOn"`
	State      string        `json:"state"`
	WaitReason string        `json:"waitReason,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	RunnerID   string        `json:"runnerId,omitempty"`
	Fence      int64         `json:"fence"`
	Attempts   []AttemptView `json:"attempts"`
	AgentTime  AgentTimeView `json:"agentTime"`
}

// ExecutionView is a run of a plan revision.
type ExecutionView struct {
	ID           string        `json:"id"`
	PlanRevision int           `json:"planRevision"`
	PlanHash     string        `json:"planHash"`
	State        string        `json:"state"`
	Reason       string        `json:"reason,omitempty"`
	CreatedAt    time.Time     `json:"createdAt"`
	FinishedAt   *time.Time    `json:"finishedAt"`
	Steps        []StepView    `json:"steps"`
	AgentTime    AgentTimeView `json:"agentTime"`
	Usage        *UsageView    `json:"usage,omitempty"` // the AI CLI usage of all attempts (KIN-79)
}

// RevisionView is the task's current plan revision.
type RevisionView struct {
	Number    int       `json:"number"`
	Hash      string    `json:"hash"`
	Risk      string    `json:"risk"`
	Summary   string    `json:"summary"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// DetailView is GET /api/tasks/{id}.
type DetailView struct {
	Task      TaskView       `json:"task"`
	Revision  *RevisionView  `json:"revision"`
	Execution *ExecutionView `json:"execution"`
	CanManage bool           `json:"canManage"`
	Time      TimeView       `json:"time"`
}

// ListItemView is a task in the list with its time.
type ListItemView struct {
	TaskView
	Time TimeView `json:"time"`
}

// ListView is GET /api/tasks: at most app.MaxTaskList tasks, newest first, and the agent time of every task the
// filter selects.
type ListView struct {
	Tasks []ListItemView `json:"tasks"`
	Total AgentTimeView  `json:"total"`
}

// EventView is one recorded runner event of an attempt. Data is the process's output as text, never markup.
type EventView struct {
	Seq     int64     `json:"seq"`
	Kind    string    `json:"kind"`
	At      time.Time `json:"at"`
	Stream  string    `json:"stream,omitempty"`
	Offset  int64     `json:"offset,omitempty"`
	Data    string    `json:"data,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Omitted bool      `json:"omitted,omitempty"`
}

// EventPageView is a page of an attempt's events: ask again with after=next (right away while more).
type EventPageView struct {
	Events []EventView `json:"events"`
	Next   int64       `json:"next"`
	More   bool        `json:"more"`
}

// InstructionFileView is an instruction file of the attempt's worktree: path (untrusted text), SHA-256, size.
type InstructionFileView struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Symlink bool   `json:"symlink,omitempty"`
}

// ChangedFileView is a path the attempt changed: A, M, D or T (renames show as D + A).
type ChangedFileView struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// InstructionChangeView is an instruction file whose hash changed during the attempt (empty: absent).
type InstructionChangeView struct {
	Path   string `json:"path"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// PreparedView is the latest workspace.prepared of an attempt.
type PreparedView struct {
	At                    time.Time             `json:"at"`
	Repository            string                `json:"repository"`
	Branch                string                `json:"branch"`
	Base                  string                `json:"base"`
	BaseSHA               string                `json:"baseSha"`
	Instructions          []InstructionFileView `json:"instructions"`
	InstructionsTruncated bool                  `json:"instructionsTruncated,omitempty"`
}

// ChangesView is the latest workspace.changes of an attempt.
type ChangesView struct {
	At                  time.Time               `json:"at"`
	Branch              string                  `json:"branch"`
	BaseSHA             string                  `json:"baseSha"`
	HeadSHA             string                  `json:"headSha"`
	Files               []ChangedFileView       `json:"files"`
	FilesTotal          int                     `json:"filesTotal"`
	Truncated           bool                    `json:"truncated,omitempty"`
	InstructionsChanged []InstructionChangeView `json:"instructionsChanged"`
}

// WorkspaceView is GET …/attempts/{fence}/workspace: the attempt's latest workspace reports, null where none.
type WorkspaceView struct {
	Prepared *PreparedView `json:"prepared"`
	Changes  *ChangesView  `json:"changes"`
}

// CreateRequest is POST /api/tasks.
type CreateRequest struct {
	ProjectID string `json:"projectId"`
	Title     string `json:"title"`
}

// MockRunRequest is POST /api/tasks/{id}/mock-run.
type MockRunRequest struct {
	Scenario string `json:"scenario"`
}

// CancelRequest is POST /api/tasks/{id}/cancel.
type CancelRequest struct {
	Reason string `json:"reason"`
}

// Register adds the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks", h.list)
	mux.HandleFunc("POST /api/tasks", h.create)
	mux.HandleFunc("GET /api/tasks/{id}", h.get)
	mux.HandleFunc("POST /api/tasks/{id}/mock-run", h.mockRun)
	mux.HandleFunc("POST /api/tasks/{id}/cancel", h.cancel)
	mux.HandleFunc("POST /api/tasks/{id}/retry", h.retry)
	mux.HandleFunc("GET /api/tasks/{id}/steps/{step}/attempts/{fence}/events", h.events)
	mux.HandleFunc("GET /api/tasks/{id}/steps/{step}/attempts/{fence}/workspace", h.workspace)
}

func (h *Handler) now() time.Time { return h.Service.Now().UTC() }

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
	}
	return p, ok
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, app.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the Owner or a project admin may do this")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "task or project not found")
	case errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrConflict),
		errors.Is(err, domain.ErrStaleRevision):
		httpx.WriteError(w, http.StatusConflict, "invalid_state", "the task's state does not allow this")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("tasks API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func taskView(t domain.Task) TaskView {
	return TaskView{ID: t.ID, ProjectID: t.ProjectID, Title: t.Title, YouTrackIssue: t.YouTrackIssue, State: string(t.State),
		BlockReason: string(t.BlockReason), Reason: t.Reason.Detail, PlanRevision: t.PlanRevision, CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt}
}

func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func executionView(e domain.Execution, now time.Time) *ExecutionView {
	v := &ExecutionView{ID: e.ID, PlanRevision: e.PlanRevision, PlanHash: e.PlanHash, State: string(e.State),
		Reason: e.Reason, CreatedAt: e.CreatedAt, FinishedAt: e.FinishedAt, Steps: make([]StepView, 0, len(e.Steps)),
		AgentTime: agentTimeView(e.AgentTime(now))}
	var total domain.AgentUsage
	for _, s := range e.Steps {
		sv := StepView{ID: s.ID, Key: s.Key, Title: s.Title, Repository: s.Repository, DependsOn: orEmpty(s.DependsOn),
			State: string(s.State), WaitReason: string(s.WaitReason), Reason: s.Reason, RunnerID: s.RunnerID, Fence: s.Fence,
			Attempts: make([]AttemptView, 0, len(s.Attempts)), AgentTime: agentTimeView(s.AgentTime(now))}
		for _, a := range s.Attempts {
			st, d := a.Work(now)
			sv.Attempts = append(sv.Attempts, AttemptView{Number: a.Number, RunnerID: a.RunnerID, Fence: a.Fence,
				State: string(a.State), Error: a.Error, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Agent: a.Agent,
				WorkStartedAt: a.WorkStart, WorkEndedAt: a.WorkEnd, WorkStatus: string(st), WorkMs: d.Milliseconds(),
				Session: a.Report.Session, Model: a.Report.Model, CLIVersion: a.Report.Version, Usage: usageView(a.Report.Usage)})
			total.Add(a.Report.Usage)
		}
		v.Steps = append(v.Steps, sv)
	}
	v.Usage = usageView(total)
	return v
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	q := app.TaskQuery{ProjectID: r.URL.Query().Get("projectId")}
	for _, v := range r.URL.Query()["state"] {
		for st := range strings.SplitSeq(v, ",") {
			if st != "" {
				q.States = append(q.States, domain.TaskState(st))
			}
		}
	}
	l, err := h.Service.ListTasks(r.Context(), p, q)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := ListView{Tasks: make([]ListItemView, 0, len(l.Tasks)), Total: agentTimeView(l.Total)}
	for _, it := range l.Tasks {
		out.Tasks = append(out.Tasks, ListItemView{TaskView: taskView(it.Task), Time: timeView(it.Time)})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request, p auth.Principal, id string, status int) {
	d, err := h.Service.TaskDetail(r.Context(), p, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	v := DetailView{Task: taskView(d.Task), CanManage: d.CanManage, Time: timeView(d.Time)}
	if d.Revision != nil {
		rev := d.Revision
		v.Revision = &RevisionView{Number: rev.Number(), Hash: rev.Hash(), Risk: string(rev.Risk()),
			Summary: rev.Plan().Summary, CreatedBy: rev.CreatedBy(), CreatedAt: rev.CreatedAt()}
	}
	if d.Execution != nil {
		v.Execution = executionView(*d.Execution, h.now())
	}
	httpx.WriteJSON(w, status, v)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	h.detail(w, r, p, r.PathValue("id"), http.StatusOK)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req CreateRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	t, err := h.Service.CreateTaskAs(r.Context(), p, req.ProjectID, req.Title)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.detail(w, r, p, t.ID, http.StatusCreated)
}

func (h *Handler) mockRun(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req MockRunRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if _, err := h.Service.RunMock(r.Context(), p, r.PathValue("id"), req.Scenario); err != nil {
		h.fail(w, err)
		return
	}
	h.detail(w, r, p, r.PathValue("id"), http.StatusAccepted)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req CancelRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if _, err := h.Service.CancelTaskAs(r.Context(), p, r.PathValue("id"), req.Reason); err != nil {
		h.fail(w, err)
		return
	}
	h.detail(w, r, p, r.PathValue("id"), http.StatusOK)
}

func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if _, err := h.Service.RetryTask(r.Context(), p, r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	h.detail(w, r, p, r.PathValue("id"), http.StatusOK)
}

func nonNegative(w http.ResponseWriter, field, s string) (int64, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "number", Field: field})
		return 0, false
	}
	return n, true
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	fence, ok := nonNegative(w, "fence", r.PathValue("fence"))
	if !ok {
		return
	}
	after, ok := nonNegative(w, "after", r.URL.Query().Get("after"))
	if !ok {
		return
	}
	pg, err := h.Service.AttemptEvents(r.Context(), p, r.PathValue("id"), r.PathValue("step"), fence, after)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := EventPageView{Events: make([]EventView, 0, len(pg.Events)), Next: pg.Next, More: pg.More}
	for _, e := range pg.Events {
		out.Events = append(out.Events, EventView{Seq: e.Seq, Kind: e.Kind, At: e.At, Stream: e.Stream, Offset: e.Offset,
			Data: e.Data, Bytes: e.Bytes, Reason: e.Reason, Omitted: e.Omitted})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) workspace(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	fence, ok := nonNegative(w, "fence", r.PathValue("fence"))
	if !ok {
		return
	}
	ws, err := h.Service.AttemptWorkspace(r.Context(), p, r.PathValue("id"), r.PathValue("step"), fence)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, workspaceView(ws))
}

func workspaceView(ws app.AttemptWorkspace) WorkspaceView {
	var out WorkspaceView
	if p := ws.Prepared; p != nil {
		v := &PreparedView{At: p.At, Repository: p.Repository, Branch: p.Branch, Base: p.Base, BaseSHA: p.BaseSHA,
			Instructions: make([]InstructionFileView, 0, len(p.Instructions)), InstructionsTruncated: p.InstructionsTruncated}
		for _, f := range p.Instructions {
			v.Instructions = append(v.Instructions, InstructionFileView(f))
		}
		out.Prepared = v
	}
	if c := ws.Changes; c != nil {
		v := &ChangesView{At: c.At, Branch: c.Branch, BaseSHA: c.BaseSHA, HeadSHA: c.HeadSHA,
			Files: make([]ChangedFileView, 0, len(c.Files)), FilesTotal: c.FilesTotal, Truncated: c.Truncated,
			InstructionsChanged: make([]InstructionChangeView, 0, len(c.InstructionsChanged))}
		for _, f := range c.Files {
			v.Files = append(v.Files, ChangedFileView(f))
		}
		for _, f := range c.InstructionsChanged {
			v.InstructionsChanged = append(v.InstructionsChanged, InstructionChangeView(f))
		}
		out.Changes = v
	}
	return out
}
