// Package httpapi exposes the runners context: Owner management under /api/runners and the public, rate-limited
// enrollment endpoint a runner calls with its one-time token. Tokens travel only in JSON bodies, never in URLs.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// EnrollPath is the public enrollment endpoint; the identity middleware lets it through without a session.
const EnrollPath = protocol.EnrollPath

// Handler serves the runners API.
type Handler struct {
	Service *app.Service
	Limiter *ratelimit.Limiter // enrollment attempts per client address
	Logger  *slog.Logger
}

// TokenView is an enrollment token as listed for the Owner (never the token or its hash).
type TokenView struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	RunnerID  string    `json:"runnerId,omitempty"`
}

// CreateTokenResult carries the token, shown to the Owner once.
type CreateTokenResult struct {
	EnrollmentToken TokenView `json:"enrollmentToken"`
	Token           string    `json:"token"`
}

// ProjectRef is a project a runner may execute.
type ProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// RunnerView is a runner as the Owner sees it.
type RunnerView struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	Fingerprint string       `json:"fingerprint"`
	PublicKey   string       `json:"publicKey"`
	Hostname    string       `json:"hostname"`
	OS          string       `json:"os"`
	Arch        string       `json:"arch"`
	Version     string       `json:"version"`
	EnrolledAt  time.Time    `json:"enrolledAt"`
	ApprovedAt  *time.Time   `json:"approvedAt"`
	DisabledAt  *time.Time   `json:"disabledAt"`
	RevokedAt   *time.Time   `json:"revokedAt"`
	LastSeenAt  *time.Time   `json:"lastSeenAt"`
	Projects    []ProjectRef `json:"projects"`
}

// ProjectsRequest is the body of approve and PUT projects: the explicit, non-empty list of project ids.
type ProjectsRequest struct {
	ProjectIDs []string `json:"projectIds"`
}

// EnrollRequest is the body of POST /api/runners/enroll (shared wire message, see internal/protocol).
type EnrollRequest = protocol.EnrollRequest

// EnrollResult is the runner's registration: 201 when created, 200 for a retry of the same enrollment.
type EnrollResult = protocol.EnrollResult

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+EnrollPath, h.enroll)
	mux.HandleFunc("GET /api/runners/enrollment-tokens", h.listTokens)
	mux.HandleFunc("POST /api/runners/enrollment-tokens", h.createToken)
	mux.HandleFunc("POST /api/runners/enrollment-tokens/{id}/revoke", h.revokeToken)
	mux.HandleFunc("GET /api/runners", h.list)
	mux.HandleFunc("GET /api/runners/{id}", h.get)
	mux.HandleFunc("POST /api/runners/{id}/approve", h.withProjects(h.Service.Approve))
	mux.HandleFunc("PUT /api/runners/{id}/projects", h.withProjects(h.Service.SetProjects))
	mux.HandleFunc("POST /api/runners/{id}/disable", h.action(h.Service.Disable))
	mux.HandleFunc("POST /api/runners/{id}/enable", h.action(h.Service.Enable))
	mux.HandleFunc("POST /api/runners/{id}/revoke", h.action(h.Service.Revoke))
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
	gone := func(code, msg string) { httpx.WriteError(w, http.StatusGone, code, msg) }
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "runner or enrollment token not found")
	case errors.Is(err, domain.ErrInvalidTransition):
		httpx.WriteError(w, http.StatusConflict, "invalid_transition", "the runner's status does not allow this action")
	case errors.Is(err, domain.ErrKeyEnrolled):
		httpx.WriteError(w, http.StatusConflict, "key_enrolled", "this public key is already enrolled; generate a new key")
	case errors.Is(err, domain.ErrTokenInvalid):
		httpx.WriteError(w, http.StatusUnauthorized, "token_invalid", "invalid enrollment token")
	case errors.Is(err, domain.ErrTokenUsed):
		gone("token_used", "enrollment token already used")
	case errors.Is(err, domain.ErrTokenExpired):
		gone("token_expired", "enrollment token expired")
	case errors.Is(err, domain.ErrTokenRevoked):
		gone("token_revoked", "enrollment token revoked")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("runners API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func tokenView(v app.TokenView) TokenView {
	return TokenView{ID: v.Token.ID, State: string(v.State), CreatedAt: v.Token.CreatedAt, ExpiresAt: v.Token.ExpiresAt,
		RunnerID: v.Token.RunnerID}
}

func runnerView(v app.RunnerView) RunnerView {
	r := v.Runner
	out := RunnerView{ID: r.ID, Status: string(r.Status), Fingerprint: r.Fingerprint, PublicKey: r.PublicKey.String(),
		Hostname: r.Metadata.Hostname, OS: r.Metadata.OS, Arch: r.Metadata.Arch, Version: r.Metadata.Version,
		EnrolledAt: r.EnrolledAt, ApprovedAt: r.ApprovedAt, DisabledAt: r.DisabledAt, RevokedAt: r.RevokedAt,
		LastSeenAt: r.LastSeenAt, Projects: make([]ProjectRef, 0, len(v.Projects))}
	for _, p := range v.Projects {
		out.Projects = append(out.Projects, ProjectRef(p))
	}
	return out
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	tok, plain, err := h.Service.CreateToken(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, CreateTokenResult{
		EnrollmentToken: tokenView(app.TokenView{Token: tok, State: domain.TokenPending}), Token: plain})
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ts, err := h.Service.ListTokens(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]TokenView, 0, len(ts))
	for _, t := range ts {
		out = append(out, tokenView(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.RevokeToken(r.Context(), p, r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	vs, err := h.Service.List(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]RunnerView, 0, len(vs))
	for _, v := range vs {
		out = append(out, runnerView(v))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.Get(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, runnerView(v))
}

type (
	projectsOp func(ctx context.Context, actor auth.Principal, id string, projectIDs []string) (app.RunnerView, error)
	runnerOp   func(ctx context.Context, actor auth.Principal, id string) (app.RunnerView, error)
)

func (h *Handler) withProjects(op projectsOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principal(w, r)
		if !ok {
			return
		}
		if !p.Owner { // answer 403 before looking at the body
			h.fail(w, domain.ErrForbidden)
			return
		}
		var in ProjectsRequest
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		v, err := op(r.Context(), p, r.PathValue("id"), in.ProjectIDs)
		if err != nil {
			h.fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, runnerView(v))
	}
}

func (h *Handler) action(op runnerOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principal(w, r)
		if !ok {
			return
		}
		v, err := op(r.Context(), p, r.PathValue("id"))
		if err != nil {
			h.fail(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, runnerView(v))
	}
}

// enroll is public: the runner has no session, only the one-time token. Every attempt counts against the client
// address, so guessing tokens is bounded (and 256 random bits make it hopeless anyway).
func (h *Handler) enroll(w http.ResponseWriter, r *http.Request) {
	client := httpx.ClientIP(r)
	if !h.Limiter.Allow(client) {
		h.logger().Warn("runner enrollment rate limited", "client", client)
		w.Header().Set("Retry-After", "60")
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests, try again later")
		return
	}
	var in EnrollRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	res, err := h.Service.Enroll(r.Context(), app.EnrollRequest{Token: in.Token, PublicKey: in.PublicKey,
		Hostname: in.Hostname, OS: in.OS, Arch: in.Arch, Version: in.Version})
	if err != nil {
		h.fail(w, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, status, EnrollResult{RunnerID: res.Runner.ID, Status: string(res.Runner.Status), Fingerprint: res.Runner.Fingerprint})
}
