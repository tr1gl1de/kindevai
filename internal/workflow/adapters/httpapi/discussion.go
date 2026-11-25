package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The plan discussion API (KIN-43): the thread of a task, a new message (202: the AI answers asynchronously; poll
// the thread), sending a FAILED message again, and the limits (the Owner changes them). Texts are untrusted: the UI
// renders them as text. Error codes, limit codes and notice codes are worded by the UI in English or Russian.

// MessageView is one discussion message.
type MessageView struct {
	ID           string     `json:"id"`
	Role         string     `json:"role"` // user | assistant | system
	Author       string     `json:"author"`
	PlanRevision int        `json:"planRevision"`
	Text         string     `json:"text"`
	Status       string     `json:"status"` // PENDING | DELIVERED | ANSWERED | FAILED
	Reason       string     `json:"reason,omitempty"`
	Notice       string     `json:"notice,omitempty"`   // system: revision_created | plan_rejected | plan_outdated | plan_not_applied
	Revision     int        `json:"revision,omitempty"` // revision_created: the new revision
	ReplyTo      string     `json:"replyTo,omitempty"`
	Agent        string     `json:"agent,omitempty"`
	Model        string     `json:"model,omitempty"`
	Usage        *UsageView `json:"usage,omitempty"`
	WorkMs       int64      `json:"workMs,omitempty"`
	DeliveredAt  *time.Time `json:"deliveredAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// LimitsView is the discussion limits.
type LimitsView struct {
	MaxMessageChars    int   `json:"maxMessageChars"`
	MaxMessagesPerPlan int   `json:"maxMessagesPerPlan"`
	MaxMessagesPerHour int   `json:"maxMessagesPerHour"`
	AnswerTimeoutSec   int64 `json:"answerTimeoutSec"`
}

// ThreadView is GET /api/tasks/{id}/discussion.
type ThreadView struct {
	Messages   []MessageView `json:"messages"`
	Limits     LimitsView    `json:"limits"`
	CanWrite   bool          `json:"canWrite"`
	InFlight   bool          `json:"inFlight"`
	Used       int           `json:"used"`
	HasSession bool          `json:"hasSession"`
}

// PostMessageRequest is POST /api/tasks/{id}/discussion/messages.
type PostMessageRequest struct {
	Text string `json:"text"`
}

// LimitErrorBody is a 429: code is the limit (message_too_long | answer_in_flight | plan_message_limit |
// hourly_message_limit), limit its value, retryAfterSec when the next message is allowed (hourly).
type LimitErrorBody struct {
	Error         string `json:"error"` // rate_limited
	Message       string `json:"message"`
	Code          string `json:"code"`
	Limit         int    `json:"limit,omitempty"`
	RetryAfterSec int64  `json:"retryAfterSec,omitempty"`
}

func messageView(m domain.DiscussionMessage) MessageView {
	return MessageView{ID: m.ID, Role: string(m.Role), Author: m.Author, PlanRevision: m.PlanRevision, Text: m.Text,
		Status: string(m.Status), Reason: m.Reason, Notice: m.Notice, Revision: m.Revision, ReplyTo: m.ReplyTo, Agent: m.Agent,
		Model: m.Model, Usage: usageView(m.Usage), WorkMs: m.Work.Milliseconds(), DeliveredAt: m.DeliveredAt,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
}

func limitsView(l domain.DiscussionLimits) LimitsView {
	return LimitsView{MaxMessageChars: l.MaxMessageChars, MaxMessagesPerPlan: l.MaxMessagesPerPlan,
		MaxMessagesPerHour: l.MaxMessagesPerHour, AnswerTimeoutSec: int64(l.AnswerTimeout / time.Second)}
}

func (h *Handler) registerDiscussion(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks/{id}/discussion", h.thread)
	mux.HandleFunc("POST /api/tasks/{id}/discussion/messages", h.postMessage)
	mux.HandleFunc("POST /api/tasks/{id}/discussion/messages/{msg}/retry", h.retryMessage)
	mux.HandleFunc("GET /api/plan-discussion/limits", h.limits)
	mux.HandleFunc("PUT /api/plan-discussion/limits", h.setLimits)
}

// discussionFail maps the discussion's own errors, then the task API's.
func (h *Handler) discussionFail(w http.ResponseWriter, err error) {
	var le *domain.LimitError
	switch {
	case errors.As(err, &le):
		if le.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(le.RetryAfter/time.Second), 10))
		}
		httpx.WriteJSON(w, http.StatusTooManyRequests, LimitErrorBody{Error: "rate_limited", Message: le.Error(), Code: le.Code,
			Limit: le.Limit, RetryAfterSec: int64(le.RetryAfter / time.Second)})
	case errors.Is(err, app.ErrDiscussionUnavailable):
		httpx.WriteError(w, http.StatusServiceUnavailable, "discussion_unavailable", "the plan discussion is not configured")
	case errors.Is(err, domain.ErrInvalidTransition):
		httpx.WriteError(w, http.StatusConflict, "invalid_state", "the task has no plan that may change now")
	default:
		h.fail(w, err)
	}
}

func (h *Handler) thread(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	th, err := h.Service.PlanDiscussion(r.Context(), p, r.PathValue("id"))
	if err != nil {
		h.discussionFail(w, err)
		return
	}
	out := ThreadView{Messages: make([]MessageView, 0, len(th.Messages)), Limits: limitsView(th.Limits), CanWrite: th.CanWrite,
		InFlight: th.InFlight, Used: th.Used, HasSession: th.Session}
	for _, m := range th.Messages {
		out.Messages = append(out.Messages, messageView(m))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req PostMessageRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	m, err := h.Service.PostMessage(r.Context(), p, r.PathValue("id"), req.Text)
	if err != nil {
		h.discussionFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, messageView(m))
}

func (h *Handler) retryMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	m, err := h.Service.RetryMessage(r.Context(), p, r.PathValue("id"), r.PathValue("msg"))
	if err != nil {
		h.discussionFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, messageView(m))
}

func (h *Handler) limits(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal(w, r); !ok {
		return
	}
	l, err := h.Service.DiscussionLimits(r.Context())
	if err != nil {
		h.discussionFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, limitsView(l))
}

func (h *Handler) setLimits(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var req LimitsView
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.AnswerTimeoutSec < 0 || req.AnswerTimeoutSec > 24*3600 {
		req.AnswerTimeoutSec = 0 // out of range (and no overflow): refused by the validation
	}
	l, err := h.Service.SetDiscussionLimits(r.Context(), p, domain.DiscussionLimits{MaxMessageChars: req.MaxMessageChars,
		MaxMessagesPerPlan: req.MaxMessagesPerPlan, MaxMessagesPerHour: req.MaxMessagesPerHour,
		AnswerTimeout: time.Duration(req.AnswerTimeoutSec) * time.Second})
	if err != nil {
		h.discussionFail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, limitsView(l))
}
