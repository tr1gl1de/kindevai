package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"sync"
	"time"

	identityapp "github.com/tr1gl1de/kindevai/internal/identity/app"
	identity "github.com/tr1gl1de/kindevai/internal/identity/domain"
	integrationshttp "github.com/tr1gl1de/kindevai/internal/integrations/adapters/httpapi"
	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// intakeModule is the YouTrack intake (KIN-27): the poller, its inbox/outbox handlers and its HTTP API.
type intakeModule struct {
	Intake *integrationsapp.Intake
	HTTP   server.Module
}

// newYouTrackIntake wires the intake and registers its inbox/outbox handlers; call it before msg.start.
func newYouTrackIntake(inst *bootstrap.Installation, msg *messagingModule, integ *integrationsModule, id *identityModule, logger *slog.Logger) *intakeModule {
	projectsSvc := &projectsapp.Service{Repo: projectsdb.New(inst.DB), Users: userDirectory{id.Users}}
	wf := &workflowapp.Service{
		Repo:     workflowdb.New(inst.DB),
		Projects: workflowProjects{projects: projectsSvc, integrations: integ.Service},
		Tx:       db.Transactor{DB: inst.DB},
		Now:      time.Now,
		NewID:    rand.Text,
	}
	in := &integrationsapp.Intake{
		Integrations: integ.Service,
		Store:        integrationsdb.New(inst.DB),
		Tasks:        intakeTasks{wf},
		Access:       projectsSvc,
		Users:        intakeUsers{id.Users},
		Source:       &youtrack.Client{},
		Inbox:        msg.Inbox,
		Health:       msg.Outbox.Health(),
	}
	msg.Inbox.Handle(integrationsapp.TopicYouTrackIssue, in.HandleIssue)
	msg.Inbox.Handle(integrationsapp.TopicYouTrackWebhook, in.HandleWebhook)
	msg.Outbox.Handle(integrationsapp.TopicYouTrackFetch, in.HandleFetch)
	perMinute := func(burst int) *ratelimit.Limiter {
		return ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Minute, MaxKeys: 10_000}, time.Now)
	}
	return &intakeModule{Intake: in, HTTP: &integrationshttp.IntakeHandler{Intake: in, WebhookLimiter: perMinute(120),
		RunLimiter: perMinute(20), Logger: logger}}
}

// start runs the poller until ctx ends; the returned func stops it and waits.
func (m *intakeModule) start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { m.Intake.Run(ctx) })
	return func() { cancel(); wg.Wait() }
}

// intakeTasks gives the intake the workflow context's app API.
type intakeTasks struct{ wf *workflowapp.Service }

func (t intakeTasks) TaskForIssue(ctx context.Context, projectID, issueID string) (string, bool, error) {
	task, err := t.wf.TaskForIssue(ctx, projectID, issueID)
	if errors.Is(err, workflow.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return task.ID, true, nil
}

func (t intakeTasks) CreateIssueTask(ctx context.Context, projectID, issueID, title, createdBy string) (string, error) {
	task, err := t.wf.CreateTask(ctx, projectID, title, issueID, createdBy)
	return task.ID, err
}

func (t intakeTasks) RetitleTask(ctx context.Context, taskID, title string) (bool, error) {
	return t.wf.RetitleTask(ctx, taskID, title)
}

// workflowProjects gives the workflow context the projects it may plan for, with their registered repositories.
type workflowProjects struct {
	projects     *projectsapp.Service
	integrations *integrationsapp.Service
}

func (w workflowProjects) Repositories(ctx context.Context, projectID string) ([]string, error) {
	if _, err := w.projects.Lookup(ctx, projectID); errors.Is(err, projects.ErrNotFound) {
		return nil, workflow.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rs, err := w.integrations.RepositoriesOf(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.FullName())
	}
	return out, nil
}

// intakeUsers resolves accounts for the intake through the identity context's port.
type intakeUsers struct{ users identityapp.UserRepository }

func (u intakeUsers) ByLogin(ctx context.Context, login string) (integrationsapp.UserRef, error) {
	l, err := identity.NewLogin(login)
	if err != nil {
		return integrationsapp.UserRef{}, integrations.ErrNotFound
	}
	usr, err := u.users.ByLogin(ctx, l)
	if errors.Is(err, identity.ErrNotFound) {
		return integrationsapp.UserRef{}, integrations.ErrNotFound
	}
	if err != nil {
		return integrationsapp.UserRef{}, err
	}
	return integrationsapp.UserRef{ID: usr.ID, Login: string(usr.Login)}, nil
}

func (u intakeUsers) ByIDs(ctx context.Context, ids []string) (map[string]integrationsapp.UserRef, error) {
	out := make(map[string]integrationsapp.UserRef, len(ids))
	for _, id := range ids {
		if _, seen := out[id]; seen {
			continue
		}
		usr, err := u.users.ByID(ctx, id)
		if errors.Is(err, identity.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = integrationsapp.UserRef{ID: usr.ID, Login: string(usr.Login)}
	}
	return out, nil
}
