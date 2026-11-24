package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// InvitationsHandler serves invitation management (Owner) and the public preview/accept endpoints. The token is
// accepted only in JSON bodies, never in URLs, so it stays out of access logs and Referer headers.
type InvitationsHandler struct {
	Service *app.InvitationsService
	Limiter *ratelimit.Limiter // preview/accept attempts per client address
	Logger  *slog.Logger
}

// InvitationView is an invitation as listed for the Owner (never the token or its hash).
type InvitationView struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	Role        string    `json:"role"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// CreateInvitationRequest is the body of POST /api/invitations; TTLHours 0 means the default (72h).
type CreateInvitationRequest struct {
	ProjectID string `json:"projectId"`
	Role      string `json:"role"`
	TTLHours  int    `json:"ttlHours"`
}

// CreateInvitationResult carries the token, shown to the Owner once.
type CreateInvitationResult struct {
	Invitation InvitationView `json:"invitation"`
	Token      string         `json:"token"`
}

// InvitationTokenRequest is the body of POST /api/invitations/preview.
type InvitationTokenRequest struct {
	Token string `json:"token"`
}

// AcceptInvitationRequest is the body of POST /api/invitations/accept.
type AcceptInvitationRequest struct {
	Token    string `json:"token"`
	Login    string `json:"login"`
	Password string `json:"password"`
}

// InvitationPreviewView is what the token holder sees before accepting.
type InvitationPreviewView struct {
	ProjectName string    `json:"projectName"`
	Role        string    `json:"role"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Register mounts the routes. Preview and accept are public (see publicAPI).
func (h *InvitationsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/invitations", h.list)
	mux.HandleFunc("POST /api/invitations", h.create)
	mux.HandleFunc("POST /api/invitations/{id}/revoke", h.revoke)
	mux.HandleFunc("POST /api/invitations/preview", h.preview)
	mux.HandleFunc("POST /api/invitations/accept", h.accept)
}

func (h *InvitationsHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func invitationView(v app.InvitationView) InvitationView {
	i := v.Invitation
	return InvitationView{ID: i.ID, ProjectID: i.ProjectID, ProjectName: v.ProjectName, Role: i.Role,
		State: string(v.State), CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt}
}

func (h *InvitationsHandler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	gone := func(code, msg string) { httpx.WriteError(w, http.StatusGone, code, msg) }
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "invitation_not_found", "invitation not found")
	case errors.Is(err, domain.ErrInvitationInvalid):
		httpx.WriteError(w, http.StatusNotFound, "invitation_invalid", "invalid invitation")
	case errors.Is(err, domain.ErrInvitationUsed):
		gone("invitation_used", "invitation already used")
	case errors.Is(err, domain.ErrInvitationRevoked):
		gone("invitation_revoked", "invitation revoked")
	case errors.Is(err, domain.ErrInvitationExpired):
		gone("invitation_expired", "invitation expired")
	case errors.Is(err, domain.ErrLoginTaken):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "login_taken", Message: "login is already taken", Field: "login"})
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("invitations API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (h *InvitationsHandler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	vs, err := h.Service.List(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]InvitationView, 0, len(vs))
	for _, v := range vs {
		out = append(out, invitationView(v))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *InvitationsHandler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var in CreateInvitationRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, token, err := h.Service.Create(r.Context(), p, in.ProjectID, in.Role, in.TTLHours)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, CreateInvitationResult{Invitation: invitationView(v), Token: token})
}

func (h *InvitationsHandler) revoke(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.Revoke(r.Context(), p, r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *InvitationsHandler) limited(w http.ResponseWriter, r *http.Request) bool {
	client := httpx.ClientIP(r)
	if h.Limiter.Allow(client) {
		return false
	}
	h.logger().Warn("invitation attempts rate limited", "client", client)
	rateLimited(w)
	return true
}

func (h *InvitationsHandler) preview(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var in InvitationTokenRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	pv, err := h.Service.Preview(r.Context(), in.Token)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, InvitationPreviewView{ProjectName: pv.ProjectName, Role: pv.Role, ExpiresAt: pv.ExpiresAt})
}

func (h *InvitationsHandler) accept(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var in AcceptInvitationRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	u, err := h.Service.Accept(r.Context(), in.Token, in.Login, in.Password)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, LoginResult{User: view(u)})
}
