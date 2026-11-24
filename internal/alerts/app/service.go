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
	Create(ctx context.Context, a domain.Alert) error
	Save(ctx context.Context, a domain.Alert) error
	Get(ctx context.Context, id string) (domain.Alert, error)                   // domain.ErrNotFound
	List(ctx context.Context, q ListQuery) ([]domain.Alert, error)              // newest activity first
	Preferences(ctx context.Context, userID string) (domain.Preferences, error) // defaults when never saved
	SavePreferences(ctx context.Context, p domain.Preferences, now time.Time) error
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
}

// People is the identity context as the notifier needs it.
type People interface {
	Owners(ctx context.Context) ([]string, error) // enabled platform Owners' user ids
	// TelegramChat returns the linked Telegram user id (the private chat with the bot) of an enabled user.
	TelegramChat(ctx context.Context, userID string) (int64, bool, error)
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
	Repo     Repository
	Projects Projects
	People   People
	Tx       TxRunner
	Audit    AuditLog
	Outbox   Outbox
	Now      func() time.Time
	NewID    func() string
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
		escalated := a.Recur(o, now)
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
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		a, err := s.Repo.Active(ctx, dedupKey)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loading alert: %w", err)
		}
		if err := a.Resolve("", s.Now()); err != nil {
			return err
		}
		return s.Repo.Save(ctx, a)
	})
}

// View is an alert as a user sees it.
type View struct {
	Alert       domain.Alert
	ProjectName string
	CanManage   bool
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
	out := make([]View, 0, len(as))
	for _, a := range as {
		if acc.sees(a) { // defence in depth: the query already filters
			out = append(out, View{Alert: a, ProjectName: acc.roles[a.ProjectID].Name, CanManage: acc.manages(a)})
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
		out = View{Alert: a, ProjectName: acc.roles[a.ProjectID].Name, CanManage: true}
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
}

// NotifyApproval tells the Owners (the only ones who decide) about a pending approval. The message only opens
// the approval in the Mini App; deciding happens there after the Mini App sign-in, never from the chat.
func (s *Service) NotifyApproval(ctx context.Context, r ApprovalRequest) error {
	if r.ApprovalID == "" {
		return &domain.ValidationError{Field: "approvalId", Code: "required"}
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
		n := domain.ApprovalNotice{ApprovalID: r.ApprovalID, TaskID: r.TaskID, Project: project, Risk: r.Risk, Reason: r.Reason}
		return s.send(ctx, owners, domain.NotifyApprovalRequested, "approval/"+r.ApprovalID, "approval_"+r.ApprovalID,
			func(l domain.Language) string { return domain.RenderApproval(l, n) })
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
