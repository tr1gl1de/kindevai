// Package httpapi exposes the Alert Center (/api/alerts) and the signed-in user's notification preferences
// (/api/notifications/preferences). Visibility follows project membership: platform alerts are the Owner's.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves the alerts API.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// AlertView is an alert in the list.
type AlertView struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"projectId,omitempty"`
	ProjectName string            `json:"projectName,omitempty"`
	Kind        string            `json:"kind"`
	Severity    string            `json:"severity"`
	Subject     string            `json:"subject"`
	Details     map[string]string `json:"details"`
	State       string            `json:"state"`
	Occurrences int               `json:"occurrences"`
	FirstSeenAt time.Time         `json:"firstSeenAt"`
	LastSeenAt  time.Time         `json:"lastSeenAt"`
	AckedAt     *time.Time        `json:"ackedAt"`
	ClosedAt    *time.Time        `json:"closedAt"`
	CanManage   bool              `json:"canManage"`
	// KIN-61: the Owner / Project Admin may suggest an analysis task; the requested or created one, if any.
	CanSuggestAnalysis bool              `json:"canSuggestAnalysis"`
	AnalysisTask       *AnalysisTaskView `json:"analysisTask"`
}

// PreferencesRequest is the body of PUT /api/notifications/preferences.
type PreferencesRequest struct {
	Language string   `json:"language"`
	Disabled []string `json:"disabled"`
}

// PreferencesView is the response of GET/PUT /api/notifications/preferences.
type PreferencesView struct {
	Language       string   `json:"language"`
	Disabled       []string `json:"disabled"`
	Types          []string `json:"types"` // every notification type
	TelegramLinked bool     `json:"telegramLinked"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/alerts", h.list)
	mux.HandleFunc("POST /api/alerts/{id}/{action}", h.act)
	mux.HandleFunc("GET /api/alerts/{id}/analysis-task", h.previewAnalysis)
	mux.HandleFunc("POST /api/alerts/{id}/analysis-task", h.suggestAnalysis)
	mux.HandleFunc("GET /api/notifications/preferences", h.preferences)
	mux.HandleFunc("PUT /api/notifications/preferences", h.savePreferences)
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
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "alert not found")
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the Owner or a Project Admin may change this alert")
	case errors.Is(err, domain.ErrInvalidTransition):
		httpx.WriteError(w, http.StatusConflict, "invalid_transition", "the alert is not in a state that allows this")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("alerts API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func alertView(v app.View) AlertView {
	a := v.Alert
	d := a.Details
	if d == nil {
		d = map[string]string{}
	}
	return AlertView{ID: a.ID, ProjectID: a.ProjectID, ProjectName: v.ProjectName, Kind: string(a.Kind),
		Severity: string(a.Severity), Subject: a.Subject, Details: d, State: string(a.State), Occurrences: a.Occurrences,
		FirstSeenAt: a.FirstSeenAt, LastSeenAt: a.LastSeenAt, AckedAt: a.AckedAt, ClosedAt: a.ClosedAt,
		CanManage: v.CanManage && a.Active(), CanSuggestAnalysis: v.CanSuggest, AnalysisTask: analysisView(v.Analysis)}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var active bool
	switch r.URL.Query().Get("state") {
	case "", "active":
		active = true
	case "all":
	default:
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "state_unknown", Field: "state"})
		return
	}
	vs, err := h.Service.List(r.Context(), p, active)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]AlertView, 0, len(vs))
	for _, v := range vs {
		out = append(out, alertView(v))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) act(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	act := app.Action(r.PathValue("action"))
	switch act {
	case app.ActionAcknowledge, app.ActionDismiss, app.ActionResolve:
	default:
		httpx.WriteError(w, http.StatusNotFound, "not_found", "unknown action")
		return
	}
	v, err := h.Service.Act(r.Context(), p, r.PathValue("id"), act)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, alertView(v))
}

func preferencesView(v app.PreferencesView) PreferencesView {
	out := PreferencesView{Language: string(v.Preferences.Language), Disabled: []string{},
		TelegramLinked: v.TelegramLinked}
	for _, t := range v.Preferences.Disabled {
		out.Disabled = append(out.Disabled, string(t))
	}
	for _, t := range domain.NotificationTypes {
		out.Types = append(out.Types, string(t))
	}
	return out
}

func (h *Handler) preferences(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.Preferences(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, preferencesView(v))
}

func (h *Handler) savePreferences(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req PreferencesRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if len(req.Disabled) > len(domain.NotificationTypes) {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "too_many", Field: "disabled"})
		return
	}
	types := make([]domain.NotificationType, 0, len(req.Disabled))
	for _, t := range req.Disabled {
		types = append(types, domain.NotificationType(t))
	}
	v, err := h.Service.SavePreferences(r.Context(), p, req.Language, types)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, preferencesView(v))
}
