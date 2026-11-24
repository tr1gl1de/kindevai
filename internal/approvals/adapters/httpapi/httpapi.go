// Package httpapi exposes the Approval Center (/api/approvals) and the project risk policy
// (/api/projects/{id}/risk-policy). Only the Owner approves or rejects; Project Admins see their projects'
// approvals and policy.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves the approvals API.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// FindingView is a policy rule that matched a path.
type FindingView struct {
	Category   string `json:"category"`
	Risk       string `json:"risk"`
	Pattern    string `json:"pattern"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
}

// ApprovalView is an approval in the list and the details.
type ApprovalView struct {
	ID           string        `json:"id"`
	ProjectID    string        `json:"projectId"`
	ProjectName  string        `json:"projectName,omitempty"`
	TaskID       string        `json:"taskId"`
	TaskTitle    string        `json:"taskTitle"`
	PlanRevision int           `json:"planRevision"`
	PlanHash     string        `json:"planHash"`
	Scope        string        `json:"scope"`
	Reason       string        `json:"reason"`
	StepKey      string        `json:"stepKey,omitempty"`
	Paths        []string      `json:"paths"`
	Risk         string        `json:"risk"`
	ProposedRisk string        `json:"proposedRisk,omitempty"`
	Findings     []FindingView `json:"findings"`
	State        string        `json:"state"`
	Approver     string        `json:"approver,omitempty"`
	Comment      string        `json:"comment,omitempty"`
	RequestedAt  time.Time     `json:"requestedAt"`
	DecidedAt    *time.Time    `json:"decidedAt"`
}

// StepView is one plan step.
type StepView struct {
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	Repository   string   `json:"repository"`
	Scope        []string `json:"scope"`
	DependsOn    []string `json:"dependsOn"`
	Capabilities []string `json:"capabilities"`
}

// PlanView is a plan as the planner proposed it.
type PlanView struct {
	Summary     string     `json:"summary"`
	Risk        string     `json:"risk"`
	MaxParallel int        `json:"maxParallel"`
	Steps       []StepView `json:"steps"`
}

// RevisionView is a plan revision.
type RevisionView struct {
	Number    int       `json:"number"`
	Hash      string    `json:"hash"`
	Risk      string    `json:"risk"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	Plan      PlanView  `json:"plan"`
}

// TaskView is the task an approval belongs to.
type TaskView struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	State        string `json:"state"`
	PlanRevision int    `json:"planRevision"`
}

// ChangeView is one difference from the previous revision.
type ChangeView struct {
	Kind    string `json:"kind"`
	StepKey string `json:"stepKey,omitempty"`
	Field   string `json:"field"`
	Before  string `json:"before"`
	After   string `json:"after"`
}

// DetailView is GET /api/approvals/{id}.
type DetailView struct {
	Approval  ApprovalView   `json:"approval"`
	Task      TaskView       `json:"task"`
	Revision  RevisionView   `json:"revision"`
	Previous  *RevisionView  `json:"previous"`
	Changes   []ChangeView   `json:"changes"`
	Related   []ApprovalView `json:"related"`
	CanDecide bool           `json:"canDecide"`
}

// DecisionRequest is the body of approve and reject: the revision and hash the Owner reviewed.
type DecisionRequest struct {
	PlanRevision int    `json:"planRevision"`
	PlanHash     string `json:"planHash"`
	Comment      string `json:"comment"`
}

// RuleView is one category rule of a risk policy.
type RuleView struct {
	Category string   `json:"category"`
	Risk     string   `json:"risk"`
	Patterns []string `json:"patterns"`
}

// PolicyView is GET/PUT /api/projects/{id}/risk-policy.
type PolicyView struct {
	Custom bool       `json:"custom"`
	Rules  []RuleView `json:"rules"`
}

// PolicyRequest is the body of PUT /api/projects/{id}/risk-policy.
type PolicyRequest struct {
	Rules []RuleView `json:"rules"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/approvals", h.list)
	mux.HandleFunc("GET /api/approvals/{id}", h.get)
	mux.HandleFunc("POST /api/approvals/{id}/approve", h.decide(true))
	mux.HandleFunc("POST /api/approvals/{id}/reject", h.decide(false))
	mux.HandleFunc("GET /api/projects/{id}/risk-policy", h.policy)
	mux.HandleFunc("PUT /api/projects/{id}/risk-policy", h.setPolicy)
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// principal returns the caller; the identity middleware already rejects anonymous calls, this is defence in depth.
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
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the Owner may do this")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "approval or project not found")
	case errors.Is(err, domain.ErrStaleRevision):
		httpx.WriteError(w, http.StatusConflict, "stale_revision", "the plan revision is no longer current")
	case errors.Is(err, domain.ErrAlreadyDecided), errors.Is(err, app.ErrConflict):
		httpx.WriteError(w, http.StatusConflict, "already_decided", "the approval was already decided")
	case errors.Is(err, domain.ErrNotReady):
		httpx.WriteError(w, http.StatusConflict, "not_ready", "the task is not ready for this decision")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("approvals API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func approvalView(a domain.Approval, project string) ApprovalView {
	v := ApprovalView{ID: a.ID, ProjectID: a.ProjectID, ProjectName: project, TaskID: a.TaskID, TaskTitle: a.TaskTitle,
		PlanRevision: a.Revision, PlanHash: a.PlanHash, Scope: string(a.Scope), Reason: string(a.Reason),
		StepKey: a.StepKey, Paths: a.Paths, Risk: string(a.Risk), ProposedRisk: string(a.ProposedRisk),
		Findings: make([]FindingView, 0, len(a.Findings)), State: string(a.State), Approver: a.Approver,
		Comment: a.Comment, RequestedAt: a.RequestedAt, DecidedAt: a.DecidedAt}
	if v.Paths == nil {
		v.Paths = []string{}
	}
	for _, f := range a.Findings {
		v.Findings = append(v.Findings, FindingView{Category: string(f.Category), Risk: string(f.Risk),
			Pattern: f.Pattern, Repository: f.Repository, Path: f.Path})
	}
	return v
}

func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func revisionView(r app.RevisionRef) RevisionView {
	v := RevisionView{Number: r.Number, Hash: r.Hash, Risk: string(r.Risk), CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt,
		Plan: PlanView{Summary: r.Plan.Summary, Risk: string(r.Plan.Risk), MaxParallel: r.Plan.MaxParallel,
			Steps: make([]StepView, 0, len(r.Plan.Steps))}}
	for _, s := range r.Plan.Steps {
		v.Plan.Steps = append(v.Plan.Steps, StepView{Key: s.Key, Title: s.Title, Repository: s.Repository,
			Scope: orEmpty(s.Scope), DependsOn: orEmpty(s.DependsOn), Capabilities: orEmpty(s.Capabilities)})
	}
	return v
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var q app.ListQuery
	switch r.URL.Query().Get("state") {
	case "", "all":
	case "pending":
		q.States = []domain.State{domain.StatePending}
	case "decided":
		q.States = []domain.State{domain.StateApproved, domain.StateRejected, domain.StateSuperseded}
	default:
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "state_unknown", Field: "state"})
		return
	}
	items, err := h.Service.List(r.Context(), p, q)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]ApprovalView, 0, len(items))
	for _, it := range items {
		out = append(out, approvalView(it.Approval, it.ProjectName))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	d, err := h.Service.Get(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, detailView(d))
}

func detailView(d app.Detail) DetailView {
	out := DetailView{Approval: approvalView(d.Approval, d.ProjectName),
		Task:     TaskView{ID: d.Task.ID, Title: d.Task.Title, State: d.Task.State, PlanRevision: d.Task.PlanRevision},
		Revision: revisionView(d.Revision), Changes: make([]ChangeView, 0, len(d.Changes)),
		Related: make([]ApprovalView, 0, len(d.Related)), CanDecide: d.CanDecide}
	if d.Previous != nil {
		pv := revisionView(*d.Previous)
		out.Previous = &pv
	}
	for _, c := range d.Changes {
		out.Changes = append(out.Changes, ChangeView{Kind: string(c.Kind), StepKey: c.StepKey, Field: c.Field,
			Before: c.Before, After: c.After})
	}
	for _, a := range d.Related {
		out.Related = append(out.Related, approvalView(a, d.ProjectName))
	}
	return out
}

func (h *Handler) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principal(w, r)
		if !ok {
			return
		}
		var req DecisionRequest
		if !httpx.DecodeJSON(w, r, &req) {
			return
		}
		a, err := h.Service.Decide(r.Context(), p, r.PathValue("id"), app.DecideInput{Approve: approve,
			Revision: req.PlanRevision, PlanHash: req.PlanHash, Comment: req.Comment})
		if err != nil {
			h.fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, approvalView(a, ""))
	}
}

func policyView(v app.PolicyView) PolicyView {
	out := PolicyView{Custom: v.Custom, Rules: make([]RuleView, 0, len(v.Policy.Rules))}
	for _, r := range v.Policy.Rules {
		out.Rules = append(out.Rules, RuleView{Category: string(r.Category), Risk: string(r.Risk), Patterns: orEmpty(r.Patterns)})
	}
	return out
}

func (h *Handler) policy(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.Policy(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policyView(v))
}

func (h *Handler) setPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req PolicyRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rules := make([]domain.Rule, 0, len(req.Rules))
	for _, x := range req.Rules {
		rules = append(rules, domain.Rule{Category: domain.Category(x.Category), Risk: domain.Risk(x.Risk), Patterns: x.Patterns})
	}
	v, err := h.Service.SetPolicy(r.Context(), p, r.PathValue("id"), rules)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, policyView(v))
}
