// Package httpapi exposes the minimal task API of the workflow context (KIN-23; the full UI is KIN-25):
// list and details of tasks with their plan revision, latest execution, steps and attempts, creating a manual
// task, starting a mock run and cancelling. Members see their projects' tasks; the Owner and project admins
// manage them.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
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

// AttemptView is one runner launch of a step.
type AttemptView struct {
	Number     int        `json:"number"`
	RunnerID   string     `json:"runnerId"`
	Fence      int64      `json:"fence"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
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
}

// ExecutionView is a run of a plan revision.
type ExecutionView struct {
	ID           string     `json:"id"`
	PlanRevision int        `json:"planRevision"`
	PlanHash     string     `json:"planHash"`
	State        string     `json:"state"`
	Reason       string     `json:"reason,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
	Steps        []StepView `json:"steps"`
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
}

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

func executionView(e domain.Execution) *ExecutionView {
	v := &ExecutionView{ID: e.ID, PlanRevision: e.PlanRevision, PlanHash: e.PlanHash, State: string(e.State),
		Reason: e.Reason, CreatedAt: e.CreatedAt, FinishedAt: e.FinishedAt, Steps: make([]StepView, 0, len(e.Steps))}
	for _, s := range e.Steps {
		sv := StepView{ID: s.ID, Key: s.Key, Title: s.Title, Repository: s.Repository, DependsOn: orEmpty(s.DependsOn),
			State: string(s.State), WaitReason: string(s.WaitReason), Reason: s.Reason, RunnerID: s.RunnerID, Fence: s.Fence,
			Attempts: make([]AttemptView, 0, len(s.Attempts))}
		for _, a := range s.Attempts {
			sv.Attempts = append(sv.Attempts, AttemptView{Number: a.Number, RunnerID: a.RunnerID, Fence: a.Fence,
				State: string(a.State), Error: a.Error, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt})
		}
		v.Steps = append(v.Steps, sv)
	}
	return v
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ts, err := h.Service.ListTasks(r.Context(), p, r.URL.Query().Get("projectId"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]TaskView, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskView(t))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request, p auth.Principal, id string, status int) {
	d, err := h.Service.TaskDetail(r.Context(), p, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	v := DetailView{Task: taskView(d.Task), CanManage: d.CanManage}
	if d.Revision != nil {
		rev := d.Revision
		v.Revision = &RevisionView{Number: rev.Number(), Hash: rev.Hash(), Risk: string(rev.Risk()),
			Summary: rev.Plan().Summary, CreatedBy: rev.CreatedBy(), CreatedAt: rev.CreatedAt()}
	}
	if d.Execution != nil {
		v.Execution = executionView(*d.Execution)
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
