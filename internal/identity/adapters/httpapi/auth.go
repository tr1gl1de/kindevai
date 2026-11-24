package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// SessionCookie is the name of the session cookie.
const SessionCookie = "kd_session"

// publicAPI lists API paths reachable without a session; everything else under /api/ requires one.
var publicAPI = map[string]bool{
	"/api/health":       true,
	"/api/setup/status": true,
	"/api/setup":        true,
	"/api/auth/login":   true,
	"/api/auth/logout":  true,
}

// AuthHandler serves login/logout/me and authenticates API requests.
type AuthHandler struct {
	Service      *app.AuthService
	IPLimiter    *ratelimit.Limiter // login attempts per client address
	LoginLimiter *ratelimit.Limiter // login attempts per account
	Logger       *slog.Logger
}

// LoginRequest is the body of POST /api/auth/login.
type LoginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// UserView is the public shape of the current user.
type UserView struct {
	ID    string `json:"id"`
	Login string `json:"login"`
	Role  string `json:"role"`
}

// LoginResult is the response of a successful login.
type LoginResult struct {
	User UserView `json:"user"`
}

func view(u domain.User) UserView {
	return UserView{ID: u.ID, Login: string(u.Login), Role: string(u.Role)}
}

// Register mounts the routes.
func (h *AuthHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/auth/login", httpx.Allow([]string{http.MethodPost}, h.login))
	mux.Handle("/api/auth/logout", httpx.Allow([]string{http.MethodPost}, h.logout))
	mux.Handle("/api/auth/me", httpx.Allow([]string{http.MethodGet}, h.me))
}

func (h *AuthHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// Middleware rejects cross-origin state changes (CSRF), resolves the session cookie into an auth.Principal,
// and answers 401 for every non-public /api/ path without a valid session.
func (h *AuthHandler) Middleware(next http.Handler) http.Handler {
	authn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
			u, err := h.Service.Authenticate(r.Context(), c.Value)
			switch {
			case err == nil:
				r = r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
					UserID: u.ID, Login: string(u.Login), Owner: u.Role == domain.RoleOwner,
				}))
			case !errors.Is(err, domain.ErrUnauthenticated):
				h.logger().Error("authenticating session", "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !publicAPI[r.URL.Path] {
			if _, ok := auth.FromContext(r.Context()); !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected")
	}))
	return csrf.Handler(authn)
}

func (h *AuthHandler) login(w http.ResponseWriter, r *http.Request) {
	if !h.IPLimiter.Allow(httpx.ClientIP(r)) {
		rateLimited(w)
		return
	}
	var in LoginRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if !h.LoginLimiter.Allow(strings.ToLower(strings.TrimSpace(in.Login))) {
		rateLimited(w)
		return
	}
	token, u, err := h.Service.Login(r.Context(), in.Login, in.Password)
	switch {
	case errors.Is(err, domain.ErrInvalidCredentials):
		h.logger().Warn("failed login", "client", httpx.ClientIP(r))
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid login or password")
		return
	case err != nil:
		h.logger().Error("login", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: isHTTPS(r), MaxAge: int(domain.SessionMaxAge.Seconds()),
	})
	httpx.WriteJSON(w, http.StatusOK, LoginResult{User: view(u)})
}

func (h *AuthHandler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if err := h.Service.Logout(r.Context(), c.Value); err != nil {
			h.logger().Error("logout", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: isHTTPS(r), MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *AuthHandler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		return
	}
	role := domain.RoleUser
	if p.Owner {
		role = domain.RoleOwner
	}
	httpx.WriteJSON(w, http.StatusOK, UserView{ID: p.UserID, Login: p.Login, Role: string(role)})
}

// isHTTPS decides the cookie Secure flag. Trusting X-Forwarded-Proto here is safe: a spoofed value can only
// make the cookie stricter (Secure over plain HTTP just means the browser will not send it).
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
