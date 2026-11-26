// Package httpapi serves the Owner's read-only audit view. There are no routes that change or delete events.
package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/audit/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
)

// Handler serves GET /api/audit and GET /api/audit/kinds.
type Handler struct {
	Service *app.Service
	Logger  *slog.Logger
}

// ActorView is who acted.
type ActorView struct {
	Type  string `json:"type"` // user | system | runner
	ID    string `json:"id"`
	Label string `json:"label"`
}

// EventView is one audit event.
type EventView struct {
	ID        int64             `json:"id"`
	At        time.Time         `json:"at"`
	Actor     ActorView         `json:"actor"`
	Kind      string            `json:"kind"`
	ProjectID string            `json:"projectId,omitempty"`
	Subject   string            `json:"subject,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

// PageView is the body of GET /api/audit: events newest first; Next is the "before" cursor of the next page,
// null on the last page (a string, so clients never round large ids).
type PageView struct {
	Events []EventView `json:"events"`
	Next   *string     `json:"next"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/audit", h.list)
	mux.HandleFunc("GET /api/audit/kinds", h.kinds)
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrForbidden):
		httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, domain.ErrInvalidFilter):
		httpx.WriteError(w, http.StatusBadRequest, "invalid_filter", err.Error())
	default:
		h.logger().Error("audit API", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
	}
	return p, ok
}

func badFilter(field string) error { return errors.Join(domain.ErrInvalidFilter, errors.New(field)) }

// parseFilter reads actor, project, kind, from, to (RFC 3339), before and limit.
func parseFilter(r *http.Request) (domain.Filter, error) {
	q := r.URL.Query()
	f := domain.Filter{Actor: q.Get("actor"), ProjectID: q.Get("project"), Kind: q.Get("kind")}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return domain.Filter{}, badFilter(p.name + " must be RFC 3339")
			}
			*p.dst = t
		}
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return domain.Filter{}, badFilter("before")
		}
		f.Before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return domain.Filter{}, badFilter("limit")
		}
		f.Limit = n
	}
	return f, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if !p.Owner { // before parsing, so a non-Owner learns nothing from validation errors
		h.fail(w, domain.ErrForbidden)
		return
	}
	f, err := parseFilter(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	page, err := h.Service.Query(r.Context(), p, f)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := PageView{Events: make([]EventView, 0, len(page.Events))}
	for _, e := range page.Events {
		out.Events = append(out.Events, EventView{
			ID: e.ID, At: e.At, Kind: string(e.Kind), ProjectID: e.ProjectID, Subject: e.Subject, Details: e.Details,
			Actor: ActorView{Type: string(e.Actor.Type), ID: e.Actor.ID, Label: e.Actor.Label},
		})
	}
	if page.Next > 0 {
		next := strconv.FormatInt(page.Next, 10)
		out.Next = &next
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) kinds(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	k, err := h.Service.Kinds(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, k)
}
