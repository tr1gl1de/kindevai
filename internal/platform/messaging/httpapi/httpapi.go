// Package httpapi exposes the durable inbox/outbox to the platform Owner: integration state per adapter, dead
// messages and their retry. The handlers only read and update SQLite and in-memory health; they never call an
// external system, so an outage cannot block them.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// Handler serves /api/integrations/status and /api/messaging/{outbox|inbox}/dead.
type Handler struct {
	Outbox *messaging.Outbox
	Inbox  *messaging.Inbox
	Logger *slog.Logger
}

// AdapterView is the state of one external system as seen by outgoing deliveries (and pollers).
type AdapterView struct {
	Adapter             string     `json:"adapter"`
	State               string     `json:"state"` // ok | degraded | circuit_open
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	LastError           string     `json:"lastError,omitempty"`
	LastFailureAt       *time.Time `json:"lastFailureAt,omitempty"`
	OpenUntil           *time.Time `json:"openUntil,omitempty"`
	Pending             int        `json:"pending"`
	Dead                int        `json:"dead"`
}

// SourceView counts unfinished inbound events of one sender.
type SourceView struct {
	Source  string `json:"source"`
	Pending int    `json:"pending"`
	Dead    int    `json:"dead"`
}

// StatusView is the body of GET /api/integrations/status.
type StatusView struct {
	Adapters []AdapterView `json:"adapters"`
	Inbox    []SourceView  `json:"inbox"`
}

// DeadView is a dead message without its payload.
type DeadView struct {
	ID        int64     `json:"id"`
	Endpoint  string    `json:"endpoint"` // adapter (outbox) or source (inbox)
	Topic     string    `json:"topic"`
	Key       string    `json:"key"` // idempotency key (outbox) or delivery id (inbox)
	Attempts  int       `json:"attempts"`
	LastError string    `json:"lastError"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/status", h.owner(h.status))
	mux.HandleFunc("GET /api/messaging/{box}/dead", h.owner(h.dead))
	mux.HandleFunc("POST /api/messaging/{box}/dead/{id}/retry", h.owner(h.retry))
}

func (h *Handler) logger() *slog.Logger {
	if h.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Logger
}

// owner admits only the platform Owner: error texts and keys describe integrations of every project.
func (h *Handler) owner(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		switch {
		case !ok:
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "login required")
		case !p.Owner:
			httpx.WriteError(w, http.StatusForbidden, "forbidden", "not allowed")
		default:
			next(w, r)
		}
	}
}

func (h *Handler) internal(w http.ResponseWriter, err error) {
	h.logger().Error("messaging API", "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	out, err := h.statusView(r.Context())
	if err != nil {
		h.internal(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) statusView(ctx context.Context) (StatusView, error) {
	counts, err := h.Outbox.Stats(ctx)
	if err != nil {
		return StatusView{}, err
	}
	out := StatusView{Adapters: []AdapterView{}, Inbox: []SourceView{}}
	seen := map[string]bool{}
	for _, s := range h.Outbox.Health().Snapshot() {
		seen[s.Adapter] = true
		c := counts[s.Adapter]
		out.Adapters = append(out.Adapters, AdapterView{Adapter: s.Adapter, State: string(s.State),
			ConsecutiveFailures: s.ConsecutiveFailures, LastError: s.LastError, LastFailureAt: s.LastFailureAt,
			OpenUntil: s.OpenUntil, Pending: c.Pending, Dead: c.Dead})
	}
	for name, c := range counts { // messages of adapters without a handler in this process
		if !seen[name] {
			out.Adapters = append(out.Adapters, AdapterView{Adapter: name, State: string(messaging.StateOK), Pending: c.Pending, Dead: c.Dead})
		}
	}
	slices.SortFunc(out.Adapters, func(a, b AdapterView) int { return strings.Compare(a.Adapter, b.Adapter) })
	in, err := h.Inbox.Stats(ctx)
	if err != nil {
		return StatusView{}, err
	}
	for name, c := range in {
		out.Inbox = append(out.Inbox, SourceView{Source: name, Pending: c.Pending, Dead: c.Dead})
	}
	slices.SortFunc(out.Inbox, func(a, b SourceView) int { return strings.Compare(a.Source, b.Source) })
	return out, nil
}

// box is the common part of the outbox and the inbox used here.
type box interface {
	Dead(ctx context.Context, limit int) ([]messaging.DeadMessage, error)
	Retry(ctx context.Context, id int64) error
}

func (h *Handler) box(w http.ResponseWriter, r *http.Request) (box, bool) {
	switch r.PathValue("box") {
	case "outbox":
		return h.Outbox, true
	case "inbox":
		return h.Inbox, true
	}
	httpx.WriteError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
	return nil, false
}

const maxDeadLimit = 200

func (h *Handler) dead(w http.ResponseWriter, r *http.Request) {
	b, ok := h.box(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxDeadLimit {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1..200")
			return
		}
		limit = n
	}
	ms, err := b.Dead(r.Context(), limit)
	if err != nil {
		h.internal(w, err)
		return
	}
	out := make([]DeadView, 0, len(ms))
	for _, m := range ms {
		out = append(out, DeadView(m))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	b, ok := h.box(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "message not found")
		return
	}
	switch err := b.Retry(r.Context(), id); {
	case errors.Is(err, messaging.ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, "not_found", "message not found")
	case err != nil:
		h.internal(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
