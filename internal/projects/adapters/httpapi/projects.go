// Package httpapi exposes the projects context over the JSON API.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/projects/app"
	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

// Handler serves /api/projects.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// ProjectView is the JSON shape of a project.
type ProjectView struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	MyRole    string    `json:"myRole"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreateProjectRequest is the body of POST /api/projects.
type CreateProjectRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// MemberView is the JSON shape of a membership.
type MemberView struct {
	UserID string `json:"userId"`
	Login  string `json:"login"`
	Role   string `json:"role"`
}

// SetMemberRequest is the body of PUT /api/projects/{id}/members/{login}.
type SetMemberRequest struct {
	Role string `json:"role"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", h.list)
	mux.HandleFunc("POST /api/projects", h.create)
	mux.HandleFunc("GET /api/projects/{id}", h.get)
	mux.HandleFunc("GET /api/projects/{id}/members", h.members)
	mux.HandleFunc("PUT /api/projects/{id}/members/{login}", h.setMember)
	mux.HandleFunc("DELETE /api/projects/{id}/members/{login}", h.removeMember)
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// actor returns the caller; the identity middleware already rejects anonymous calls, this is defence in depth.
func actor(w http.ResponseWriter, r *http.Request) (domain.Actor, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		return domain.Actor{}, false
	}
	return domain.Actor{UserID: p.UserID, Owner: p.Owner}, true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "project not found")
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrSlugTaken):
		httpx.WriteError(w, http.StatusConflict, "slug_taken", "project slug is already taken")
	case errors.Is(err, domain.ErrNotMember):
		httpx.WriteError(w, http.StatusNotFound, "membership_not_found", "user is not a member of this project")
	case errors.Is(err, domain.ErrUserNotFound):
		httpx.WriteError(w, http.StatusNotFound, "user_not_found", "user not found")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("projects API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func projectView(v app.ProjectView) ProjectView {
	return ProjectView{ID: v.Project.ID, Slug: string(v.Project.Slug), Name: string(v.Project.Name),
		MyRole: v.MyRole, CreatedAt: v.Project.CreatedAt}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	a, ok := actor(w, r)
	if !ok {
		return
	}
	vs, err := h.Service.List(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]ProjectView, 0, len(vs))
	for _, v := range vs {
		out = append(out, projectView(v))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	a, ok := actor(w, r)
	if !ok {
		return
	}
	var in CreateProjectRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	p, err := h.Service.Create(r.Context(), a, in.Name, in.Slug)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, projectView(app.ProjectView{Project: p, MyRole: "owner"}))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	a, ok := actor(w, r)
	if !ok {
		return
	}
	v, err := h.Service.Get(r.Context(), a, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectView(v))
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	a, ok := actor(w, r)
	if !ok {
		return
	}
	ms, err := h.Service.Members(r.Context(), a, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		out = append(out, MemberView(m))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) setMember(w http.ResponseWriter, r *http.Request) {
	a, ok := actor(w, r)
	if !ok {
		return
	}
	var in SetMemberRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	m, err := h.Service.SetMember(r.Context(), a, r.PathValue("id"), r.PathValue("login"), in.Role)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, MemberView(m))
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	a, ok := actor(w, r)
	if !ok {
		return
	}
	if err := h.Service.RemoveMember(r.Context(), a, r.PathValue("id"), r.PathValue("login")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
