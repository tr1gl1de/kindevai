// Package httpapi exposes the network context under /api/network (Owner only): profiles, assignments, the
// effective profile and Test Network. Proxy passwords are accepted in request bodies and never returned.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/network/app"
	"github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves the network API.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
	Now     func() time.Time
}

// ProxyJSON is a proxy without its password; URL is the normalized form.
type ProxyJSON struct {
	Scheme   string `json:"scheme"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	URL      string `json:"url,omitempty"`
}

// ScopeJSON is an assignment scope.
type ScopeJSON struct {
	Level  string `json:"level"`
	Target string `json:"target"`
}

// ProfileJSON is a profile as the Owner sees it.
type ProfileJSON struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Proxy         *ProxyJSON    `json:"proxy"`
	PasswordSet   bool          `json:"passwordSet"`
	Enforcement   string        `json:"enforcement"`
	DNS           string        `json:"dns"`
	FailClosed    bool          `json:"failClosed"`
	ExpectedIPv4  string        `json:"expectedIpv4"`
	ExpectedIPv6  string        `json:"expectedIpv6"`
	Allow         []domain.Rule `json:"allow"`
	DiagnosticURL string        `json:"diagnosticUrl"`
	Assigned      []ScopeJSON   `json:"assigned"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
}

// ProfileRequest is the body of POST and PUT /api/network/profiles. Either proxyUrl or proxy (fields) may be
// given; password nil keeps the stored one.
type ProfileRequest struct {
	Name          string             `json:"name"`
	ProxyURL      string             `json:"proxyUrl"`
	Proxy         *ProxyJSON         `json:"proxy"`
	Password      *string            `json:"password"`
	ClearPassword bool               `json:"clearPassword"`
	Enforcement   string             `json:"enforcement"`
	DNS           string             `json:"dns"`
	FailClosed    bool               `json:"failClosed"`
	ExpectedIPv4  string             `json:"expectedIpv4"`
	ExpectedIPv6  string             `json:"expectedIpv6"`
	Allow         []domain.RuleInput `json:"allow"`
	DiagnosticURL string             `json:"diagnosticUrl"`
}

// AssignmentJSON is an assignment; PUT with an empty profileId removes it.
type AssignmentJSON struct {
	Level     string     `json:"level"`
	Target    string     `json:"target"`
	ProfileID string     `json:"profileId"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// EffectiveJSON is the resolved profile for a runner/project/role.
type EffectiveJSON struct {
	Proxy          *ProxyJSON    `json:"proxy"`
	ProxyProfileID string        `json:"proxyProfileId"`
	Enforcement    string        `json:"enforcement"`
	DNS            string        `json:"dns"`
	FailClosed     bool          `json:"failClosed"`
	ExpectedIPv4   string        `json:"expectedIpv4"`
	ExpectedIPv6   string        `json:"expectedIpv6"`
	Restricted     bool          `json:"restricted"`
	Allow          []domain.Rule `json:"allow"`
	DiagnosticURL  string        `json:"diagnosticUrl"`
	StrictFrom     string        `json:"strictFrom"`
	Sources        []string      `json:"sources"`
}

// TestRequest is the body of POST /api/network/tests.
type TestRequest struct {
	RunnerID  string `json:"runnerId"`
	ProfileID string `json:"profileId"`
}

// CheckJSON is one endpoint check.
type CheckJSON struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	OK        bool   `json:"ok"`
	Code      string `json:"code"`
	LatencyMs int64  `json:"latencyMs"`
}

// TestJSON is a Test Network run.
type TestJSON struct {
	ID           string      `json:"id"`
	RunnerID     string      `json:"runnerId"`
	ProfileID    string      `json:"profileId"`
	Status       string      `json:"status"`
	Code         string      `json:"code"`
	Detail       string      `json:"detail"`
	ExpectedIPv4 string      `json:"expectedIpv4"`
	ExpectedIPv6 string      `json:"expectedIpv6"`
	ObservedIPv4 string      `json:"observedIpv4"`
	ObservedIPv6 string      `json:"observedIpv6"`
	Checks       []CheckJSON `json:"checks"`
	DurationMs   int64       `json:"durationMs"`
	RequestedAt  time.Time   `json:"requestedAt"`
	CompletedAt  *time.Time  `json:"completedAt"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/network/profiles", h.listProfiles)
	mux.HandleFunc("POST /api/network/profiles", h.createProfile)
	mux.HandleFunc("PUT /api/network/profiles/{id}", h.updateProfile)
	mux.HandleFunc("DELETE /api/network/profiles/{id}", h.deleteProfile)
	mux.HandleFunc("GET /api/network/assignments", h.listAssignments)
	mux.HandleFunc("PUT /api/network/assignments", h.assign)
	mux.HandleFunc("GET /api/network/effective", h.effective)
	mux.HandleFunc("GET /api/network/tests", h.listTests)
	mux.HandleFunc("POST /api/network/tests", h.requestTest)
	mux.HandleFunc("GET /api/network/tests/{id}", h.getTest)
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func (h *Handler) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

// owner returns the caller if it is the Owner (403 before the body is read otherwise).
func (h *Handler) owner(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		return p, false
	}
	if !p.Owner {
		h.fail(w, domain.ErrForbidden)
		return p, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return p, true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	var ve *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "profile, runner, project or test not found")
	case errors.Is(err, domain.ErrInUse):
		httpx.WriteError(w, http.StatusConflict, "in_use", "the profile is assigned; remove its assignments first")
	case errors.Is(err, domain.ErrNameTaken):
		httpx.WriteJSON(w, http.StatusConflict, httpx.ErrorBody{Error: "name_taken", Message: "a profile with this name exists", Field: "name"})
	case errors.As(err, &ve):
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, httpx.ErrorBody{Error: "validation", Message: ve.Code, Field: ve.Field})
	default:
		h.logger().Error("network API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func proxyJSON(p *domain.Proxy) *ProxyJSON {
	if p == nil {
		return nil
	}
	return &ProxyJSON{Scheme: string(p.Scheme), Host: p.Host, Port: p.Port, Username: p.Username, URL: p.URL()}
}

func rules(rs []domain.Rule) []domain.Rule {
	if rs == nil {
		return []domain.Rule{}
	}
	return rs
}

func profileJSON(v app.ProfileView) ProfileJSON {
	p := v.Profile
	out := ProfileJSON{ID: p.ID, Name: p.Name, Proxy: proxyJSON(p.Proxy), PasswordSet: v.PasswordSet,
		Enforcement: string(p.Enforcement), DNS: string(p.DNS), FailClosed: p.FailClosed, ExpectedIPv4: p.ExpectedIPv4,
		ExpectedIPv6: p.ExpectedIPv6, Allow: rules(p.Allow), DiagnosticURL: p.DiagnosticURL, Assigned: []ScopeJSON{},
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	for _, s := range v.Assigned {
		out.Assigned = append(out.Assigned, ScopeJSON{Level: string(s.Level), Target: s.Target})
	}
	return out
}

func (in ProfileRequest) toApp() app.ProfileRequest {
	r := app.ProfileRequest{Name: in.Name, ProxyURL: in.ProxyURL, Password: in.Password, ClearPassword: in.ClearPassword,
		Enforcement: in.Enforcement, DNS: in.DNS, FailClosed: in.FailClosed, ExpectedIPv4: in.ExpectedIPv4,
		ExpectedIPv6: in.ExpectedIPv6, Allow: in.Allow, DiagnosticURL: in.DiagnosticURL}
	if in.Proxy != nil {
		r.ProxyFields = &app.ProxyFields{Scheme: in.Proxy.Scheme, Host: in.Proxy.Host, Port: in.Proxy.Port, Username: in.Proxy.Username}
	}
	return r
}

func (h *Handler) listProfiles(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	vs, err := h.Service.Profiles(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]ProfileJSON, 0, len(vs))
	for _, v := range vs {
		out = append(out, profileJSON(v))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) createProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	var in ProfileRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.Service.CreateProfile(r.Context(), p, in.toApp())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, profileJSON(v))
}

func (h *Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	var in ProfileRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.Service.UpdateProfile(r.Context(), p, r.PathValue("id"), in.toApp())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, profileJSON(v))
}

func (h *Handler) deleteProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	if err := h.Service.DeleteProfile(r.Context(), p, r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listAssignments(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	as, err := h.Service.Assignments(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]AssignmentJSON, 0, len(as))
	for _, a := range as {
		at := a.UpdatedAt
		out = append(out, AssignmentJSON{Level: string(a.Scope.Level), Target: a.Scope.Target, ProfileID: a.ProfileID, UpdatedAt: &at})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) assign(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	var in AssignmentJSON
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.Service.Assign(r.Context(), p, in.Level, in.Target, in.ProfileID); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) effective(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	e, err := h.Service.Effective(r.Context(), p, q.Get("runnerId"), q.Get("projectId"), q.Get("role"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := EffectiveJSON{Proxy: proxyJSON(e.Proxy), ProxyProfileID: e.ProxyProfileID, Enforcement: string(e.Enforcement),
		DNS: string(e.DNS), FailClosed: e.FailClosed, ExpectedIPv4: e.ExpectedIPv4, ExpectedIPv6: e.ExpectedIPv6,
		Restricted: e.Restricted, Allow: rules(e.Allow), DiagnosticURL: e.DiagnosticURL, StrictFrom: string(e.StrictFrom),
		Sources: []string{}}
	for _, s := range e.Sources {
		out.Sources = append(out.Sources, string(s))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) testJSON(t domain.Test) TestJSON {
	out := TestJSON{ID: t.ID, RunnerID: t.RunnerID, ProfileID: t.ProfileID, Status: string(t.StatusAt(h.now())),
		Code: t.Code, Detail: t.Detail, ExpectedIPv4: t.Expected.IPv4, ExpectedIPv6: t.Expected.IPv6,
		ObservedIPv4: t.ObservedIPv4, ObservedIPv6: t.ObservedIPv6, Checks: []CheckJSON{}, DurationMs: t.DurationMs,
		RequestedAt: t.RequestedAt, CompletedAt: t.CompletedAt}
	for _, c := range t.Checks {
		out.Checks = append(out.Checks, CheckJSON(c))
	}
	return out
}

func (h *Handler) requestTest(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	var in TestRequest
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	t, err := h.Service.RequestTest(r.Context(), p, in.RunnerID, in.ProfileID)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, h.testJSON(t))
}

func (h *Handler) listTests(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ts, err := h.Service.Tests(r.Context(), p, r.URL.Query().Get("runnerId"), limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]TestJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, h.testJSON(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) getTest(w http.ResponseWriter, r *http.Request) {
	p, ok := h.owner(w, r)
	if !ok {
		return
	}
	t, err := h.Service.Test(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.testJSON(t))
}
