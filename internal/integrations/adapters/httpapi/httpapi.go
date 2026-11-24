// Package httpapi exposes the integrations context to the Owner: the YouTrack connection and the GitHub App under
// /api/integrations and each project's YouTrack binding and repository registry under
// /api/projects/{id}/... It also receives GitHub webhooks (POST /api/webhooks/github, public, authenticated by
// their HMAC signature). Credentials are write-only: requests may carry them, responses only say whether one is set
// and when it changed. Every response is Cache-Control: no-store.
package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves the integrations API.
type Handler struct {
	Service *app.Service
	GitHub  *app.GitHubService // nil: the GitHub routes answer 404
	Logger  *slog.Logger

	slotsOnce sync.Once
	slots     chan struct{} // bounds webhook bodies held in memory at once
}

// webhookConcurrency is how many webhook bodies are read at once (memory bound: MaxWebhookBytes each); further
// deliveries wait for a slot.
const webhookConcurrency = 4

var signatureRE = regexp.MustCompile(`^sha256=[0-9a-fA-F]{64}$`)

// MaxWebhookBytes bounds a webhook body read into memory (GitHub caps deliveries at 25 MB; the platform's events
// are far smaller, a bigger push is refused with 413 and can be reconciled later).
const MaxWebhookBytes = 5 << 20

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

// GitHub is the installation's GitHub App.
type GitHub struct {
	AppID          int64      `json:"appId"`
	PrivateKey     SecretMeta `json:"privateKey"`
	WebhookSecret  SecretMeta `json:"webhookSecret"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	Check          Check      `json:"check"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt"`
	LastEvent      string     `json:"lastEvent,omitempty"`
}

// GitHubRequest is PUT /api/integrations/github; an empty privateKey or webhookSecret keeps the stored one.
type GitHubRequest struct {
	AppID         string `json:"appId"`
	PrivateKey    string `json:"privateKey"`    //nolint:gosec // G117: write-only input, never serialised in a response
	WebhookSecret string `json:"webhookSecret"` //nolint:gosec // G117: write-only input, never serialised in a response
}

// Integrations is GET /api/integrations; YouTrack and GitHub are null when not configured.
type Integrations struct {
	YouTrack *YouTrack `json:"youtrack"`
	GitHub   *GitHub   `json:"github"`
}

// YouTrackRequest is PUT /api/integrations/youtrack; an empty token keeps the stored one.
type YouTrackRequest struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"` //nolint:gosec // G117: write-only input, never serialised in a response
}

// Repository is a registered repository.
type Repository struct {
	ID                string     `json:"id"`
	Owner             string     `json:"owner"`
	Name              string     `json:"name"`
	FullName          string     `json:"fullName"`
	IntegrationBranch string     `json:"integrationBranch"`
	ProtectedBranches []string   `json:"protectedBranches"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	GitHub            RepoGitHub `json:"github"`
}

// RepoGitHub is what the App last read from GitHub about a repository.
type RepoGitHub struct {
	Installed     bool     `json:"installed"`
	DefaultBranch string   `json:"defaultBranch"`
	Protection    string   `json:"protection"`  // unknown | protected | unprotected
	Unprotected   []string `json:"unprotected"` // existing main/master without a ruleset or branch protection
	Sync          Check    `json:"sync"`
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
	mux.HandleFunc("PUT /api/integrations/github", h.putGitHub)
	mux.HandleFunc("DELETE /api/integrations/github", h.deleteGitHub)
	mux.HandleFunc("POST /api/integrations/github/check", h.checkGitHub)
	mux.HandleFunc("POST /api/projects/{id}/repositories/{repo}/sync", h.syncRepo)
	mux.HandleFunc("POST /api/webhooks/github", h.webhook)
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

func checkView(c domain.Check) Check {
	status := c.Status
	if status == "" {
		status = domain.CheckNone
	}
	return Check{Status: string(status), Code: c.Code, Detail: c.Detail, RequestedAt: c.RequestedAt, CheckedAt: c.CheckedAt}
}

func repoView(r domain.Repository) Repository {
	g := r.GitHub
	protection := g.Protection
	if protection == "" {
		protection = domain.ProtectionUnknown
	}
	unprotected := g.Unprotected
	if unprotected == nil {
		unprotected = []string{}
	}
	return Repository{ID: r.ID, Owner: r.Owner, Name: r.Name, FullName: r.FullName(), IntegrationBranch: r.IntegrationBranch,
		ProtectedBranches: r.ProtectedBranches(), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		GitHub: RepoGitHub{Installed: g.InstallationID != 0, DefaultBranch: g.DefaultBranch, Protection: string(protection),
			Unprotected: unprotected, Sync: checkView(g.Sync)}}
}

func githubView(v app.GitHubView) GitHub {
	meta := func(m domain.SecretMeta) SecretMeta {
		out := SecretMeta{Set: m.Set}
		if m.Set {
			at := m.UpdatedAt
			out.UpdatedAt = &at
		}
		return out
	}
	return GitHub{AppID: v.App.AppID, PrivateKey: meta(v.PrivateKey), WebhookSecret: meta(v.WebhookSecret),
		UpdatedAt: v.App.UpdatedAt, Check: checkView(v.App.Check), LastDeliveryAt: v.App.LastDeliveryAt, LastEvent: v.App.LastEvent}
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
	if h.GitHub != nil {
		gv, err := h.GitHub.GitHub(r.Context(), p)
		if err != nil {
			h.fail(w, err)
			return
		}
		if gv != nil {
			g := githubView(*gv)
			out.GitHub = &g
		}
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

// github returns the GitHub service, or answers 404 when it is not wired.
func (h *Handler) github(w http.ResponseWriter) (*app.GitHubService, bool) {
	if h.GitHub == nil {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return nil, false
	}
	return h.GitHub, true
}

func (h *Handler) putGitHub(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	gh, ok := h.github(w)
	if !ok {
		return
	}
	var req GitHubRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	v, err := gh.ConfigureGitHub(r.Context(), p, app.GitHubInput{AppID: req.AppID, PrivateKey: req.PrivateKey, WebhookSecret: req.WebhookSecret})
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, githubView(v))
}

func (h *Handler) deleteGitHub(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	gh, ok := h.github(w)
	if !ok {
		return
	}
	err := gh.DisconnectGitHub(r.Context(), p)
	if errors.Is(err, domain.ErrNotConfigured) {
		err = domain.ErrNotFound
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) checkGitHub(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	gh, ok := h.github(w)
	if !ok {
		return
	}
	v, err := gh.CheckGitHub(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, githubView(v))
}

func (h *Handler) syncRepo(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	gh, ok := h.github(w)
	if !ok {
		return
	}
	repo, err := gh.SyncRepository(r.Context(), p, r.PathValue("id"), r.PathValue("repo"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, repoView(repo))
}

// WebhookResponse acknowledges a delivery.
type WebhookResponse struct {
	Status string `json:"status"` // accepted | duplicate | ignored
}

// webhook receives a GitHub delivery. It needs no session: the HMAC signature over the body is the authentication,
// checked before anything else is done with the payload. 2xx is sent only once the event is durably stored.
func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	gh, ok := h.github(w)
	if !ok {
		return
	}
	// Unsigned requests are refused before the body is read or the secret decrypted.
	if !signatureRE.MatchString(r.Header.Get("X-Hub-Signature-256")) {
		httpx.WriteError(w, http.StatusUnauthorized, "bad_signature", "signature invalid")
		return
	}
	h.slotsOnce.Do(func() { h.slots = make(chan struct{}, webhookConcurrency) })
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-r.Context().Done():
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "too_large", "payload too large")
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return
	}
	out, err := gh.ReceiveWebhook(r.Context(), app.WebhookDelivery{ID: r.Header.Get("X-GitHub-Delivery"),
		Event: r.Header.Get("X-GitHub-Event"), Signature: r.Header.Get("X-Hub-Signature-256"), Body: body})
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrBadSignature), errors.Is(err, domain.ErrNotConfigured):
		// The same answer either way: an unauthenticated caller does not learn whether the App is configured.
		reason := "bad signature"
		if errors.Is(err, domain.ErrNotConfigured) {
			reason = "no webhook secret configured"
		}
		h.logger().Warn("github webhook rejected", "reason", reason, "client", httpx.ClientIP(r))
		httpx.WriteError(w, http.StatusUnauthorized, "bad_signature", "signature invalid")
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	case err != nil:
		h.logger().Error("github webhook", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	case out == app.WebhookAccepted:
		httpx.WriteJSON(w, http.StatusAccepted, WebhookResponse{Status: string(out)})
	default:
		httpx.WriteJSON(w, http.StatusOK, WebhookResponse{Status: string(out)})
	}
}
