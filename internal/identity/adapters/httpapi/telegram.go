package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// TelegramSessionCookie holds sessions opened from the Mini App. It is separate from SessionCookie because the
// Telegram web client hosts the Mini App in a cross-site iframe: over HTTPS this cookie is SameSite=None; Secure;
// Partitioned, while the password session keeps SameSite=Strict. CSRF is still blocked by CrossOriginProtection.
const TelegramSessionCookie = "kd_tg_session"

// TelegramHandler serves the Mini App sign-in, linking/unlinking and the Owner's bot settings. Raw initData is
// accepted only in JSON bodies.
type TelegramHandler struct {
	Service *app.TelegramService
	Limiter *ratelimit.Limiter // public Mini App calls per client address
	Logger  *slog.Logger
}

// TelegramInitDataRequest is the body of the public Mini App endpoints.
type TelegramInitDataRequest struct {
	InitData string `json:"initData"`
}

// TelegramSettingsView is the Owner's view of the bot settings; the token is write-only.
type TelegramSettingsView struct {
	Configured   bool       `json:"configured"`
	BotUsername  string     `json:"botUsername"`
	AppShortName string     `json:"appShortName"`
	TokenSet     bool       `json:"tokenSet"`
	UpdatedAt    *time.Time `json:"updatedAt"`
}

// TelegramSettingsRequest is the body of PUT /api/telegram/settings; an empty botToken keeps the stored one.
type TelegramSettingsRequest struct {
	BotUsername  string `json:"botUsername"`
	AppShortName string `json:"appShortName"`
	BotToken     string `json:"botToken"`
}

// TelegramIdentityView is the linked Telegram account.
type TelegramIdentityView struct {
	TelegramID string    `json:"telegramId"` // string: JSON numbers lose precision past 2^53
	Username   string    `json:"username"`
	LinkedAt   time.Time `json:"linkedAt"`
}

// LinkRequestView is a link request as its web session sees it.
type LinkRequestView struct {
	State        string    `json:"state"`
	ExpiresAt    time.Time `json:"expiresAt"`
	TelegramName string    `json:"telegramName,omitempty"`
	URL          string    `json:"url,omitempty"` // only in the response that created it
}

// TelegramLinkStatusView is GET /api/telegram/link.
type TelegramLinkStatusView struct {
	Configured bool                  `json:"configured"`
	Identity   *TelegramIdentityView `json:"identity"`
	Request    *LinkRequestView      `json:"request"`
}

// CompleteLinkRequest is the body of POST /api/telegram/link/complete.
type CompleteLinkRequest struct {
	Code string `json:"code"`
}

// LinkPreviewView is what the Mini App shows for a link request.
type LinkPreviewView struct {
	Login     string    `json:"login"`
	ExpiresAt time.Time `json:"expiresAt"`
	Code      string    `json:"code,omitempty"`
}

// Register mounts the routes. Login, link preview and link confirm are public (see publicAPI).
func (h *TelegramHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/telegram/login", h.login)
	mux.HandleFunc("POST /api/telegram/link/preview", h.preview)
	mux.HandleFunc("POST /api/telegram/link/confirm", h.confirm)
	mux.HandleFunc("GET /api/telegram/link", h.status)
	mux.HandleFunc("POST /api/telegram/link", h.start)
	mux.HandleFunc("DELETE /api/telegram/link", h.unlink)
	mux.HandleFunc("POST /api/telegram/link/cancel", h.cancel)
	mux.HandleFunc("POST /api/telegram/link/complete", h.complete)
	mux.HandleFunc("GET /api/telegram/settings", h.settings)
	mux.HandleFunc("PUT /api/telegram/settings", h.saveSettings)
}

func (h *TelegramHandler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func (h *TelegramHandler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	switch {
	case writeTelegramError(w, err):
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrUnauthenticated):
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("telegram API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// writeTelegramError answers the Telegram-specific errors (also from invitation acceptance in the Mini App);
// false if err is none of them.
func writeTelegramError(w http.ResponseWriter, err error) bool {
	e := func(status int, code, msg string) { httpx.WriteError(w, status, code, msg) }
	switch {
	case errors.Is(err, domain.ErrTelegramNotConfigured):
		e(http.StatusServiceUnavailable, "telegram_not_configured", "Telegram is not configured")
	case errors.Is(err, domain.ErrInitDataInvalid):
		e(http.StatusUnauthorized, "telegram_auth_invalid", "invalid Telegram data")
	case errors.Is(err, domain.ErrInitDataExpired):
		e(http.StatusUnauthorized, "telegram_auth_expired", "Telegram data expired, reopen the app")
	case errors.Is(err, domain.ErrTelegramNotLinked):
		e(http.StatusUnauthorized, "telegram_not_linked", "this Telegram account is not linked")
	case errors.Is(err, domain.ErrInvalidCredentials):
		e(http.StatusForbidden, "account_disabled", "account disabled")
	case errors.Is(err, domain.ErrTelegramTaken):
		e(http.StatusConflict, "telegram_taken", "this Telegram account is linked to another user")
	case errors.Is(err, domain.ErrUserHasTelegram):
		e(http.StatusConflict, "telegram_already_linked", "a Telegram account is already linked")
	case errors.Is(err, domain.ErrLinkInvalid):
		e(http.StatusNotFound, "link_invalid", "invalid link request")
	case errors.Is(err, domain.ErrLinkExpired):
		e(http.StatusGone, "link_expired", "link request expired")
	case errors.Is(err, domain.ErrLinkUsed):
		e(http.StatusGone, "link_used", "link request already used")
	case errors.Is(err, domain.ErrLinkCancelled):
		e(http.StatusGone, "link_cancelled", "link request cancelled")
	case errors.Is(err, domain.ErrLinkClaimed):
		e(http.StatusConflict, "link_claimed", "link request was opened by another Telegram account")
	case errors.Is(err, domain.ErrLinkNotClaimed):
		e(http.StatusConflict, "link_not_confirmed", "confirm the link in Telegram first")
	case errors.Is(err, domain.ErrLinkCodeWrong):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "link_code_wrong", Message: "wrong code", Field: "code"})
	case errors.Is(err, domain.ErrLinkTooManyAttempts):
		e(http.StatusGone, "link_cancelled", "too many wrong codes, start again")
	default:
		return false
	}
	return true
}

func (h *TelegramHandler) limited(w http.ResponseWriter, r *http.Request) bool {
	client := httpx.ClientIP(r)
	if h.Limiter.Allow(client) {
		return false
	}
	h.logger().Warn("Telegram attempts rate limited", "client", client)
	rateLimited(w)
	return true
}

func (h *TelegramHandler) login(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var in TelegramInitDataRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	token, u, err := h.Service.Login(r.Context(), in.InitData, httpx.ClientIP(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	setTelegramCookie(w, r, token, int(domain.SessionMaxAge.Seconds()))
	httpx.WriteJSON(w, http.StatusOK, LoginResult{User: view(u)})
}

// setTelegramCookie writes the Mini App session cookie (see TelegramSessionCookie).
func setTelegramCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	c := &http.Cookie{Name: TelegramSessionCookie, Value: value, Path: "/", HttpOnly: true, MaxAge: maxAge,
		SameSite: http.SameSiteStrictMode}
	if isHTTPS(r) {
		c.Secure, c.SameSite, c.Partitioned = true, http.SameSiteNoneMode, true
	}
	http.SetCookie(w, c)
}

func (h *TelegramHandler) preview(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var in TelegramInitDataRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	pv, err := h.Service.PreviewLink(r.Context(), in.InitData)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, LinkPreviewView{Login: pv.Login, ExpiresAt: pv.ExpiresAt})
}

func (h *TelegramHandler) confirm(w http.ResponseWriter, r *http.Request) {
	if h.limited(w, r) {
		return
	}
	var in TelegramInitDataRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	code, pv, err := h.Service.ConfirmLink(r.Context(), in.InitData)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, LinkPreviewView{Login: pv.Login, ExpiresAt: pv.ExpiresAt, Code: code})
}

// sessionHash is the token hash of the session that authenticated r (nil if none).
func sessionHash(r *http.Request) []byte {
	h, _ := r.Context().Value(sessionKey{}).([]byte)
	return h
}

func (h *TelegramHandler) status(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	st, err := h.Service.Status(r.Context(), p, sessionHash(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := TelegramLinkStatusView{Configured: st.Configured}
	if st.Identity != nil {
		out.Identity = identityView(*st.Identity)
	}
	if st.Request != nil {
		out.Request = &LinkRequestView{State: string(st.State), ExpiresAt: st.Request.ExpiresAt}
		if st.Request.Telegram.ID != 0 {
			out.Request.TelegramName = st.Request.Telegram.DisplayName()
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func identityView(id domain.TelegramIdentity) *TelegramIdentityView {
	return &TelegramIdentityView{TelegramID: strconv.FormatInt(id.TelegramID, 10), Username: id.Username, LinkedAt: id.LinkedAt}
}

func (h *TelegramHandler) start(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	req, url, err := h.Service.StartLink(r.Context(), p, sessionHash(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, LinkRequestView{State: string(domain.LinkAwaitingTelegram), ExpiresAt: req.ExpiresAt, URL: url})
}

func (h *TelegramHandler) cancel(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.CancelLink(r.Context(), p); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *TelegramHandler) complete(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var in CompleteLinkRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	id, err := h.Service.CompleteLink(r.Context(), p, sessionHash(r), in.Code)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, identityView(id))
}

func (h *TelegramHandler) unlink(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.Unlink(r.Context(), p); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func settingsView(v app.TelegramSettingsView) TelegramSettingsView {
	out := TelegramSettingsView{Configured: v.Configured, BotUsername: v.BotUsername, AppShortName: v.AppShortName, TokenSet: v.TokenSet}
	if !v.UpdatedAt.IsZero() {
		out.UpdatedAt = &v.UpdatedAt
	}
	return out
}

func (h *TelegramHandler) settings(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.Settings(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, settingsView(v))
}

func (h *TelegramHandler) saveSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var in TelegramSettingsRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.Service.SaveSettings(r.Context(), p, in.BotUsername, in.AppShortName, in.BotToken)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, settingsView(v))
}
