// Package httpapi exposes maintenance mode (KIN-41): GET /api/maintenance for every signed-in user (the UI banner),
// PUT /api/maintenance for the Owner, and the read-only middleware that refuses user changes while it is on.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/maintenance/app"
	"github.com/tr1gl1de/kindevai/internal/maintenance/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves the maintenance API.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// StatusView is the response of GET/PUT /api/maintenance.
type StatusView struct {
	Enabled        bool       `json:"enabled"`
	Reason         string     `json:"reason"`
	Since          *time.Time `json:"since"`
	By             string     `json:"by"`
	ActiveAttempts int        `json:"activeAttempts"` // claimed or running steps still draining
}

// SetRequest is the body of PUT /api/maintenance.
type SetRequest struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/maintenance", h.get)
	mux.HandleFunc("PUT /api/maintenance", h.put)
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func view(st app.Status) StatusView {
	v := StatusView{Enabled: st.Enabled, Reason: st.Reason, By: st.By, ActiveAttempts: st.ActiveAttempts}
	if st.Enabled && !st.Since.IsZero() {
		since := st.Since
		v.Since = &since
	}
	return v
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		return
	}
	st, err := h.Service.Status(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view(st))
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		return
	}
	var req SetRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	st, err := h.Service.Set(r.Context(), p, req.Enabled, req.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view(st))
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "only the Owner can switch maintenance mode")
	case errors.Is(err, domain.ErrInvalidReason):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: "reason_invalid", Field: "reason"})
	default:
		h.logger().Error("maintenance API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// ReadOnly refuses API changes (any method but GET, HEAD, OPTIONS) with 503 "maintenance" while maintenance mode
// is on, except the paths domain.WriteAllowed lets through. Reads, the UI and runner connections are unaffected.
func (h *Handler) ReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if strings.HasPrefix(r.URL.Path, "/api/") && h.Service.Mode().Enabled && !domain.WriteAllowed(r.URL.Path) {
				w.Header().Set("Retry-After", "60")
				httpx.WriteError(w, http.StatusServiceUnavailable, "maintenance",
					"the platform is in maintenance mode: changes are paused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
