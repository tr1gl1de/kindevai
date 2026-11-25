package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// WebhookPath receives YouTrack webhook calls. It needs no session (see the identity middleware's public paths);
// the caller proves itself with the shared secret in WebhookSecretHeader.
const (
	WebhookPath         = "/api/webhooks/youtrack"
	WebhookSecretHeader = "X-Kindevai-Webhook-Secret" //nolint:gosec // G101: a header name, not a credential
)

// IntakeHandler serves the YouTrack intake (KIN-27): the Owner's settings under /api/integrations/youtrack/...,
// the members' view and "run by issue ID" under /api/projects/{id}/youtrack and the public webhook.
type IntakeHandler struct {
	Intake         *app.Intake
	WebhookLimiter *ratelimit.Limiter // webhook calls per client address; nil = unlimited
	RunLimiter     *ratelimit.Limiter // runs by ID per user; nil = unlimited
	Logger         *slog.Logger
}

// IntakeSettings is GET/PUT /api/integrations/youtrack/intake.
type IntakeSettings struct {
	PollIntervalSeconds int           `json:"pollIntervalSeconds"`
	WebhookSecret       SecretMeta    `json:"webhookSecret"`
	WebhookPath         string        `json:"webhookPath"`
	WebhookHeader       string        `json:"webhookHeader"`
	Links               []UserLink    `json:"links"`
	Health              AdapterHealth `json:"health"`
}

// AdapterHealth is the YouTrack adapter's health for the Owner.
type AdapterHealth struct {
	State               string     `json:"state"` // ok | degraded | circuit_open
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	LastError           string     `json:"lastError,omitempty"`
	LastFailureAt       *time.Time `json:"lastFailureAt"`
}

// UserLink is a YouTrack login linked to an account.
type UserLink struct {
	UserID        string    `json:"userId"`
	Login         string    `json:"login"`
	YouTrackLogin string    `json:"youtrackLogin"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// PollIntervalRequest changes the poll interval.
type PollIntervalRequest struct {
	PollIntervalSeconds int `json:"pollIntervalSeconds"`
}

// WebhookSecretRequest sets the webhook secret.
type WebhookSecretRequest struct {
	Secret string `json:"secret"` //nolint:gosec // G117: write-only input, never serialised in a response
}

// UserLinkRequest links a YouTrack login to the account with the given login.
type UserLinkRequest struct {
	Login         string `json:"login"`
	YouTrackLogin string `json:"youtrackLogin"`
}

// TriggerTagRequest changes a binding's trigger tag ("" = default).
type TriggerTagRequest struct {
	TriggerTag string `json:"triggerTag"`
}

// AutoPlanRequest turns automatic planning of the project's intake tasks on or off (KIN-85).
type AutoPlanRequest struct {
	AutoPlan bool `json:"autoPlan"`
}

// Binding is a project's YouTrack binding as members see it.
type Binding struct {
	ShortName  string `json:"shortName"`
	TriggerTag string `json:"triggerTag"`
	AutoPlan   bool   `json:"autoPlan"` // KIN-85: intake tasks go to the AI planner at once
}

func bindingView(b domain.Binding) Binding {
	return Binding{ShortName: b.ShortName, TriggerTag: b.TriggerTag, AutoPlan: b.AutoPlan}
}

// IntakeEntry is one intake decision.
type IntakeEntry struct {
	IssueID     string    `json:"issueId"`
	Outcome     string    `json:"outcome"` // queued | created | updated | unchanged | rejected
	Reason      string    `json:"reason,omitempty"`
	Initiator   string    `json:"initiator,omitempty"` // platform login
	Reporter    string    `json:"reporter,omitempty"`  // YouTrack login
	TaskID      string    `json:"taskId,omitempty"`
	Channel     string    `json:"channel"`
	ProcessedAt time.Time `json:"processedAt"`
}

// ProjectYouTrack is GET /api/projects/{id}/youtrack for members who may start AI in the project.
type ProjectYouTrack struct {
	Connected    bool          `json:"connected"`
	Binding      *Binding      `json:"binding"`
	Status       string        `json:"status"` // ok | degraded | circuit_open
	LastPolledAt *time.Time    `json:"lastPolledAt"`
	Recent       []IntakeEntry `json:"recent"`
}

// RunRequest is POST /api/projects/{id}/youtrack/runs.
type RunRequest struct {
	IssueID string `json:"issueId"`
}

// Register mounts the routes.
func (h *IntakeHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/youtrack/intake", h.settings)
	mux.HandleFunc("PUT /api/integrations/youtrack/intake", h.putInterval)
	mux.HandleFunc("PUT /api/integrations/youtrack/webhook", h.putWebhook)
	mux.HandleFunc("DELETE /api/integrations/youtrack/webhook", h.deleteWebhook)
	mux.HandleFunc("POST /api/integrations/youtrack/users", h.link)
	mux.HandleFunc("DELETE /api/integrations/youtrack/users/{user}", h.unlink)
	mux.HandleFunc("PUT /api/projects/{id}/integrations/youtrack/trigger", h.trigger)
	mux.HandleFunc("PUT /api/projects/{id}/integrations/youtrack/autoplan", h.autoPlan)
	mux.HandleFunc("GET /api/projects/{id}/youtrack", h.project)
	mux.HandleFunc("POST /api/projects/{id}/youtrack/runs", h.run)
	mux.HandleFunc("POST "+WebhookPath, h.webhook)
}

func (h *IntakeHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func (h *IntakeHandler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotBound):
		httpx.WriteError(w, http.StatusConflict, "not_bound", "the project is not bound to a YouTrack project")
	case errors.Is(err, domain.ErrYouTrackLoginTaken):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "youtrack_login_taken", Message: "YouTrack login already linked", Field: "youtrackLogin"})
	default:
		(&Handler{Logger: h.Logger}).fail(w, err)
	}
}

func (h *IntakeHandler) settings(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	o, err := h.Intake.IntakeOverview(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := IntakeSettings{PollIntervalSeconds: int(o.Settings.PollInterval / time.Second), WebhookSecret: SecretMeta{Set: o.WebhookSecret.Set},
		WebhookPath: WebhookPath, WebhookHeader: WebhookSecretHeader, Links: make([]UserLink, 0, len(o.Links)),
		Health: AdapterHealth{State: string(o.Health.State), ConsecutiveFailures: o.Health.ConsecutiveFailures,
			LastError: o.Health.LastError, LastFailureAt: o.Health.LastFailureAt}}
	if o.WebhookSecret.Set {
		at := o.WebhookSecret.UpdatedAt
		out.WebhookSecret.UpdatedAt = &at
	}
	if out.Health.State == "" {
		out.Health.State = "ok"
	}
	for _, l := range o.Links {
		out.Links = append(out.Links, UserLink{UserID: l.UserID, Login: l.Login, YouTrackLogin: l.YouTrackLogin, UpdatedAt: l.UpdatedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *IntakeHandler) putInterval(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req PollIntervalRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if err := h.Intake.SetPollInterval(r.Context(), p, req.PollIntervalSeconds); err != nil {
		h.fail(w, err)
		return
	}
	h.settings(w, r)
}

func (h *IntakeHandler) putWebhook(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req WebhookSecretRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Secret == "" { // removing goes through DELETE
		h.fail(w, &domain.ValidationError{Field: "webhookSecret", Code: "webhook_secret_range"})
		return
	}
	if err := h.Intake.SetWebhookSecret(r.Context(), p, req.Secret); err != nil {
		h.fail(w, err)
		return
	}
	h.settings(w, r)
}

func (h *IntakeHandler) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Intake.SetWebhookSecret(r.Context(), p, ""); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IntakeHandler) link(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req UserLinkRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	l, err := h.Intake.LinkUser(r.Context(), p, req.Login, req.YouTrackLogin)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, UserLink{UserID: l.UserID, Login: l.Login, YouTrackLogin: l.YouTrackLogin, UpdatedAt: l.UpdatedAt})
}

func (h *IntakeHandler) unlink(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Intake.UnlinkUser(r.Context(), p, r.PathValue("user")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *IntakeHandler) trigger(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req TriggerTagRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	b, err := h.Intake.SetTriggerTag(r.Context(), p, r.PathValue("id"), req.TriggerTag)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bindingView(b))
}

func (h *IntakeHandler) autoPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req AutoPlanRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	b, err := h.Intake.SetAutoPlan(r.Context(), p, r.PathValue("id"), req.AutoPlan)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, bindingView(b))
}

func entryView(e app.IntakeEntry) IntakeEntry {
	return IntakeEntry{IssueID: e.IssueID, Outcome: string(e.Outcome), Reason: e.Reason, Initiator: e.InitiatorLogin,
		Reporter: e.Reporter, TaskID: e.TaskID, Channel: e.Channel, ProcessedAt: e.ProcessedAt}
}

func (h *IntakeHandler) project(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Intake.ProjectIntake(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := ProjectYouTrack{Connected: v.Connected, Status: string(v.Status.State), LastPolledAt: v.Status.LastPolledAt,
		Recent: make([]IntakeEntry, 0, len(v.Recent))}
	if out.Status == "" {
		out.Status = "ok"
	}
	if v.Binding != nil {
		b := bindingView(*v.Binding)
		out.Binding = &b
	}
	for _, e := range v.Recent {
		out.Recent = append(out.Recent, entryView(e))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *IntakeHandler) run(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if h.RunLimiter != nil && !h.RunLimiter.Allow(p.UserID) {
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
		return
	}
	var req RunRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	rec, err := h.Intake.RunIssue(r.Context(), p, r.PathValue("id"), req.IssueID)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, entryView(app.IntakeEntry{IntakeRecord: rec, InitiatorLogin: p.Login}))
}

// webhook answers 202 only after the hint is stored in the inbox; it never says more than the status code.
func (h *IntakeHandler) webhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.WebhookLimiter != nil && !h.WebhookLimiter.Allow(httpx.ClientIP(r)) {
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
	if err != nil {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
		return
	}
	err = h.Intake.ReceiveWebhook(r.Context(), r.Header.Get(WebhookSecretHeader), body)
	var ve *domain.ValidationError
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, domain.ErrWebhookDisabled):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, domain.ErrWebhookUnauthorized):
		h.logger().Warn("youtrack webhook with a wrong secret", "client", httpx.ClientIP(r))
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid webhook secret")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("youtrack webhook", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}
