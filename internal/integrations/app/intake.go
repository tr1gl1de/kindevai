package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// YouTrack intake (KIN-27). Polling is the primary channel: PollOnce reads the issues of every bound YouTrack
// project updated since the binding's cursor and hands each one to the inbox. The optional webhook only names an
// issue; its state is always re-read through the REST API (outbox), so a webhook payload never decides anything.
// "Run by issue ID" works the same way on behalf of a project member. HandleIssue (inbox, one transaction, no
// external calls) turns a snapshot into a task when the initiator may start AI in the project: the member who ran
// it by ID, or the YouTrack user who applied the trigger tag as read from the issue's tag history before the inbox
// (KIN-50).

// Inbox/outbox topics of the intake.
const (
	SourceYouTrack       = "youtrack"
	TopicYouTrackIssue   = "youtrack.issue"       // inbox: an authoritative issue snapshot (poller or fetch)
	TopicYouTrackWebhook = "youtrack.webhook"     // inbox: an unverified hint that an issue changed
	TopicYouTrackFetch   = "youtrack.fetch_issue" // outbox: read an issue through the REST API
)

// Audit event kinds of the intake.
const (
	KindYouTrackTaskCreated    = "integration.youtrack_task_created"
	KindYouTrackIssueRejected  = "integration.youtrack_issue_rejected"
	KindYouTrackRunRequested   = "integration.youtrack_run_requested"
	KindYouTrackIntakeChanged  = "integration.youtrack_intake_changed"
	KindYouTrackWebhookChanged = "integration.youtrack_webhook_changed"
	KindYouTrackTriggerChanged = "integration.youtrack_trigger_changed"
	KindYouTrackUserLinked     = "integration.youtrack_user_linked"
	KindYouTrackUserUnlinked   = "integration.youtrack_user_unlinked"
	// KIN-85 automatic planning of intake tasks.
	KindYouTrackAutoPlanChanged = "integration.youtrack_autoplan_changed"
	KindYouTrackPlanningStarted = "integration.youtrack_planning_started"
	KindYouTrackPlanningSkipped = "integration.youtrack_planning_skipped"
	// KIN-38 write-back: the project's task state → YouTrack state map.
	KindYouTrackStatesChanged = "integration.youtrack_states_changed"
)

const (
	defaultPageSize        = 50
	defaultMaxPages        = 10
	maxBackoff             = 15 * time.Minute
	recentIntakeLimit      = 20
	maxWebhookIssueIDBytes = 64
	youtrackIntakeSubject  = "integration:youtrack"
)

// IntakeStore keeps bindings, cursors, settings, login links and intake decisions. Every method runs on the
// caller's transaction (db.Conn).
type IntakeStore interface {
	Bindings(ctx context.Context) ([]domain.Binding, error)
	Binding(ctx context.Context, projectID string) (domain.Binding, error)            // domain.ErrNotFound
	BindingByShortName(ctx context.Context, shortName string) (domain.Binding, error) // domain.ErrNotFound
	SetTriggerTag(ctx context.Context, projectID, tag string, at time.Time) error     // domain.ErrNotFound
	SetAutoPlan(ctx context.Context, projectID string, on bool, at time.Time) error   // domain.ErrNotFound
	AdvanceCursor(ctx context.Context, projectID, shortName string, cursor int64, at time.Time) error

	IntakeSettings(ctx context.Context) (domain.IntakeSettings, error)
	SaveIntakeSettings(ctx context.Context, s domain.IntakeSettings) error

	UserLinks(ctx context.Context) ([]domain.UserLink, error)
	LinkedUser(ctx context.Context, youtrackLogin string) (string, error) // domain.ErrNotFound
	LinkUser(ctx context.Context, l domain.UserLink) error                // domain.ErrYouTrackLoginTaken, domain.ErrNotFound
	UnlinkUser(ctx context.Context, userID string) error

	IntakeRecord(ctx context.Context, projectID, issueID string) (domain.IntakeRecord, bool, error)
	SaveIntakeRecord(ctx context.Context, r domain.IntakeRecord) error
	RecentIntake(ctx context.Context, projectID string, limit int) ([]domain.IntakeRecord, error)
}

// Tasks is the workflow context's API as the intake needs it; it joins the caller's transaction.
type Tasks interface {
	TaskForIssue(ctx context.Context, projectID, issueID string) (taskID string, found bool, err error)
	CreateIssueTask(ctx context.Context, projectID, issueID, title, createdBy string) (taskID string, err error)
	RetitleTask(ctx context.Context, taskID, title string) (changed bool, err error)
	// AutoStartPlanning hands a task the intake just created to the AI planner on behalf of requestedBy (KIN-85).
	// The outcome is AutoPlanStarted or why the workflow declined (the task then stays as it is).
	AutoStartPlanning(ctx context.Context, taskID, requestedBy string) (outcome string, err error)
}

// AutoPlanStarted is the Tasks.AutoStartPlanning outcome of a started planning run.
const AutoPlanStarted = "started"

// Access is the projects context's permission check: may the account start AI work in the project?
type Access interface {
	CanRunAI(ctx context.Context, projectID, userID string) (bool, error)
	// CanManageAI: may the account change how AI works in the project (Owner, project admin)?
	CanManageAI(ctx context.Context, projectID, userID string) (bool, error)
}

// UserRef is how the intake sees an account (owned by the identity context).
type UserRef struct {
	ID    string
	Login string
}

// Users resolves accounts.
type Users interface {
	ByLogin(ctx context.Context, login string) (UserRef, error) // domain.ErrNotFound
	ByIDs(ctx context.Context, ids []string) (map[string]UserRef, error)
}

// IssueQuery selects the issues of one binding for a poll page.
type IssueQuery struct {
	ShortName string
	Tag       string
	Since     int64 // cursor (Unix ms); 0 = no lower bound
	Skip, Top int
}

// IssueSource reads issues from the YouTrack REST API. Errors carry no credentials or response bodies.
type IssueSource interface {
	// UpdatedIssues returns one page of the binding's tagged issues updated around or after Since, oldest first.
	UpdatedIssues(ctx context.Context, baseURL string, token domain.Secret, q IssueQuery) ([]domain.IssueSnapshot, error)
	Issue(ctx context.Context, baseURL string, token domain.Secret, issueID string) (domain.IssueSnapshot, error) // domain.ErrIssueNotFound
	// TagEvents returns the issue's tag history (KIN-50): domain.ErrIssueNotFound, ErrTagHistoryUnusable when the
	// answer cannot be relied on (malformed, too long), *StatusError, or a transient error.
	TagEvents(ctx context.Context, baseURL string, token domain.Secret, issueID string) ([]domain.TagEvent, error)
}

// ErrTagHistoryUnusable: YouTrack's tag history of an issue cannot be relied on; nobody counts as the tag's applier.
var ErrTagHistoryUnusable = errors.New("youtrack: tag history unusable")

// StatusError is an unexpected HTTP status from YouTrack.
type StatusError struct {
	Status     int
	RetryAfter time.Duration // from Retry-After on 429/503, 0 if absent
}

func (e *StatusError) Error() string { return fmt.Sprintf("youtrack: HTTP %d", e.Status) }

// Permanent reports a client error that retrying cannot fix (4xx other than 408 and 429).
func (e *StatusError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != 408 && e.Status != 429
}

// Health is the adapters' health registry (messaging.Health).
type Health interface {
	Allow(adapter string) bool
	Success(adapter string)
	Failure(adapter string, err error)
	Status(adapter string) messaging.AdapterStatus
}

// Intake implements the YouTrack intake use cases. Integrations supplies the connection, vault, transactions,
// audit log and outbox.
type Intake struct {
	Integrations *Service
	Store        IntakeStore
	Tasks        Tasks
	Access       Access
	Users        Users
	Source       IssueSource
	Inbox        Inbox
	Health       Health
	States       StateStore // KIN-38: the projects' task state → YouTrack state maps
	PageSize     int // issues per poll request (default 50)
	MaxPages     int // poll requests per binding and cycle (default 10)
	// FetchAttempts must equal the outbox's MaxAttempts (default: the messaging default).
	FetchAttempts int
	Tick          time.Duration // how often Run checks whether a poll is due (default 5s)

	mu     sync.Mutex
	resume map[string]int // binding/cursor -> issues already read when a poll stopped at the page cap
}

func (in *Intake) now() time.Time { return in.Integrations.Now().UTC() }

func (in *Intake) logger() *slog.Logger { return in.Integrations.logger() }

// fetchAttempts is the outbox's attempt limit: a run by ID that is still failing then is recorded as rejected.
func (in *Intake) fetchAttempts() int {
	if in.FetchAttempts > 0 {
		return in.FetchAttempts
	}
	return messaging.Config{}.WithDefaults().MaxAttempts
}

func (in *Intake) pageSize() int {
	if in.PageSize > 0 {
		return in.PageSize
	}
	return defaultPageSize
}

func (in *Intake) maxPages() int {
	if in.MaxPages > 0 {
		return in.MaxPages
	}
	return defaultMaxPages
}

// issuePayload is the inbox payload of TopicYouTrackIssue. Only the intake itself produces it, from a REST read.
type issuePayload struct {
	Issue       domain.IssueSnapshot `json:"issue"`
	Channel     string               `json:"channel"`
	ProjectID   string               `json:"projectId,omitempty"`   // run by ID: the project it was started in
	RequestedBy string               `json:"requestedBy,omitempty"` // run by ID: the member who started it
	// Poll and webhook (KIN-50): the YouTrack login that applied TaggedTag, from the issue's tag history; "" when it
	// could not be told. HandleIssue only trusts it for the binding's current trigger tag.
	TaggedBy  string `json:"taggedBy,omitempty"`
	TaggedTag string `json:"taggedTag,omitempty"`
}

// webhookPayload is all that is kept of a webhook call: the issue it names.
type webhookPayload struct {
	IssueID string `json:"issueId"`
}

// fetchPayload is the outbox payload of TopicYouTrackFetch.
type fetchPayload struct {
	IssueID     string `json:"issueId"`
	Channel     string `json:"channel"`
	ProjectID   string `json:"projectId,omitempty"`
	RequestedBy string `json:"requestedBy,omitempty"`
	RequestID   string `json:"requestId,omitempty"`
}

// ---- polling ----

// PollOnce polls every binding once. It does nothing without a configured connection or binding, or while the
// YouTrack circuit is open. A binding that fails (e.g. its YouTrack project was deleted) does not stop the others;
// Health records a failure, and PollOnce returns an error, only when every binding failed.
func (in *Intake) PollOnce(ctx context.Context) error {
	settings, err := in.Integrations.YouTrackSettings(ctx)
	if errors.Is(err, domain.ErrNotConfigured) || errors.Is(err, domain.ErrSecretNotSet) {
		return nil
	}
	if err != nil {
		return err
	}
	bindings, err := in.Store.Bindings(ctx)
	if err != nil || len(bindings) == 0 {
		return err
	}
	if !in.Health.Allow(AdapterYouTrack) {
		return nil
	}
	var errs []error
	for _, b := range bindings {
		if err := in.pollBinding(ctx, settings, b); err != nil {
			if ctx.Err() != nil {
				return err
			}
			in.logger().Warn("youtrack poll failed", "project", b.ProjectID, "youtrackProject", b.ShortName, "err", err)
			errs = append(errs, err)
		}
	}
	if len(errs) == len(bindings) {
		err := errors.Join(errs...)
		in.Health.Failure(AdapterYouTrack, err)
		return err
	}
	in.Health.Success(AdapterYouTrack)
	return nil
}

// pollBinding hands the binding's tagged issues updated at or after its cursor to the inbox, oldest first, and
// advances the cursor. The YouTrack query is coarse (whole days), so older issues it returns are skipped; when a
// cycle stops at the page cap, the next one resumes after the pages already read instead of re-reading them.
func (in *Intake) pollBinding(ctx context.Context, settings YouTrackSettings, b domain.Binding) error {
	key := b.ProjectID + "/" + b.ShortName + "/" + strconv.FormatInt(b.Cursor, 10)
	in.mu.Lock()
	skip := in.resume[key]
	in.mu.Unlock()
	cursor, capped := b.Cursor, true
	for range in.maxPages() {
		q := IssueQuery{ShortName: b.ShortName, Tag: b.TriggerTag, Since: b.Cursor, Skip: skip, Top: in.pageSize()}
		issues, err := in.Source.UpdatedIssues(ctx, settings.BaseURL, settings.Token, q)
		if err != nil {
			return err // the cursor stays: what was received is deduplicated by the inbox next time
		}
		skip += len(issues)
		for _, s := range issues {
			if s.Updated < b.Cursor {
				continue
			}
			if err := s.Validate(); err != nil || s.Project != b.ShortName {
				in.logger().Warn("youtrack issue skipped", "issue", s.ID, "err", err)
				continue
			}
			p := issuePayload{Issue: s, Channel: domain.ChannelPoll}
			if p.TaggedBy, p.TaggedTag, err = in.tagApplier(ctx, settings, b, s); err != nil {
				return err // as above: nothing past this issue is handed over, the cursor stays before it
			}
			if err := in.receive(ctx, p, issueDelivery(s)); err != nil {
				return err
			}
			cursor = max(cursor, s.Updated)
		}
		if len(issues) < in.pageSize() {
			capped = false
			break
		}
	}
	in.mu.Lock()
	delete(in.resume, key)
	if capped && cursor == b.Cursor { // nothing newer within the cap: continue from here next time
		if in.resume == nil {
			in.resume = map[string]int{}
		}
		in.resume[key] = skip
	}
	in.mu.Unlock()
	if capped {
		in.logger().Warn("youtrack poll stopped at the page cap", "youtrackProject", b.ShortName, "read", skip)
	}
	return in.Store.AdvanceCursor(ctx, b.ProjectID, b.ShortName, cursor, in.now())
}

// tagApplier reads who applied the binding's trigger tag to the issue (KIN-50) when that decides anything: for an
// open issue that carries the tag. A history that cannot be relied on for good (malformed, too long, the issue gone)
// yields no applier, so the issue is rejected (fail closed) instead of stalling the binding. Any other failure —
// transient, or a refusal such as 401/403 that the Owner must fix — is returned, so the cursor stays and the issue
// is decided once YouTrack answers instead of being rejected for good.
func (in *Intake) tagApplier(ctx context.Context, settings YouTrackSettings, b domain.Binding, s domain.IssueSnapshot) (login, tag string, err error) {
	if s.Resolved || !b.Wants(s, false) {
		return "", "", nil
	}
	events, err := in.Source.TagEvents(ctx, settings.BaseURL, settings.Token, s.ID)
	var se *StatusError
	switch {
	case errors.Is(err, ErrTagHistoryUnusable), errors.Is(err, domain.ErrIssueNotFound), errors.As(err, &se) && se.Status == http.StatusGone:
		in.logger().Warn("youtrack tag history unreadable", "issue", s.ID, "err", err)
		return "", b.TriggerTag, nil
	case err != nil:
		return "", "", err
	}
	login, ok := domain.TagApplier(events, b.TriggerTag)
	if !ok {
		in.logger().Warn("youtrack tag applier unknown", "issue", s.ID, "tag", b.TriggerTag)
	}
	return login, b.TriggerTag, nil
}

func issueDelivery(s domain.IssueSnapshot) string {
	return fmt.Sprintf("%s%s/%d", "issue/", s.ID, s.Updated)
}

func (in *Intake) receive(ctx context.Context, p issuePayload, delivery string) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding issue: %w", err)
	}
	_, err = in.Inbox.Receive(ctx, messaging.InboundEvent{Source: SourceYouTrack, DeliveryID: delivery,
		Topic: TopicYouTrackIssue, Payload: body})
	return err
}

// Run polls until ctx ends. Every tick it re-reads the poll interval, so a changed interval or a new binding is
// picked up within a tick; after failures it backs off exponentially up to 15 minutes, and never polls sooner than
// a Retry-After from YouTrack.
func (in *Intake) Run(ctx context.Context) {
	tick := in.Tick
	if tick <= 0 {
		tick = 5 * time.Second
	}
	var last time.Time // zero: poll at once
	failures := 0
	var lastErr error
	for {
		interval := domain.DefaultIntakeSettings().PollInterval
		if s, err := in.Store.IntakeSettings(ctx); err == nil {
			interval = s.PollInterval
		}
		wait := interval
		if failures > 0 {
			wait = NextPoll(interval, failures, lastErr)
		}
		if last.IsZero() || !time.Now().Before(last.Add(wait)) {
			last = time.Now()
			if lastErr = in.PollOnce(ctx); ctx.Err() != nil {
				return
			}
			if lastErr != nil {
				failures++
			} else {
				failures = 0
			}
		}
		t := time.NewTimer(tick)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// NextPoll is the wait after the given number of consecutive failed polls: the interval doubled per failure, capped
// at 15 minutes (or the interval, if longer), and at least a Retry-After YouTrack sent.
func NextPoll(interval time.Duration, failures int, err error) time.Duration {
	wait := interval
	for i := 0; i < failures && wait < maxBackoff; i++ {
		wait *= 2
	}
	wait = min(wait, max(maxBackoff, interval))
	var se *StatusError
	if errors.As(err, &se) && se.RetryAfter > wait {
		wait = se.RetryAfter
	}
	return wait
}

// ---- inbox: issue snapshots ----

// HandleIssue is the inbox handler of TopicYouTrackIssue. It runs in the inbox transaction and calls nothing
// external. Snapshots of unbound or untagged issues are ignored, older ones than already applied too (out of
// order); an existing task follows the summary; a new task needs an initiator who may start AI in the project.
func (in *Intake) HandleIssue(ctx context.Context, e messaging.InboundEvent) error {
	var p issuePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return messaging.Permanent(errors.New("malformed issue payload"))
	}
	s := p.Issue
	if err := s.Validate(); err != nil {
		return messaging.Permanent(fmt.Errorf("issue snapshot: %w", err))
	}
	manual := p.Channel == domain.ChannelManual
	b, err := in.Store.BindingByShortName(ctx, s.Project)
	switch {
	case errors.Is(err, domain.ErrNotFound) && manual:
		return in.rejectManual(ctx, p, domain.ReasonOtherProject)
	case errors.Is(err, domain.ErrNotFound):
		return nil // unbound meanwhile
	case err != nil:
		return err
	}
	if manual && b.ProjectID != p.ProjectID {
		return in.rejectManual(ctx, p, domain.ReasonOtherProject)
	}
	if !b.Wants(s, manual) {
		return nil
	}
	prev, seen, err := in.Store.IntakeRecord(ctx, b.ProjectID, s.ID)
	if err != nil {
		return err
	}
	if seen && !manual && prev.Supersedes(s) {
		return nil
	}
	rec := domain.IntakeRecord{ProjectID: b.ProjectID, IssueID: s.ID, IssueUpdated: s.Updated, Reporter: s.Reporter,
		Channel: p.Channel, ProcessedAt: in.now()}
	title := s.TaskTitle()
	taskID, found, err := in.Tasks.TaskForIssue(ctx, b.ProjectID, s.ID)
	if err != nil {
		return err
	}
	if found {
		changed, err := in.Tasks.RetitleTask(ctx, taskID, title)
		if err != nil {
			return err
		}
		rec.TaskID, rec.Outcome = taskID, domain.OutcomeUnchanged
		if changed {
			rec.Outcome = domain.OutcomeUpdated
		}
		if seen {
			rec.Initiator = prev.Initiator
		}
		return in.Store.SaveIntakeRecord(ctx, rec)
	}
	if s.Resolved && !manual {
		return nil // a closed issue does not start new work
	}
	// The initiator is the member who ran the issue by ID, or the YouTrack user who applied the trigger tag: anybody
	// who may edit an issue may tag it, so the reporter grants nothing (KIN-50).
	initiator, denied := p.RequestedBy, domain.ReasonNotAllowed
	if !manual {
		denied = domain.ReasonTagNotAllowed
		rec.Tagger = p.TaggedBy
		if p.TaggedBy == "" || !strings.EqualFold(strings.TrimSpace(p.TaggedTag), b.TriggerTag) {
			return in.reject(ctx, rec, denied)
		}
		initiator, err = in.Store.LinkedUser(ctx, p.TaggedBy)
		if errors.Is(err, domain.ErrNotFound) {
			return in.reject(ctx, rec, denied)
		}
		if err != nil {
			return err
		}
	}
	rec.Initiator = initiator
	ok, err := in.Access.CanRunAI(ctx, b.ProjectID, initiator)
	if err != nil {
		return err
	}
	if !ok {
		return in.reject(ctx, rec, denied)
	}
	if rec.TaskID, err = in.Tasks.CreateIssueTask(ctx, b.ProjectID, s.ID, title, initiator); err != nil {
		return err
	}
	rec.Outcome = domain.OutcomeCreated
	if err := in.Store.SaveIntakeRecord(ctx, rec); err != nil {
		return err
	}
	if err := in.Integrations.record(ctx, auditapp.System(), KindYouTrackTaskCreated, b.ProjectID, "task:"+rec.TaskID,
		map[string]string{"issue": s.ID, "initiator": initiator, "tagger": rec.Tagger, "channel": p.Channel}); err != nil {
		return err
	}
	if !b.AutoPlan {
		return nil
	}
	return in.autoPlan(ctx, b.ProjectID, rec.TaskID, s.ID, initiator)
}

// autoPlan hands a task the intake just created to the planner (KIN-85) in the same transaction. Only a new task
// gets here, so a redelivered or changed issue never plans twice; the workflow refuses a second start as well.
func (in *Intake) autoPlan(ctx context.Context, projectID, taskID, issueID, initiator string) error {
	outcome, err := in.Tasks.AutoStartPlanning(ctx, taskID, initiator)
	if err != nil {
		return err
	}
	details := map[string]string{"issue": issueID, "initiator": initiator}
	if outcome == AutoPlanStarted {
		return in.Integrations.record(ctx, auditapp.System(), KindYouTrackPlanningStarted, projectID, "task:"+taskID, details)
	}
	details["reason"] = outcome
	return in.Integrations.record(ctx, auditapp.System(), KindYouTrackPlanningSkipped, projectID, "task:"+taskID, details)
}

func (in *Intake) reject(ctx context.Context, rec domain.IntakeRecord, reason string) error {
	rec.Outcome, rec.Reason = domain.OutcomeRejected, reason
	prev, seen, err := in.Store.IntakeRecord(ctx, rec.ProjectID, rec.IssueID)
	if err != nil {
		return err
	}
	switch {
	case seen && prev.TaskID != "":
		// The issue already has a task: a failed run by ID must not hide it. Only the audit log records the failure;
		// a queued run of it is settled as "up to date".
		if prev.Outcome == domain.OutcomeQueued {
			prev.Outcome, prev.ProcessedAt = domain.OutcomeUnchanged, rec.ProcessedAt
			if err := in.Store.SaveIntakeRecord(ctx, prev); err != nil {
				return err
			}
		}
	default:
		if seen {
			rec.IssueUpdated = max(rec.IssueUpdated, prev.IssueUpdated)
			rec.Reporter = cmp.Or(rec.Reporter, prev.Reporter)
		}
		if err := in.Store.SaveIntakeRecord(ctx, rec); err != nil {
			return err
		}
	}
	return in.Integrations.record(ctx, auditapp.System(), KindYouTrackIssueRejected, rec.ProjectID,
		"project:"+rec.ProjectID, map[string]string{"issue": rec.IssueID, "reason": reason,
			"reporter": rec.Reporter, "initiator": rec.Initiator, "tagger": rec.Tagger, "channel": rec.Channel})
}

func (in *Intake) rejectManual(ctx context.Context, p issuePayload, reason string) error {
	if p.ProjectID == "" {
		return nil
	}
	return in.reject(ctx, domain.IntakeRecord{ProjectID: p.ProjectID, IssueID: p.Issue.ID, Reporter: p.Issue.Reporter,
		Initiator: p.RequestedBy, Channel: domain.ChannelManual, ProcessedAt: in.now()}, reason)
}

// ---- webhook ----

// ReceiveWebhook accepts a webhook call: the shared secret must match (constant time), and only the issue ID of
// the body is kept. The issue's state is read through the REST API later, so a forged or replayed body can at
// most cause a re-read. domain.ErrWebhookDisabled without a secret, domain.ErrWebhookUnauthorized on a mismatch.
func (in *Intake) ReceiveWebhook(ctx context.Context, presented string, body []byte) error {
	secret, err := in.Integrations.Vault.Credential(ctx, domain.SecretYouTrackWebhookSecret)
	if errors.Is(err, domain.ErrSecretNotSet) {
		return domain.ErrWebhookDisabled
	}
	if err != nil {
		return err
	}
	want, got := sha256.Sum256([]byte(secret.Reveal())), sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return domain.ErrWebhookUnauthorized
	}
	var req struct {
		IssueID string `json:"issueId"`
		Updated int64  `json:"updated"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.IssueID) > maxWebhookIssueIDBytes {
		return &domain.ValidationError{Field: "issueId", Code: "issue_format"}
	}
	id, err := domain.NewIssueID(req.IssueID)
	if err != nil {
		return err
	}
	// Without `updated`, calls for one issue within the same minute are one delivery (bounded re-reads).
	delivery := fmt.Sprintf("hook/%s/m%d", id, in.now().Unix()/60)
	if req.Updated > 0 {
		delivery = fmt.Sprintf("%s%s/%d", "hook/", id, req.Updated)
	}
	payload, err := json.Marshal(webhookPayload{IssueID: id})
	if err != nil {
		return err
	}
	_, err = in.Inbox.Receive(ctx, messaging.InboundEvent{Source: SourceYouTrack, DeliveryID: delivery,
		Topic: TopicYouTrackWebhook, Payload: payload})
	return err
}

// HandleWebhook is the inbox handler of TopicYouTrackWebhook: it asks the outbox to read the issue.
func (in *Intake) HandleWebhook(ctx context.Context, e messaging.InboundEvent) error {
	var p webhookPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return messaging.Permanent(errors.New("malformed webhook payload"))
	}
	id, err := domain.NewIssueID(p.IssueID)
	if err != nil {
		return messaging.Permanent(err)
	}
	return in.enqueueFetch(ctx, "fetch/"+e.DeliveryID, fetchPayload{IssueID: id, Channel: domain.ChannelWebhook})
}

func (in *Intake) enqueueFetch(ctx context.Context, key string, p fetchPayload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding fetch: %w", err)
	}
	return in.Integrations.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterYouTrack, Topic: TopicYouTrackFetch,
		Key: key, Payload: body})
}

// HandleFetch is the outbox handler of TopicYouTrackFetch: it reads the issue and hands the snapshot to the inbox.
// A run by ID that cannot be read is recorded as rejected so the member sees why.
func (in *Intake) HandleFetch(ctx context.Context, d messaging.Delivery) error {
	var p fetchPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil {
		return messaging.Permanent(errors.New("malformed fetch payload"))
	}
	id, err := domain.NewIssueID(p.IssueID)
	if err != nil {
		return messaging.Permanent(err)
	}
	settings, err := in.Integrations.YouTrackSettings(ctx)
	if errors.Is(err, domain.ErrNotConfigured) || errors.Is(err, domain.ErrSecretNotSet) {
		return in.fetchFailed(ctx, p, domain.ReasonUnavailable, nil)
	}
	if err != nil {
		return err
	}
	s, err := in.Source.Issue(ctx, settings.BaseURL, settings.Token, id)
	var se *StatusError
	switch {
	case errors.Is(err, domain.ErrIssueNotFound):
		return in.fetchFailed(ctx, p, domain.ReasonIssueNotFound, nil)
	case errors.As(err, &se) && se.Permanent():
		return in.fetchFailed(ctx, p, domain.ReasonUnavailable, messaging.Permanent(err))
	case err != nil:
		if p.Channel == domain.ChannelManual && d.Attempt >= in.fetchAttempts() {
			return in.fetchFailed(ctx, p, domain.ReasonUnavailable, err)
		}
		return err
	}
	if err := s.Validate(); err != nil || s.ID != id {
		return in.fetchFailed(ctx, p, domain.ReasonInvalidIssue, nil)
	}
	delivery := issueDelivery(s)
	out := issuePayload{Issue: s, Channel: p.Channel, ProjectID: p.ProjectID, RequestedBy: p.RequestedBy}
	if p.Channel == domain.ChannelManual {
		delivery = "manual/" + p.RequestID
	} else {
		b, err := in.Store.BindingByShortName(ctx, s.Project)
		switch {
		case errors.Is(err, domain.ErrNotFound): // unbound: HandleIssue ignores it
		case err != nil:
			return err
		default:
			if out.TaggedBy, out.TaggedTag, err = in.tagApplier(ctx, settings, b, s); err != nil {
				if errors.As(err, &se) && se.Permanent() {
					return messaging.Permanent(err) // polling covers the issue once the Owner fixes the token
				}
				return err // the outbox retries; polling still covers the issue if it gives up
			}
		}
	}
	return in.receive(ctx, out, delivery)
}

// fetchFailed records why a run by ID produced no task (webhook hints are dropped: polling still covers them) and
// returns cause.
func (in *Intake) fetchFailed(ctx context.Context, p fetchPayload, reason string, cause error) error {
	if p.Channel != domain.ChannelManual || p.ProjectID == "" {
		return cause
	}
	err := in.Integrations.Tx.InTx(ctx, func(ctx context.Context) error {
		return in.reject(ctx, domain.IntakeRecord{ProjectID: p.ProjectID, IssueID: p.IssueID, Initiator: p.RequestedBy,
			Channel: domain.ChannelManual, ProcessedAt: in.now()}, reason)
	})
	return errors.Join(cause, err)
}

// ---- run by issue ID ----

// RunIssue starts intake of one issue on behalf of a project member, without the trigger tag. The member becomes
// the initiator; the issue is read through the outbox, so the call returns at once with a queued record.
func (in *Intake) RunIssue(ctx context.Context, actor auth.Principal, projectID, issueID string) (domain.IntakeRecord, error) {
	if err := in.mayRun(ctx, actor, projectID); err != nil {
		return domain.IntakeRecord{}, err
	}
	id, err := domain.NewIssueID(issueID)
	if err != nil {
		return domain.IntakeRecord{}, err
	}
	var rec domain.IntakeRecord
	err = in.Integrations.inTx(ctx, func(ctx context.Context) error {
		if _, err := in.Integrations.Store.YouTrack(ctx); err != nil {
			return err
		}
		b, err := in.Store.Binding(ctx, projectID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrNotBound
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(id, b.ShortName+"-") {
			return &domain.ValidationError{Field: "issueId", Code: "issue_project"}
		}
		reqID := in.Integrations.NewID()
		if err := in.enqueueFetch(ctx, "run/"+reqID, fetchPayload{IssueID: id, Channel: domain.ChannelManual,
			ProjectID: projectID, RequestedBy: actor.UserID, RequestID: reqID}); err != nil {
			return err
		}
		prev, seen, err := in.Store.IntakeRecord(ctx, projectID, id)
		if err != nil {
			return err
		}
		rec = domain.IntakeRecord{ProjectID: projectID, IssueID: id, Outcome: domain.OutcomeQueued, Initiator: actor.UserID,
			Channel: domain.ChannelManual, ProcessedAt: in.now()}
		if seen {
			rec.IssueUpdated, rec.Reporter, rec.TaskID = prev.IssueUpdated, prev.Reporter, prev.TaskID
		}
		if err := in.Store.SaveIntakeRecord(ctx, rec); err != nil {
			return err
		}
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackRunRequested, projectID,
			"project:"+projectID, map[string]string{"issue": id, "requestId": reqID})
	})
	return rec, err
}

// mayRun: members who may start AI in the project; everybody else learns nothing about it.
func (in *Intake) mayRun(ctx context.Context, actor auth.Principal, projectID string) error {
	ok, err := in.Access.CanRunAI(ctx, projectID, actor.UserID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrNotFound
	}
	return nil
}

// ---- views ----

// IntakeStatus is the YouTrack adapter's health as members see it (no error detail).
type IntakeStatus struct {
	State        messaging.State
	LastPolledAt *time.Time
}

// IntakeEntry is an intake decision with the initiator's login.
type IntakeEntry struct {
	domain.IntakeRecord
	InitiatorLogin string
}

// ProjectIntakeView is what project members see: the binding (nil when unbound), the integration status and the
// latest decisions with their reasons.
type ProjectIntakeView struct {
	Connected bool
	Binding   *domain.Binding
	Status    IntakeStatus
	Recent    []IntakeEntry
}

// ProjectIntake returns a project's intake view for members who may start AI in it.
func (in *Intake) ProjectIntake(ctx context.Context, actor auth.Principal, projectID string) (ProjectIntakeView, error) {
	if err := in.mayRun(ctx, actor, projectID); err != nil {
		return ProjectIntakeView{}, err
	}
	var v ProjectIntakeView
	if _, err := in.Integrations.Store.YouTrack(ctx); err == nil {
		v.Connected = true
	} else if !errors.Is(err, domain.ErrNotConfigured) {
		return v, err
	}
	b, err := in.Store.Binding(ctx, projectID)
	switch {
	case err == nil:
		v.Binding = &b
		v.Status.LastPolledAt = b.LastPolledAt
	case !errors.Is(err, domain.ErrNotFound):
		return v, err
	}
	v.Status.State = in.Health.Status(AdapterYouTrack).State
	recs, err := in.Store.RecentIntake(ctx, projectID, recentIntakeLimit)
	if err != nil {
		return v, err
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		if r.Initiator != "" {
			ids = append(ids, r.Initiator)
		}
	}
	users, err := in.Users.ByIDs(ctx, ids)
	if err != nil {
		return v, err
	}
	v.Recent = make([]IntakeEntry, 0, len(recs))
	for _, r := range recs {
		v.Recent = append(v.Recent, IntakeEntry{IntakeRecord: r, InitiatorLogin: users[r.Initiator].Login})
	}
	return v, nil
}

// UserLinkView is a linked YouTrack login with the account's login ("" if the account is gone).
type UserLinkView struct {
	domain.UserLink
	Login string
}

// IntakeOverview is the Owner's view of the intake settings.
type IntakeOverview struct {
	Settings      domain.IntakeSettings
	WebhookSecret domain.SecretMeta
	Links         []UserLinkView
	Health        messaging.AdapterStatus
}

// IntakeOverview returns the intake settings (Owner only).
func (in *Intake) IntakeOverview(ctx context.Context, actor auth.Principal) (IntakeOverview, error) {
	if err := ownerOnly(actor); err != nil {
		return IntakeOverview{}, err
	}
	var o IntakeOverview
	var err error
	if o.Settings, err = in.Store.IntakeSettings(ctx); err != nil {
		return o, err
	}
	if o.WebhookSecret, err = in.Integrations.Vault.Meta(ctx, domain.SecretYouTrackWebhookSecret); err != nil {
		return o, err
	}
	links, err := in.Store.UserLinks(ctx)
	if err != nil {
		return o, err
	}
	ids := make([]string, 0, len(links))
	for _, l := range links {
		ids = append(ids, l.UserID)
	}
	users, err := in.Users.ByIDs(ctx, ids)
	if err != nil {
		return o, err
	}
	o.Links = make([]UserLinkView, 0, len(links))
	for _, l := range links {
		o.Links = append(o.Links, UserLinkView{UserLink: l, Login: users[l.UserID].Login})
	}
	o.Health = in.Health.Status(AdapterYouTrack)
	return o, nil
}

// ---- Owner settings ----

// SetPollInterval changes the polling interval in seconds (Owner only); it applies from the next poll.
func (in *Intake) SetPollInterval(ctx context.Context, actor auth.Principal, seconds int) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	d, err := domain.NewPollInterval(seconds)
	if err != nil {
		return err
	}
	return in.Integrations.inTx(ctx, func(ctx context.Context) error {
		if err := in.Store.SaveIntakeSettings(ctx, domain.IntakeSettings{PollInterval: d, UpdatedAt: in.now()}); err != nil {
			return err
		}
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackIntakeChanged, "", youtrackIntakeSubject,
			map[string]string{"pollIntervalSeconds": fmt.Sprint(seconds)})
	})
}

// SetWebhookSecret sets the webhook shared secret, or with "" removes it and so disables the webhook (Owner only).
func (in *Intake) SetWebhookSecret(ctx context.Context, actor auth.Principal, secret string) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	var sec domain.Secret
	if secret != "" {
		var err error
		if sec, err = domain.NewYouTrackWebhookSecret(secret); err != nil {
			return err
		}
	}
	return in.Integrations.inTx(ctx, func(ctx context.Context) error {
		var err error
		if sec.IsZero() {
			err = in.Integrations.Vault.Delete(ctx, domain.SecretYouTrackWebhookSecret)
		} else {
			err = in.Integrations.Vault.Put(ctx, domain.SecretYouTrackWebhookSecret, sec, actor.UserID)
		}
		if err != nil {
			return err
		}
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackWebhookChanged, "", youtrackIntakeSubject,
			map[string]string{"enabled": fmt.Sprint(!sec.IsZero())})
	})
}

// SetTriggerTag changes the tag that marks a bound project's issues ("" = the default) (Owner only).
func (in *Intake) SetTriggerTag(ctx context.Context, actor auth.Principal, projectID, tag string) (domain.Binding, error) {
	if err := ownerOnly(actor); err != nil {
		return domain.Binding{}, err
	}
	t, err := domain.NewTriggerTag(tag)
	if err != nil {
		return domain.Binding{}, err
	}
	var b domain.Binding
	err = in.Integrations.inTx(ctx, func(ctx context.Context) error {
		if b, err = in.Store.Binding(ctx, projectID); errors.Is(err, domain.ErrNotFound) {
			return domain.ErrNotBound
		} else if err != nil {
			return err
		}
		if b.TriggerTag == t {
			return nil
		}
		if err := in.Store.SetTriggerTag(ctx, projectID, t, in.now()); err != nil {
			return err
		}
		b.TriggerTag = t
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackTriggerChanged, projectID,
			"project:"+projectID, map[string]string{"triggerTag": t})
	})
	return b, err
}

// SetAutoPlan turns automatic planning of the project's intake tasks on or off (KIN-85): the Owner or the project's
// admin; other members are refused, everybody else learns nothing about the project.
func (in *Intake) SetAutoPlan(ctx context.Context, actor auth.Principal, projectID string, on bool) (domain.Binding, error) {
	if !actor.Owner {
		ok, err := in.Access.CanManageAI(ctx, projectID, actor.UserID)
		if err != nil {
			return domain.Binding{}, err
		}
		if !ok {
			if err := in.mayRun(ctx, actor, projectID); err != nil {
				return domain.Binding{}, err
			}
			return domain.Binding{}, domain.ErrForbidden
		}
	}
	var b domain.Binding
	err := in.Integrations.inTx(ctx, func(ctx context.Context) error {
		var err error
		if b, err = in.Store.Binding(ctx, projectID); errors.Is(err, domain.ErrNotFound) {
			return domain.ErrNotBound
		} else if err != nil {
			return err
		}
		if b.AutoPlan == on {
			return nil
		}
		if err := in.Store.SetAutoPlan(ctx, projectID, on, in.now()); err != nil {
			return err
		}
		b.AutoPlan = on
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackAutoPlanChanged, projectID,
			"project:"+projectID, map[string]string{"autoPlan": fmt.Sprint(on)})
	})
	return b, err
}

// Binding returns a project's binding for trusted callers (domain.ErrNotFound when unbound).
func (in *Intake) Binding(ctx context.Context, projectID string) (domain.Binding, error) {
	return in.Store.Binding(ctx, projectID)
}

// LinkUser links a YouTrack login to the account with the given platform login (Owner only). Only the Owner may,
// because the link decides whose permissions a YouTrack issue runs with.
func (in *Intake) LinkUser(ctx context.Context, actor auth.Principal, login, youtrackLogin string) (UserLinkView, error) {
	if err := ownerOnly(actor); err != nil {
		return UserLinkView{}, err
	}
	yl, err := domain.NewYouTrackLogin(youtrackLogin)
	if err != nil {
		return UserLinkView{}, err
	}
	u, err := in.Users.ByLogin(ctx, strings.TrimSpace(login))
	if errors.Is(err, domain.ErrNotFound) {
		return UserLinkView{}, &domain.ValidationError{Field: "login", Code: "user_not_found"}
	}
	if err != nil {
		return UserLinkView{}, err
	}
	l := domain.UserLink{YouTrackLogin: yl, UserID: u.ID, UpdatedAt: in.now()}
	err = in.Integrations.inTx(ctx, func(ctx context.Context) error {
		if err := in.Store.LinkUser(ctx, l); err != nil {
			return err
		}
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackUserLinked, "",
			"user:"+u.ID, map[string]string{"youtrackLogin": yl})
	})
	return UserLinkView{UserLink: l, Login: u.Login}, err
}

// UnlinkUser removes an account's YouTrack login (Owner only).
func (in *Intake) UnlinkUser(ctx context.Context, actor auth.Principal, userID string) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	return in.Integrations.inTx(ctx, func(ctx context.Context) error {
		if err := in.Store.UnlinkUser(ctx, userID); err != nil {
			return err
		}
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackUserUnlinked, "",
			"user:"+userID, nil)
	})
}
