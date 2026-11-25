// Package httpapi exposes the agent router (KIN-33): /api/agents for the Owner (global CLI switches, emergency
// stop, global role bindings, CLI cool-downs) and /api/projects/{id}/agents for the Owner and the project's admin
// (the project's allowlist and binding overrides).
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/routing/app"
	"github.com/tr1gl1de/kindevai/internal/routing/domain"
)

// Handler serves the agent router API.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// ChoiceJSON is a CLI with its model ("" = the CLI's default).
type ChoiceJSON struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}

// BindingRequest is the body of PUT …/bindings/{role}.
type BindingRequest struct {
	Primary  ChoiceJSON   `json:"primary"`
	Fallback []ChoiceJSON `json:"fallback"`
	Strategy string       `json:"strategy"`
}

// BindingJSON is a role's effective binding and where it comes from (default, global, project).
type BindingJSON struct {
	Role      string       `json:"role"`
	Primary   ChoiceJSON   `json:"primary"`
	Fallback  []ChoiceJSON `json:"fallback"`
	Strategy  string       `json:"strategy"`
	Source    string       `json:"source"`
	UpdatedBy string       `json:"updatedBy"`
	UpdatedAt *time.Time   `json:"updatedAt"`
}

// SwitchJSON is a CLI's switch at one level; set is false while it has the default (enabled).
type SwitchJSON struct {
	Agent     string     `json:"agent"`
	Enabled   bool       `json:"enabled"`
	Set       bool       `json:"set"`
	UpdatedBy string     `json:"updatedBy"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// CooldownJSON is a runner's CLI cooling down after a usage limit.
type CooldownJSON struct {
	RunnerID string    `json:"runnerId"`
	Agent    string    `json:"agent"`
	Until    time.Time `json:"until"`
	Reason   string    `json:"reason"`
}

// SettingsJSON is GET /api/agents.
type SettingsJSON struct {
	Switches  []SwitchJSON   `json:"switches"`
	Bindings  []BindingJSON  `json:"bindings"`
	Cooldowns []CooldownJSON `json:"cooldowns"`
}

// AgentStateJSON is a CLI in a project.
type AgentStateJSON struct {
	Agent     string     `json:"agent"`
	Global    bool       `json:"global"`
	Project   SwitchJSON `json:"project"`
	Effective bool       `json:"effective"`
}

// ProjectSettingsJSON is GET /api/projects/{id}/agents.
type ProjectSettingsJSON struct {
	ProjectID string           `json:"projectId"`
	Agents    []AgentStateJSON `json:"agents"`
	Bindings  []BindingJSON    `json:"bindings"`
}

type switchRequest struct {
	Enabled *bool `json:"enabled"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", h.settings)
	mux.HandleFunc("PUT /api/agents/switches/{agent}", h.setGlobalSwitch)
	mux.HandleFunc("POST /api/agents/switches/{agent}/emergency-stop", h.emergencyStop)
	mux.HandleFunc("PUT /api/agents/bindings/{role}", h.setGlobalBinding)
	mux.HandleFunc("DELETE /api/agents/bindings/{role}", h.resetGlobalBinding)
	mux.HandleFunc("GET /api/projects/{id}/agents", h.projectSettings)
	mux.HandleFunc("PUT /api/projects/{id}/agents/switches/{agent}", h.setProjectSwitch)
	mux.HandleFunc("PUT /api/projects/{id}/agents/bindings/{role}", h.setProjectBinding)
	mux.HandleFunc("DELETE /api/projects/{id}/agents/bindings/{role}", h.resetProjectBinding)
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		return p, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return p, true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "project not found")
	case errors.Is(err, domain.ErrDisabledGlobally):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "disabled_globally",
			Message: "the CLI is disabled globally; only the Owner can enable it", Field: "enabled"})
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		logger := h.Logger
		if logger == nil {
			logger = slog.New(slog.DiscardHandler)
		}
		logger.Error("agents API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func switchJSON(v app.SwitchView) SwitchJSON {
	return SwitchJSON{Agent: v.Agent, Enabled: v.Enabled, Set: v.Set, UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt}
}

func bindingJSON(v app.BindingView) BindingJSON {
	b := v.Binding
	out := BindingJSON{Role: string(b.Role), Primary: ChoiceJSON(b.Primary), Fallback: make([]ChoiceJSON, 0, len(b.Fallback)),
		Strategy: string(b.Strategy), Source: v.Source, UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt}
	if out.Strategy == "" {
		out.Strategy = string(domain.StrategyOrdered)
	}
	for _, c := range b.Fallback {
		out.Fallback = append(out.Fallback, ChoiceJSON(c))
	}
	return out
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	s, err := h.Service.Settings(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := SettingsJSON{Switches: []SwitchJSON{}, Bindings: []BindingJSON{}, Cooldowns: []CooldownJSON{}}
	for _, v := range s.Switches {
		out.Switches = append(out.Switches, switchJSON(v))
	}
	for _, v := range s.Bindings {
		out.Bindings = append(out.Bindings, bindingJSON(v))
	}
	for _, c := range s.Cooldowns {
		out.Cooldowns = append(out.Cooldowns, CooldownJSON{RunnerID: c.RunnerID, Agent: c.Agent, Until: c.Until, Reason: c.Reason})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) projectSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	s, err := h.Service.ProjectSettings(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := ProjectSettingsJSON{ProjectID: s.ProjectID, Agents: []AgentStateJSON{}, Bindings: []BindingJSON{}}
	for _, a := range s.Agents {
		out.Agents = append(out.Agents, AgentStateJSON{Agent: a.Agent, Global: a.Global, Project: switchJSON(a.Project),
			Effective: a.Effective})
	}
	for _, v := range s.Bindings {
		out.Bindings = append(out.Bindings, bindingJSON(v))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func decodeSwitch(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var in switchRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return false, false
	}
	if in.Enabled == nil {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "required", Field: "enabled"})
		return false, false
	}
	return *in.Enabled, true
}

func (h *Handler) setGlobalSwitch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if !p.Owner {
		h.fail(w, domain.ErrForbidden)
		return
	}
	on, ok := decodeSwitch(w, r)
	if !ok {
		return
	}
	if err := h.Service.SetGlobalSwitch(r.Context(), p, r.PathValue("agent"), on); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setProjectSwitch(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	on, ok := decodeSwitch(w, r)
	if !ok {
		return
	}
	if err := h.Service.SetProjectSwitch(r.Context(), p, r.PathValue("id"), r.PathValue("agent"), on); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) emergencyStop(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	n, err := h.Service.EmergencyStop(r.Context(), p, r.PathValue("agent"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"stopped": n})
}

func decodeBinding(w http.ResponseWriter, r *http.Request) (domain.Binding, bool) {
	var in BindingRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return domain.Binding{}, false
	}
	b := domain.Binding{Role: domain.Role(r.PathValue("role")), Primary: domain.Choice(in.Primary),
		Fallback: make([]domain.Choice, 0, len(in.Fallback)), Strategy: domain.Strategy(in.Strategy)}
	for _, c := range in.Fallback {
		b.Fallback = append(b.Fallback, domain.Choice(c))
	}
	return b, true
}

func (h *Handler) setGlobalBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if !p.Owner {
		h.fail(w, domain.ErrForbidden)
		return
	}
	b, ok := decodeBinding(w, r)
	if !ok {
		return
	}
	if err := h.Service.SetGlobalBinding(r.Context(), p, b); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setProjectBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	b, ok := decodeBinding(w, r)
	if !ok {
		return
	}
	if err := h.Service.SetProjectBinding(r.Context(), p, r.PathValue("id"), b); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resetGlobalBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.ResetGlobalBinding(r.Context(), p, domain.Role(r.PathValue("role"))); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) resetProjectBinding(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.ResetProjectBinding(r.Context(), p, r.PathValue("id"), domain.Role(r.PathValue("role"))); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
