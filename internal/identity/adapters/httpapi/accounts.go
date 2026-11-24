package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// AccountsHandler serves account management (Owner) and the self-service password change.
type AccountsHandler struct {
	Service *app.AccountsService
	Limiter *ratelimit.Limiter // failed password-change attempts per account (successes are free)
	Logger  *slog.Logger
}

// AccountView is an account as listed for the Owner (never the password hash).
type AccountView struct {
	UserView
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreateUserRequest is the body of POST /api/users.
type CreateUserRequest struct {
	Login string `json:"login"`
}

// TemporaryPasswordResult carries a server-generated password, shown to the Owner once.
type TemporaryPasswordResult struct {
	User              *AccountView `json:"user,omitempty"`
	TemporaryPassword string       `json:"temporaryPassword"`
}

// ChangePasswordRequest is the body of POST /api/auth/password.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// Register mounts the routes.
func (h *AccountsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/password", h.changePassword)
	mux.HandleFunc("GET /api/users", h.list)
	mux.HandleFunc("POST /api/users", h.create)
	mux.HandleFunc("POST /api/users/{id}/disable", h.setDisabled(true))
	mux.HandleFunc("POST /api/users/{id}/enable", h.setDisabled(false))
	mux.HandleFunc("POST /api/users/{id}/reset-password", h.resetPassword)
}

func (h *AccountsHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func accountView(u domain.User) AccountView {
	return AccountView{UserView: view(u), Disabled: u.Disabled, CreatedAt: u.CreatedAt}
}

func principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
	}
	return p, ok
}

func (h *AccountsHandler) fail(w http.ResponseWriter, err error, passwordField string) {
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "user_not_found", "user not found")
	case errors.Is(err, domain.ErrLoginTaken):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "login_taken", Message: "login is already taken", Field: "login"})
	case errors.Is(err, domain.ErrInvalidCredentials):
		httpx.WriteJSON(w, http.StatusUnauthorized, httpx.ErrorBody{Error: "invalid_credentials", Message: "current password is wrong", Field: "currentPassword"})
	case errors.As(err, &ve):
		field := ve.Field
		if field == "password" && passwordField != "" {
			field = passwordField
		}
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: field})
	default:
		h.logger().Error("accounts API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (h *AccountsHandler) changePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if !h.Limiter.Peek(p.UserID) {
		h.logger().Warn("password change rate limited", "user", p.Login)
		rateLimited(w)
		return
	}
	var in ChangePasswordRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	var keep []byte
	if c, err := r.Cookie(SessionCookie); err == nil {
		keep = domain.HashSessionToken(c.Value)
	}
	if err := h.Service.ChangePassword(r.Context(), p, in.CurrentPassword, in.NewPassword, keep); err != nil {
		if errors.Is(err, domain.ErrInvalidCredentials) {
			h.Limiter.Allow(p.UserID) // only wrong current passwords count (guessing with a stolen session)
		}
		h.fail(w, err, "newPassword")
		return
	}
	h.logger().Info("password changed", "user", p.Login)
	w.WriteHeader(http.StatusNoContent)
}

func (h *AccountsHandler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	users, err := h.Service.ListUsers(r.Context(), p)
	if err != nil {
		h.fail(w, err, "")
		return
	}
	out := make([]AccountView, 0, len(users))
	for _, u := range users {
		out = append(out, accountView(u))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *AccountsHandler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var in CreateUserRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	u, temp, err := h.Service.CreateUser(r.Context(), p, in.Login)
	if err != nil {
		h.fail(w, err, "")
		return
	}
	h.logger().Info("account created", "login", string(u.Login), "by", p.Login)
	v := accountView(u)
	httpx.WriteJSON(w, http.StatusCreated, TemporaryPasswordResult{User: &v, TemporaryPassword: temp})
}

func (h *AccountsHandler) setDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principal(w, r)
		if !ok {
			return
		}
		if err := h.Service.SetDisabled(r.Context(), p, r.PathValue("id"), disabled); err != nil {
			h.fail(w, err, "")
			return
		}
		h.logger().Info("account status changed", "user", r.PathValue("id"), "disabled", disabled, "by", p.Login)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *AccountsHandler) resetPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	temp, err := h.Service.ResetPassword(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err, "")
		return
	}
	h.logger().Info("password reset", "user", r.PathValue("id"), "by", p.Login)
	httpx.WriteJSON(w, http.StatusOK, TemporaryPasswordResult{TemporaryPassword: temp})
}
