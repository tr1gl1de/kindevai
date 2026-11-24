package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

	alertsevents "github.com/tr1gl1de/kindevai/internal/alerts/adapters/events"
	alertshttp "github.com/tr1gl1de/kindevai/internal/alerts/adapters/httpapi"
	alertsdb "github.com/tr1gl1de/kindevai/internal/alerts/adapters/sqlite"
	alertstg "github.com/tr1gl1de/kindevai/internal/alerts/adapters/telegram"
	alertsapp "github.com/tr1gl1de/kindevai/internal/alerts/app"
	alerts "github.com/tr1gl1de/kindevai/internal/alerts/domain"
	approvalsdb "github.com/tr1gl1de/kindevai/internal/approvals/adapters/sqlite"
	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	approvals "github.com/tr1gl1de/kindevai/internal/approvals/domain"
	identitydb "github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	identityapp "github.com/tr1gl1de/kindevai/internal/identity/app"
	identity "github.com/tr1gl1de/kindevai/internal/identity/domain"
	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflownotify "github.com/tr1gl1de/kindevai/internal/workflow/adapters/notify"
)

// newAlerts wires the Alert Center (KIN-40): the monitors' and approvals' outbox events become alerts and
// Telegram notifications; telegram.send delivers them with the Bot API at telegramAPI ("" = api.telegram.org).
// Call it before msg.start; dead messages that waited for these handlers are requeued by msg.start's caller.
func newAlerts(inst *bootstrap.Installation, msg *messagingModule, audit *auditModule, users identityapp.AccountRepository,
	telegramAPI string, logger *slog.Logger) server.Module {
	projectsSvc := &projectsapp.Service{Repo: projectsdb.New(inst.DB), Users: userDirectory{users}}
	tgRepo := identitydb.NewTelegramRepository(inst.DB)
	svc := &alertsapp.Service{
		Repo:      alertsdb.New(inst.DB),
		Projects:  alertProjects{projectsSvc},
		People:    alertPeople{users: users, telegram: tgRepo},
		Approvals: pendingApprovals{approvalsdb.New(inst.DB)},
		Tx:        db.Transactor{DB: inst.DB},
		Audit:     audit.Service,
		Outbox:    msg.Outbox,
		Now:       time.Now,
		NewID:     rand.Text,
	}
	ev := alertsevents.Handlers{Svc: svc}
	msg.Outbox.Handle(runnersapp.TopicRunnerUnreachable, ev.RunnerUnreachable)
	msg.Outbox.Handle(runnersapp.TopicRunnerRecovered, ev.RunnerRecovered)
	msg.Outbox.Handle(runnersapp.TopicStorageLevel, ev.StorageLevel)
	msg.Outbox.Handle(runnersapp.TopicRunnerOutputFull, ev.RunnerOutputFull)           // KIN-59
	msg.Outbox.Handle(runnersapp.TopicRunnerOutputRecovered, ev.RunnerOutputRecovered) // KIN-59
	msg.Outbox.Handle(approvalsapp.TopicApprovalRequested, ev.ApprovalRequested)
	msg.Outbox.Handle(workflownotify.TopicTaskFinished, ev.TaskFinished) // KIN-62
	bot := alertBot{
		vault:    botTokenVault{&integrationsapp.Vault{Store: integrationsdb.New(inst.DB), Cipher: inst.Box, Now: time.Now}},
		settings: tgRepo,
	}
	msg.Outbox.Handle(alertsapp.TopicTelegramSend, (&alertstg.Sender{Bot: bot, APIBase: telegramAPI, Logger: logger}).Deliver)
	return &alertshttp.Handler{Service: svc, Logger: logger}
}

// alertProjects gives the alerts context the projects context's app API.
type alertProjects struct{ svc *projectsapp.Service }

func (c alertProjects) Visible(ctx context.Context, p auth.Principal) ([]alertsapp.ProjectRef, error) {
	ps, err := c.svc.List(ctx, projects.Actor{UserID: p.UserID, Login: p.Login, Owner: p.Owner})
	if err != nil {
		return nil, err
	}
	out := make([]alertsapp.ProjectRef, 0, len(ps))
	for _, v := range ps {
		out = append(out, alertsapp.ProjectRef{ID: v.Project.ID, Name: string(v.Project.Name), Role: v.MyRole})
	}
	return out, nil
}

func (c alertProjects) Name(ctx context.Context, id string) (string, error) {
	p, err := c.svc.Lookup(ctx, id)
	if errors.Is(err, projects.ErrNotFound) {
		return "", alerts.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(p.Name), nil
}

// system is the notifier reading memberships on behalf of the platform.
var system = projects.Actor{Login: "system", Owner: true}

func (c alertProjects) Members(ctx context.Context, id string) ([]string, error) {
	ms, err := c.svc.Members(ctx, system, id)
	if errors.Is(err, projects.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.UserID)
	}
	return out, nil
}

// Admins lists the user ids of the project's admins.
func (c alertProjects) Admins(ctx context.Context, id string) ([]string, error) {
	ms, err := c.svc.Members(ctx, system, id)
	if errors.Is(err, projects.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range ms {
		if m.Role == alertsapp.RoleProjectAdmin {
			out = append(out, m.UserID)
		}
	}
	return out, nil
}

// alertPeople gives the notifier the Owners and the linked Telegram accounts of enabled users.
type alertPeople struct {
	users    identityapp.AccountRepository
	telegram *identitydb.TelegramRepository
}

func (c alertPeople) Owners(ctx context.Context) ([]string, error) {
	us, err := c.users.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range us {
		if u.Role == identity.RoleOwner && !u.Disabled {
			out = append(out, u.ID)
		}
	}
	return out, nil
}

func (c alertPeople) TelegramChat(ctx context.Context, userID string) (int64, bool, error) {
	u, err := c.users.ByID(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if u.Disabled {
		return 0, false, nil
	}
	id, err := c.telegram.IdentityByUser(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id.TelegramID, true, nil // a private chat's id is the user's Telegram id
}

// alertBot gives the Telegram sender the bot token (vault slot telegram.bot_token) and the Mini App links.
type alertBot struct {
	vault    botTokenVault
	settings *identitydb.TelegramRepository
}

func (b alertBot) Token(ctx context.Context) (string, error) {
	t, err := b.vault.BotToken(ctx)
	if errors.Is(err, identity.ErrNotFound) {
		return "", alertstg.ErrNotConfigured
	}
	return t, err
}

func (b alertBot) MiniAppLink(ctx context.Context, startParam string) (string, bool, error) {
	s, _, err := b.settings.Settings(ctx)
	if errors.Is(err, identity.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return s.LinkURL(startParam), true, nil
}

// pendingApprovals tells the notifier whether an approval still waits for a decision.
type pendingApprovals struct{ repo *approvalsdb.Repository }

func (p pendingApprovals) Pending(ctx context.Context, id string) (bool, error) {
	a, err := p.repo.Get(ctx, id)
	if errors.Is(err, approvals.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.State == approvals.StatePending, nil
}
