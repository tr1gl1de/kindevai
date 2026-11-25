// Package app holds the Alert Center use cases (KIN-40): raising and grouping alerts from the monitors' outbox
// events, the users' alert list and actions, notification preferences and the fan-out of Telegram notifications
// through the outbox.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// Outbox: one Telegram message to one linked user. The Telegram adapter registers the handler.
const (
	AdapterTelegram   = "telegram"
	TopicTelegramSend = "telegram.send"
)

// Audit event kinds recorded by the alerts context.
const (
	KindAcknowledged       = "alert.acknowledged"
	KindDismissed          = "alert.dismissed"
	KindResolved           = "alert.resolved"
	KindPreferencesUpdated = "notifications.preferences_updated"
)

// Project roles as the projects context reports them.
const RoleProjectAdmin = "project_admin"

// MaxList caps the alert list.
const MaxList = 200

// ListQuery selects alerts. Platform alerts (no project) are included only when Platform is set.
type ListQuery struct {
	ActiveOnly bool
	Platform   bool
	ProjectIDs []string
	Limit      int
}

// Repository stores alerts and preferences; every method runs on the caller's transaction (db.Conn).
type Repository interface {
	Active(ctx context.Context, dedupKey string) (domain.Alert, error) // domain.ErrNotFound
	// Applied reports whether event was the last one applied to an alert with dedupKey (active or closed).
	Applied(ctx context.Context, dedupKey, event string) (bool, error)
	// Resolved is the latest observation time of an "anomaly ended" event for dedupKey; SetResolved records one,
	// keeping the latest (KIN-99).
	Resolved(ctx context.Context, dedupKey string) (time.Time, bool, error)
	SetResolved(ctx context.Context, dedupKey string, at time.Time) error
	Create(ctx context.Context, a domain.Alert) error
	Save(ctx context.Context, a domain.Alert) error
	Get(ctx context.Context, id string) (domain.Alert, error)                   // domain.ErrNotFound
	List(ctx context.Context, q ListQuery) ([]domain.Alert, error)              // newest activity first
	Preferences(ctx context.Context, userID string) (domain.Preferences, error) // defaults when never saved
	SavePreferences(ctx context.Context, p domain.Preferences, now time.Time) error

	// Analysis tasks (KIN-61): at most one per alert.
	Analysis(ctx context.Context, alertID string) (domain.AnalysisTask, error) // domain.ErrNotFound
	Analyses(ctx context.Context, alertIDs []string) (map[string]domain.AnalysisTask, error)
	CreateAnalysis(ctx context.Context, t domain.AnalysisTask) error
	SaveAnalysis(ctx context.Context, t domain.AnalysisTask) error
}

// ProjectRef is a project visible to a user, with the user's role ("owner" for the platform Owner).
type ProjectRef struct {
	ID, Name, Role string
}

// Projects is the projects context as the Alert Center needs it.
type Projects interface {
	Visible(ctx context.Context, p auth.Principal) ([]ProjectRef, error)
	Name(ctx context.Context, projectID string) (string, error)
	Members(ctx context.Context, projectID string) ([]string, error) // user ids
	Admins(ctx context.Context, projectID string) ([]string, error)  // user ids of the project's admins
}

// People is the identity context as the notifier needs it.
type People interface {
	Owners(ctx context.Context) ([]string, error) // enabled platform Owners' user ids
	// TelegramChat returns the linked Telegram user id (the private chat with the bot) of an enabled user.
	TelegramChat(ctx context.Context, userID string) (int64, bool, error)
}

// Approvals is the approvals context as the notifier needs it.
type Approvals interface {
	Pending(ctx context.Context, approvalID string) (bool, error) // false for unknown or decided approvals
}

// AuditLog records audit events in the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Outbox stores outgoing messages in the caller's transaction.
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// TxRunner runs fn in one transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Service implements the Alert Center.
type Service struct {
	Repo      Repository
	Projects  Projects
	People    People
	Approvals Approvals
	Tx        TxRunner
	Audit     AuditLog
	Outbox    Outbox
	Tracker   IssueTracker // YouTrack, for analysis tasks (KIN-61); nil disables them
	Now       func() time.Time
	NewID     func() string
}

// SendPayload is the outbox payload of TopicTelegramSend. It holds no secrets: the adapter reads the bot token
// from the vault and builds the Mini App link from the bot settings at delivery time.
type SendPayload struct {
	ChatID     int64  `json:"chatId"`
	Text       string `json:"text"`
	Button     string `json:"button,omitempty"`
	StartParam string `json:"startParam,omitempty"` // Mini App start parameter of the button, e.g. "approval_<id>"
}

// Raise records a monitor's occurrence (event identifies it, for redeliveries): a new alert, or a repeat grouped
// into the active alert with the same dedup key. New and escalated alerts notify their recipients.
func (s *Service) Raise(ctx context.Context, event string, o domain.Occurrence) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		now := s.Now()
		a, err := s.Repo.Active(ctx, o.DedupKey)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			// A redelivery may arrive after its alert was closed: it must not open a duplicate.
			if done, err := s.Repo.Applied(ctx, o.DedupKey, event); err != nil || done {
				return err
			}
			// An occurrence observed no later than the anomaly was seen ended, delivered late, is over (KIN-99).
			if ended, err := s.endedBy(ctx, o); err != nil || ended {
				return err
			}
			if a, err = domain.NewAlert(s.NewID(), o, now); err != nil {
				return err
			}
			a.LastEvent = event
			if err := s.Repo.Create(ctx, a); err != nil {
				return fmt.Errorf("creating alert: %w", err)
			}
			return s.notifyAlert(ctx, a)
		case err != nil:
			return fmt.Errorf("loading alert: %w", err)
		case a.LastEvent == event:
			return nil // redelivery of the event already applied
		}
		// An untimed occurrence opened the alert after the anomaly ended: an ended one is not grouped into it.
		if ended, err := s.endedBy(ctx, o); err != nil || ended {
			return err
		}
		escalated, err := a.Recur(o, now)
		if err != nil {
			return err
		}
		a.LastEvent = event
		if err := s.Repo.Save(ctx, a); err != nil {
			return fmt.Errorf("saving alert: %w", err)
		}
		if escalated {
			return s.notifyAlert(ctx, a)
		}
		return nil
	})
}

// ResolveKey closes the active alert with dedupKey because the monitor saw the anomaly end; no alert is a no-op.
func (s *Service) ResolveKey(ctx context.Context, dedupKey string) error {
	return s.ResolveKeyObservedAt(ctx, dedupKey, time.Time{})
}

// ResolveKeyObservedAt is ResolveKey for an "anomaly ended" event the monitor observed at at: an alert observed
// after that (a later occurrence whose event was delivered first) stays active (KIN-88). Otherwise at is kept for
// the dedup key, with or without an active alert, so an occurrence observed no later than at and delivered after
// this event opens no alert (KIN-99). A zero at resolves as ResolveKey.
func (s *Service) ResolveKeyObservedAt(ctx context.Context, dedupKey string, at time.Time) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		a, err := s.Repo.Active(ctx, dedupKey)
		found := err == nil
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("loading alert: %w", err)
		}
		if found && a.ObservedAfter(at) {
			return nil
		}
		if !at.IsZero() { // KIN-99: an occurrence observed until at and delivered later opens no alert
			if err := s.Repo.SetResolved(ctx, dedupKey, at); err != nil {
				return err
			}
		}
		if !found {
			return nil
		}
		if err := a.Resolve("", s.Now()); err != nil {
			return err
		}
		return s.Repo.Save(ctx, a)
	})
}

// endedBy reports whether a timed occurrence was observed no later than the last "anomaly ended" event of its dedup
// key (KIN-99). An untimed one never is.
func (s *Service) endedBy(ctx context.Context, o domain.Occurrence) (bool, error) {
	if o.ObservedAt.IsZero() {
		return false, nil
	}
	at, found, err := s.Repo.Resolved(ctx, o.DedupKey)
	return found && !o.ObservedAt.After(at), err
}

// View is an alert as a user sees it.
type View struct {
	Alert       domain.Alert
	ProjectName string
	CanManage   bool
	CanSuggest  bool                 // may suggest an analysis task (Owner / Project Admin), whatever the state
	Analysis    *domain.AnalysisTask // the alert's analysis task, nil if none was requested
}

// access is what a principal may see: every project for the Owner, else the visible projects with their role.
type access struct {
	owner bool
	roles map[string]ProjectRef
}

func (s *Service) access(ctx context.Context, p auth.Principal) (access, error) {
	ps, err := s.Projects.Visible(ctx, p)
	if err != nil {
		return access{}, fmt.Errorf("listing projects: %w", err)
	}
	a := access{owner: p.Owner, roles: make(map[string]ProjectRef, len(ps))}
	for _, x := range ps {
		a.roles[x.ID] = x
	}
	return a, nil
}

func (a access) sees(al domain.Alert) bool {
	if a.owner {
		return true
	}
	_, ok := a.roles[al.ProjectID]
	return al.ProjectID != "" && ok
}

// manages: the Owner manages every alert; a Project Admin the alerts of their project.
func (a access) manages(al domain.Alert) bool {
	return a.owner || (al.ProjectID != "" && a.roles[al.ProjectID].Role == RoleProjectAdmin)
}

// List returns the alerts p may see (platform alerts only for the Owner), newest activity first.
func (s *Service) List(ctx context.Context, p auth.Principal, activeOnly bool) ([]View, error) {
	acc, err := s.access(ctx, p)
	if err != nil {
		return nil, err
	}
	q := ListQuery{ActiveOnly: activeOnly, Platform: acc.owner, Limit: MaxList}
	for id := range acc.roles {
		q.ProjectIDs = append(q.ProjectIDs, id)
	}
	slices.Sort(q.ProjectIDs)
	if !acc.owner && len(q.ProjectIDs) == 0 {
		return []View{}, nil
	}
	as, err := s.Repo.List(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing alerts: %w", err)
	}
	ids := make([]string, 0, len(as))
	for _, a := range as {
		ids = append(ids, a.ID)
	}
	tasks, err := s.Repo.Analyses(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("listing analysis tasks: %w", err)
	}
	out := make([]View, 0, len(as))
	for _, a := range as {
		if acc.sees(a) { // defence in depth: the query already filters
			v := View{Alert: a, ProjectName: acc.roles[a.ProjectID].Name, CanManage: acc.manages(a), CanSuggest: acc.manages(a)}
			if t, ok := tasks[a.ID]; ok {
				v.Analysis = &t
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// Action is a user's change of an alert's state.
type Action string

const (
	ActionAcknowledge Action = "acknowledge"
	ActionDismiss     Action = "dismiss"
	ActionResolve     Action = "resolve"
)

// Act applies a user's action. An alert p may not see is domain.ErrNotFound; one p sees but may not manage is
// domain.ErrForbidden.
func (s *Service) Act(ctx context.Context, p auth.Principal, id string, act Action) (View, error) {
	var out View
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		acc, err := s.access(ctx, p)
		if err != nil {
			return err
		}
		a, err := s.Repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if !acc.sees(a) {
			return domain.ErrNotFound
		}
		if !acc.manages(a) {
			return domain.ErrForbidden
		}
		now := s.Now()
		var kind string
		switch act {
		case ActionAcknowledge:
			err, kind = a.Acknowledge(p.UserID, now), KindAcknowledged
		case ActionDismiss:
			err, kind = a.Dismiss(p.UserID, now), KindDismissed
		case ActionResolve:
			err, kind = a.Resolve(p.UserID, now), KindResolved
		default:
			return &domain.ValidationError{Field: "action", Code: "unknown"}
		}
		if err != nil {
			return err
		}
		if err := s.Repo.Save(ctx, a); err != nil {
			return fmt.Errorf("saving alert: %w", err)
		}
		out = View{Alert: a, ProjectName: acc.roles[a.ProjectID].Name, CanManage: true, CanSuggest: true}
		if out.Analysis, err = s.analysis(ctx, a.ID); err != nil {
			return err
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: kind, ProjectID: a.ProjectID,
			Subject: "alert:" + a.ID, Details: map[string]string{"kind": string(a.Kind), "severity": string(a.Severity)}})
	})
	return out, err
}

// PreferencesView is a user's notification settings and whether Telegram is linked (else nothing is sent).
type PreferencesView struct {
	Preferences    domain.Preferences
	TelegramLinked bool
}

// Preferences returns p's notification settings.
func (s *Service) Preferences(ctx context.Context, p auth.Principal) (PreferencesView, error) {
	prefs, err := s.Repo.Preferences(ctx, p.UserID)
	if err != nil {
		return PreferencesView{}, fmt.Errorf("loading preferences: %w", err)
	}
	_, linked, err := s.People.TelegramChat(ctx, p.UserID)
	if err != nil {
		return PreferencesView{}, fmt.Errorf("loading Telegram link: %w", err)
	}
	return PreferencesView{Preferences: prefs, TelegramLinked: linked}, nil
}

// SavePreferences replaces p's notification settings.
func (s *Service) SavePreferences(ctx context.Context, p auth.Principal, language string, disabled []domain.NotificationType) (PreferencesView, error) {
	prefs, err := domain.NewPreferences(p.UserID, language, disabled)
	if err != nil {
		return PreferencesView{}, err
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.SavePreferences(ctx, prefs, s.Now()); err != nil {
			return fmt.Errorf("saving preferences: %w", err)
		}
		off := make([]string, 0, len(prefs.Disabled))
		for _, t := range prefs.Disabled {
			off = append(off, string(t))
		}
		d := map[string]string{"language": string(prefs.Language), "disabled": strings.Join(off, ",")}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindPreferencesUpdated,
			Subject: "user:" + p.UserID, Details: d})
	})
	if err != nil {
		return PreferencesView{}, err
	}
	return s.Preferences(ctx, p)
}

// ApprovalRequest is a pending approval as the approvals context announces it.
type ApprovalRequest struct {
	ApprovalID, ProjectID, TaskID, Risk, Reason string
	Scope                                       string // execute or integrate_dev (KIN-106)
}

// NotifyApproval tells the Owners (the only ones who decide) about a pending approval. The message only opens
// the approval in the Mini App; deciding happens there after the Mini App sign-in, never from the chat.
func (s *Service) NotifyApproval(ctx context.Context, r ApprovalRequest) error {
	if r.ApprovalID == "" {
		return &domain.ValidationError{Field: "approvalId", Code: "required"}
	}
	// A late delivery (retry, or a dead message requeued at startup) must not ask for an approval already decided.
	if pending, err := s.Approvals.Pending(ctx, r.ApprovalID); err != nil {
		return fmt.Errorf("checking approval: %w", err)
	} else if !pending {
		return nil
	}
	project := ""
	if r.ProjectID != "" {
		name, err := s.Projects.Name(ctx, r.ProjectID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("loading project: %w", err)
		}
		project = name
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		owners, err := s.People.Owners(ctx)
		if err != nil {
			return fmt.Errorf("listing owners: %w", err)
		}
		n := domain.ApprovalNotice{ApprovalID: r.ApprovalID, TaskID: r.TaskID, Project: project, Risk: r.Risk, Reason: r.Reason,
			Scope: r.Scope}
		return s.send(ctx, owners, domain.NotifyApprovalRequested, "approval/"+r.ApprovalID, "approval_"+r.ApprovalID,
			func(l domain.Language) string { return domain.RenderApproval(l, n) })
	})
}

// Final task states announced by the workflow (KIN-62).
const (
	TaskCompleted = "COMPLETED"
	TaskFailed    = "FAILED"
)

// TaskFinished is a task that entered COMPLETED or FAILED, as the workflow announces it.
type TaskFinished struct {
	TaskID, ProjectID, State, Title, CreatedBy string
}

// NotifyTask tells a finished task's audience: the Owners, the project's admins and the task's author while they
// are still a member of the project (RBAC is re-checked here, at delivery). Each message is keyed by task, state
// and user, so a redelivered event never sends twice.
func (s *Service) NotifyTask(ctx context.Context, e TaskFinished) error {
	switch {
	case e.TaskID == "":
		return &domain.ValidationError{Field: "taskId", Code: "required"}
	case e.ProjectID == "":
		return &domain.ValidationError{Field: "projectId", Code: "required"}
	case e.State != TaskCompleted && e.State != TaskFailed:
		return &domain.ValidationError{Field: "state", Code: "invalid"}
	}
	project, err := s.Projects.Name(ctx, e.ProjectID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // the project is gone: nobody may see the task any more
	}
	if err != nil {
		return fmt.Errorf("loading project: %w", err)
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		users, err := s.People.Owners(ctx)
		if err != nil {
			return fmt.Errorf("listing owners: %w", err)
		}
		admins, err := s.Projects.Admins(ctx, e.ProjectID)
		if err != nil {
			return fmt.Errorf("listing admins: %w", err)
		}
		users = append(users, admins...)
		if e.CreatedBy != "" {
			members, err := s.Projects.Members(ctx, e.ProjectID)
			if err != nil {
				return fmt.Errorf("listing members: %w", err)
			}
			if slices.Contains(members, e.CreatedBy) {
				users = append(users, e.CreatedBy)
			}
		}
		n := domain.TaskNotice{TaskID: e.TaskID, Title: e.Title, Project: project, Failed: e.State == TaskFailed}
		return s.send(ctx, users, domain.NotifyTaskFinished, "task/"+e.TaskID+"/"+e.State, "task_"+e.TaskID,
			func(l domain.Language) string { return domain.RenderTask(l, n) })
	})
}

// notifyAlert tells the alert's audience: the Owners, plus the project's members for a project alert.
func (s *Service) notifyAlert(ctx context.Context, a domain.Alert) error {
	users, err := s.People.Owners(ctx)
	if err != nil {
		return fmt.Errorf("listing owners: %w", err)
	}
	if a.ProjectID != "" {
		ms, err := s.Projects.Members(ctx, a.ProjectID)
		if err != nil {
			return fmt.Errorf("listing members: %w", err)
		}
		users = append(users, ms...)
	}
	key := fmt.Sprintf("alert/%s/%d", a.ID, a.Occurrences)
	return s.send(ctx, users, domain.NotifyAlertRaised, key, "alert_"+a.ID,
		func(l domain.Language) string { return domain.RenderAlert(l, a) })
}

// send enqueues one Telegram message per linked recipient who wants type t, in the recipient's language. key
// makes every (notification, user) message idempotent.
func (s *Service) send(ctx context.Context, users []string, t domain.NotificationType, key, startParam string,
	text func(domain.Language) string) error {
	slices.Sort(users)
	for _, u := range slices.Compact(users) {
		chat, linked, err := s.People.TelegramChat(ctx, u)
		if err != nil {
			return fmt.Errorf("loading Telegram link: %w", err)
		}
		if !linked {
			continue
		}
		prefs, err := s.Repo.Preferences(ctx, u)
		if err != nil {
			return fmt.Errorf("loading preferences: %w", err)
		}
		if !prefs.Wants(t) {
			continue
		}
		b, err := json.Marshal(SendPayload{ChatID: chat, Text: text(prefs.Language),
			Button: domain.ButtonLabel(prefs.Language, t), StartParam: startParam})
		if err != nil {
			return err
		}
		if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterTelegram, Topic: TopicTelegramSend,
			Key: key + "/" + u, Payload: b}); err != nil {
			return fmt.Errorf("enqueueing notification: %w", err)
		}
	}
	return nil
}
