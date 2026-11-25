package httpapi

import (
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// AnalysisTaskView is an alert's analysis task (KIN-61): requested, then created in YouTrack.
type AnalysisTaskView struct {
	ProjectID       string     `json:"projectId"`
	YouTrackProject string     `json:"youtrackProject"`
	RequestedAt     time.Time  `json:"requestedAt"`
	IssueID         string     `json:"issueId,omitempty"`
	IssueURL        string     `json:"issueUrl,omitempty"`
	CreatedAt       *time.Time `json:"createdAt"`
	Attempt         int        `json:"attempt"`
	Failure         string     `json:"failure,omitempty"` // code of a permanent failure; a new confirmation retries
	FailedAt        *time.Time `json:"failedAt"`
}

// AnalysisTargetView is a project the issue may go to.
type AnalysisTargetView struct {
	ProjectID       string `json:"projectId"`
	ProjectName     string `json:"projectName"`
	YouTrackProject string `json:"youtrackProject"`
}

// AnalysisPreviewView is the response of GET /api/alerts/{id}/analysis-task.
type AnalysisPreviewView struct {
	Summary     string               `json:"summary"`
	Description string               `json:"description"`
	Configured  bool                 `json:"configured"` // YouTrack is connected
	Targets     []AnalysisTargetView `json:"targets"`
	Task        *AnalysisTaskView    `json:"task"`
}

// AnalysisRequest is the body of POST /api/alerts/{id}/analysis-task: the user's explicit confirmation.
type AnalysisRequest struct {
	Language  string `json:"language"`
	ProjectID string `json:"projectId"` // the target of a platform alert
}

func analysisView(t *domain.AnalysisTask) *AnalysisTaskView {
	if t == nil {
		return nil
	}
	return &AnalysisTaskView{ProjectID: t.ProjectID, YouTrackProject: t.YouTrackProject, RequestedAt: t.RequestedAt,
		IssueID: t.IssueID, IssueURL: t.IssueURL, CreatedAt: t.CreatedAt, Attempt: t.Attempt, Failure: t.Failure,
		FailedAt: t.FailedAt}
}

func language(w http.ResponseWriter, s string) (domain.Language, bool) {
	switch domain.Language(s) {
	case "", domain.LangEN:
		return domain.LangEN, true
	case domain.LangRU:
		return domain.LangRU, true
	}
	httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "language_unknown", Field: "language"})
	return "", false
}

func (h *Handler) previewAnalysis(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	l, ok := language(w, r.URL.Query().Get("lang"))
	if !ok {
		return
	}
	v, err := h.Service.PreviewAnalysis(r.Context(), p, r.PathValue("id"), l)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := AnalysisPreviewView{Summary: v.Draft.Summary, Description: v.Draft.Description, Configured: v.Configured,
		Targets: []AnalysisTargetView{}, Task: analysisView(v.Task)}
	for _, t := range v.Targets {
		out.Targets = append(out.Targets, AnalysisTargetView(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// suggestAnalysis only records the request and enqueues it; YouTrack is called by the outbox, never from here.
func (h *Handler) suggestAnalysis(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req AnalysisRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	l, ok := language(w, req.Language)
	if !ok {
		return
	}
	v, err := h.Service.SuggestAnalysis(r.Context(), p, r.PathValue("id"), req.ProjectID, l)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, alertView(v))
}
