// Package httpapi exposes the identity context over the Control Plane JSON API.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// SetupHandler serves the First-run Wizard API.
type SetupHandler struct {
	Service       *app.SetupService
	Limiter       *ratelimit.Limiter // setup attempts per client address
	StatusLimiter *ratelimit.Limiter // status checks per client address; defaults to Limiter
	Logger        *slog.Logger
}

// SetupStatus is the response of GET /api/setup/status.
type SetupStatus struct {
	SetupRequired bool `json:"setupRequired"`
}

// SetupRequest is the body of POST /api/setup. The token travels only in the body, never in the URL.
type SetupRequest struct {
	Token        string `json:"token"`
	Login        string `json:"login"`
	Password     string `json:"password"`
	PlatformName string `json:"platformName"`
}

// SetupResult is the response of a successful POST /api/setup.
type SetupResult struct {
	Login string `json:"login"`
}

// Register mounts the routes.
func (h *SetupHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/setup/status", httpx.Allow([]string{http.MethodGet}, h.status))
	mux.Handle("/api/setup", httpx.Allow([]string{http.MethodPost}, h.complete))
}

func (h *SetupHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func (h *SetupHandler) status(w http.ResponseWriter, r *http.Request) {
	lim := h.StatusLimiter
	if lim == nil {
		lim = h.Limiter
	}
	if !lim.Allow(httpx.ClientIP(r)) {
		rateLimited(w)
		return
	}
	required, err := h.Service.Status(r.Context())
	if err != nil {
		h.logger().Error("setup status", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, SetupStatus{SetupRequired: required})
}

func (h *SetupHandler) complete(w http.ResponseWriter, r *http.Request) {
	if !h.Limiter.Allow(httpx.ClientIP(r)) {
		rateLimited(w)
		return
	}
	var in SetupRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	owner, err := h.Service.Complete(r.Context(), app.CompleteSetupInput{
		Token: in.Token, Login: in.Login, Password: in.Password, PlatformName: in.PlatformName,
	})
	var ve *domain.ValidationError
	switch {
	case err == nil:
		h.logger().Info("setup completed, owner created", "login", string(owner.Login))
		httpx.WriteJSON(w, http.StatusCreated, SetupResult{Login: string(owner.Login)})
	case errors.Is(err, domain.ErrSetupCompleted):
		httpx.WriteError(w, http.StatusConflict, "setup_completed", "setup is already completed")
	case errors.Is(err, domain.ErrInvalidSetupToken):
		h.logger().Warn("setup attempt with invalid token", "client", httpx.ClientIP(r))
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_setup_token", "invalid or expired setup token")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("setup failed", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func rateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
}
