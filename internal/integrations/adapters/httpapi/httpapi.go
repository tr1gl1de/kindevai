// Package httpapi exposes the integrations context to the Owner: the YouTrack connection under
// /api/integrations and each project's YouTrack binding and repository registry under
// /api/projects/{id}/... Credentials are write-only: requests may carry them, responses only say whether one is set
// and when it changed. Every response is Cache-Control: no-store.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves the integrations API.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// SecretMeta is what the API tells about a credential.
type SecretMeta struct {
	Set       bool       `json:"set"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// Check is the last connection test.
type Check struct {
	Status      string     `json:"status"` // none | pending | ok | failed
	Code        string     `json:"code,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	RequestedAt *time.Time `json:"requestedAt"`
	CheckedAt   *time.Time `json:"checkedAt"`
}

// YouTrack is the installation's YouTrack connection.
type YouTrack struct {
	BaseURL   string     `json:"baseUrl"`
	Token     SecretMeta `json:"token"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Check     Check      `json:"check"`
}

// Integrations is GET /api/integrations; YouTrack is null when not configured.
type Integrations struct {
	YouTrack *YouTrack `json:"youtrack"`
}

// YouTrackRequest is PUT /api/integrations/youtrack; an empty token keeps the stored one.
type YouTrackRequest struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"` //nolint:gosec // G117: write-only input, never serialised in a response
}

// Repository is a registered repository.
type Repository struct {
	ID                string    `json:"id"`
	Owner             string    `json:"owner"`
	Name              string    `json:"name"`
	FullName          string    `json:"fullName"`
	IntegrationBranch string    `json:"integrationBranch"`
	ProtectedBranches []string  `json:"protectedBranches"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// ProjectIntegrations is a project's integration settings; YouTrackProject is "" when unbound.
type ProjectIntegrations struct {
	ProjectID       string       `json:"projectId"`
	YouTrackProject string       `json:"youtrackProject"`
	Repositories    []Repository `json:"repositories"`
}

// ShortNameRequest binds (or with "" unbinds) a YouTrack project.
type ShortNameRequest struct {
	ShortName string `json:"shortName"`
}

// RepositoryRequest registers a repository; an empty branch means "dev".
type RepositoryRequest struct {
	Owner             string `json:"owner"`
	Name              string `json:"name"`
	IntegrationBranch string `json:"integrationBranch"`
}

// BranchRequest changes a repository's integration branch.
type BranchRequest struct {
	IntegrationBranch string `json:"integrationBranch"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations", h.get)
	mux.HandleFunc("PUT /api/integrations/youtrack", h.putYouTrack)
	mux.HandleFunc("DELETE /api/integrations/youtrack", h.deleteYouTrack)
	mux.HandleFunc("POST /api/integrations/youtrack/check", h.checkYouTrack)
	mux.HandleFunc("GET /api/projects/{id}/integrations", h.project)
	mux.HandleFunc("PUT /api/projects/{id}/integrations/youtrack", h.bind)
	mux.HandleFunc("POST /api/projects/{id}/repositories", h.addRepo)
	mux.HandleFunc("PUT /api/projects/{id}/repositories/{repo}", h.updateRepo)
	mux.HandleFunc("DELETE /api/projects/{id}/repositories/{repo}", h.removeRepo)
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// principal returns the caller; the identity middleware already rejects anonymous calls, this is defence in depth.
func principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
	}
	return p, ok
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "project or repository not found")
	case errors.Is(err, domain.ErrNotConfigured):
		httpx.WriteError(w, http.StatusConflict, "not_configured", "the integration is not configured")
	case errors.Is(err, domain.ErrSecretNotSet):
		httpx.WriteError(w, http.StatusConflict, "not_configured", "the credential is not set")
	case errors.Is(err, domain.ErrRepositoryExists):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "repository_exists", Message: "repository already registered", Field: "name"})
	case errors.Is(err, domain.ErrShortNameTaken):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "short_name_taken", Message: "YouTrack project already bound", Field: "shortName"})
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		// Use-case errors never wrap credentials (they carry names and ids only).
		h.logger().Error("integrations API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func youtrackView(v app.YouTrackView) YouTrack {
	c := v.Connection
	out := YouTrack{BaseURL: c.BaseURL, UpdatedAt: c.UpdatedAt, Token: SecretMeta{Set: v.Token.Set},
		Check: Check{Status: string(c.Check.Status), Code: c.Check.Code, Detail: c.Check.Detail,
			RequestedAt: c.Check.RequestedAt, CheckedAt: c.Check.CheckedAt}}
	if v.Token.Set {
		at := v.Token.UpdatedAt
		out.Token.UpdatedAt = &at
	}
	return out
}

func repoView(r domain.Repository) Repository {
	return Repository{ID: r.ID, Owner: r.Owner, Name: r.Name, FullName: r.FullName(), IntegrationBranch: r.IntegrationBranch,
		ProtectedBranches: r.ProtectedBranches(), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func projectView(p app.ProjectIntegrations) ProjectIntegrations {
	out := ProjectIntegrations{ProjectID: p.Project.ID, YouTrackProject: p.YouTrackProject, Repositories: make([]Repository, 0, len(p.Repositories))}
	for _, r := range p.Repositories {
		out.Repositories = append(out.Repositories, repoView(r))
	}
	return out
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.YouTrack(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := Integrations{}
	if v != nil {
		yt := youtrackView(*v)
		out.YouTrack = &yt
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) putYouTrack(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req YouTrackRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	v, err := h.Service.ConfigureYouTrack(r.Context(), p, req.BaseURL, req.Token)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, youtrackView(v))
}

func (h *Handler) deleteYouTrack(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	err := h.Service.DisconnectYouTrack(r.Context(), p)
	if errors.Is(err, domain.ErrNotConfigured) {
		err = domain.ErrNotFound
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) checkYouTrack(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.CheckYouTrack(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, youtrackView(v))
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.Service.ProjectIntegrations(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectView(v))
}

func (h *Handler) bind(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req ShortNameRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	v, err := h.Service.BindYouTrackProject(r.Context(), p, r.PathValue("id"), req.ShortName)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, projectView(v))
}

func (h *Handler) addRepo(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req RepositoryRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	repo, err := h.Service.AddRepository(r.Context(), p, r.PathValue("id"),
		app.RepositoryInput{Owner: req.Owner, Name: req.Name, IntegrationBranch: req.IntegrationBranch})
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, repoView(repo))
}

func (h *Handler) updateRepo(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req BranchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	repo, err := h.Service.UpdateRepository(r.Context(), p, r.PathValue("id"), r.PathValue("repo"), req.IntegrationBranch)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, repoView(repo))
}

func (h *Handler) removeRepo(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.Service.RemoveRepository(r.Context(), p, r.PathValue("id"), r.PathValue("repo")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
