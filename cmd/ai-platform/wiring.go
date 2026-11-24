package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	audithttp "github.com/tr1gl1de/kindevai/internal/audit/adapters/httpapi"
	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/tokenfile"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	identity "github.com/tr1gl1de/kindevai/internal/identity/domain"
	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	messaginghttp "github.com/tr1gl1de/kindevai/internal/platform/messaging/httpapi"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	projectshttp "github.com/tr1gl1de/kindevai/internal/projects/adapters/httpapi"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	runnershttp "github.com/tr1gl1de/kindevai/internal/runners/adapters/httpapi"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
)

// messagingModule is the durable outbox/inbox. Bounded contexts register their handlers on Outbox/Inbox while
// being wired, before start runs the dispatchers.
type messagingModule struct {
	Outbox *messaging.Outbox
	Inbox  *messaging.Inbox
	HTTP   server.Module
}

func newMessaging(inst *bootstrap.Installation, logger *slog.Logger) *messagingModule {
	cfg := messaging.Config{Logger: logger}
	m := &messagingModule{Outbox: messaging.NewOutbox(inst.DB, cfg), Inbox: messaging.NewInbox(inst.DB, cfg)}
	m.HTTP = &messaginghttp.Handler{Outbox: m.Outbox, Inbox: m.Inbox, Logger: logger}
	return m
}

// start runs the dispatchers until ctx ends; the returned stop cancels them and waits, so call it before the
// database closes.
func (m *messagingModule) start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { _ = m.Outbox.Run(ctx) })
	wg.Go(func() { _ = m.Inbox.Run(ctx) })
	return func() { cancel(); wg.Wait() }
}

// auditModule is the append-only audit log. Other contexts record through Service inside their own transactions.
type auditModule struct {
	Service *auditapp.Service
	HTTP    server.Module
}

func newAudit(inst *bootstrap.Installation, logger *slog.Logger) *auditModule {
	svc := &auditapp.Service{Repo: auditdb.New(inst.DB), Now: time.Now}
	return &auditModule{Service: svc, HTTP: &audithttp.Handler{Service: svc, Logger: logger}}
}

// identityModule is the wired identity bounded context.
type identityModule struct {
	Modules    []server.Module
	Middleware func(http.Handler) http.Handler
	Users      app.AccountRepository // for other contexts, via the adapters below
	Hasher     *password.Hasher      // shared so the Argon2id concurrency cap is global
	Telegram   *app.TelegramService  // Mini App sign-in; also links Telegram on invitation acceptance
}

// newIdentity wires the identity bounded context and prepares setup mode.
func newIdentity(ctx context.Context, inst *bootstrap.Installation, audit *auditModule, logger *slog.Logger) (*identityModule, error) {
	hasher := password.New(password.DefaultParams)
	tokenPath := filepath.Join(inst.Dir.Path(), "secrets", "setup-token")
	setup := &app.SetupService{
		Repo:    sqlite.NewSetupRepository(inst.DB),
		Hasher:  hasher,
		Channel: tokenfile.New(tokenPath),
		Audit:   audit.Service,
		Now:     time.Now,
		Rand:    rand.Reader,
		NewID:   rand.Text,
		Logger:  logger,
	}
	required, err := setup.Prepare(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	if required {
		// The token itself is never logged; the operator reads it from the 0600 file on the host/container.
		logger.Warn("setup required: open the dashboard and enter the one-time setup token",
			"token_file", tokenPath, "show_with", "ai-platform setup-token", "valid_for", "1h, renewed when expired")
	}
	perMinute := func(burst int) *ratelimit.Limiter {
		return ratelimit.New(ratelimit.Config{Burst: burst, Every: time.Minute, MaxKeys: 10_000}, time.Now)
	}
	status := ratelimit.New(ratelimit.Config{Burst: 30, Every: time.Second, MaxKeys: 10_000}, time.Now)
	authn := &httpapi.AuthHandler{
		Service: &app.AuthService{
			Users:     sqlite.NewUserRepository(inst.DB),
			Sessions:  sqlite.NewSessionRepository(inst.DB),
			Passwords: hasher,
			Tx:        db.Transactor{DB: inst.DB},
			Audit:     audit.Service,
			Now:       time.Now,
			Rand:      rand.Reader,
			Logger:    logger,
		},
		IPLimiter:    perMinute(20),
		LoginLimiter: perMinute(10),
		Logger:       logger,
	}
	accounts := &httpapi.AccountsHandler{
		Service: &app.AccountsService{
			Accounts: sqlite.NewUserRepository(inst.DB),
			Sessions: sqlite.NewSessionRepository(inst.DB),
			Tx:       db.Transactor{DB: inst.DB},
			Audit:    audit.Service,
			Hasher:   hasher,
			Rand:     rand.Reader,
			Now:      time.Now,
			NewID:    rand.Text,
		},
		Limiter: perMinute(10),
		Logger:  logger,
	}
	telegram := &app.TelegramService{
		Repo:     sqlite.NewTelegramRepository(inst.DB),
		Vault:    botTokenVault{&integrationsapp.Vault{Store: integrationsdb.New(inst.DB), Cipher: inst.Box, Now: time.Now}},
		Users:    sqlite.NewUserRepository(inst.DB),
		Sessions: sqlite.NewSessionRepository(inst.DB),
		Tx:       db.Transactor{DB: inst.DB},
		Audit:    audit.Service,
		Now:      time.Now,
		Rand:     rand.Reader,
		NewID:    rand.Text,
		Logger:   logger,
	}
	return &identityModule{
		Modules: []server.Module{
			&httpapi.SetupHandler{Service: setup, Limiter: perMinute(10), StatusLimiter: status, Logger: logger},
			authn,
			accounts,
			&httpapi.TelegramHandler{Service: telegram, Limiter: perMinute(20), Logger: logger},
		},
		Telegram:   telegram,
		Middleware: authn.Middleware,
		Users:      sqlite.NewUserRepository(inst.DB),
		Hasher:     hasher,
	}, nil
}

// newProjects wires the projects bounded context, the identity invitations that grant its memberships and the
// runners context, whose runners are granted projects.
func newProjects(inst *bootstrap.Installation, id *identityModule, audit *auditModule, hub *runnersapp.Hub, logger *slog.Logger) []server.Module {
	users := id.Users
	svc := &projectsapp.Service{
		Repo:  projectsdb.New(inst.DB),
		Users: userDirectory{users},
		Tx:    db.Transactor{DB: inst.DB},
		Audit: audit.Service,
		Now:   time.Now,
		NewID: rand.Text,
	}
	invitations := &httpapi.InvitationsHandler{
		Service: &app.InvitationsService{
			Invitations: sqlite.NewInvitationRepository(inst.DB),
			Accounts:    users,
			Projects:    projectCatalog{svc},
			Tx:          db.Transactor{DB: inst.DB},
			Audit:       audit.Service,
			Hasher:      id.Hasher,
			Rand:        rand.Reader,
			Now:         time.Now,
			NewID:       rand.Text,
			Logger:      logger,
			Telegram:    id.Telegram,
		},
		Limiter:  ratelimit.New(ratelimit.Config{Burst: 10, Every: time.Minute, MaxKeys: 10_000}, time.Now),
		Logger:   logger,
		Telegram: id.Telegram,
	}
	runnerMgmt := &runnershttp.Handler{
		Service: &runnersapp.Service{
			Repo:     runnersdb.New(inst.DB),
			Projects: runnerProjects{svc},
			Tx:       db.Transactor{DB: inst.DB},
			Audit:    audit.Service,
			Rand:     rand.Reader,
			Now:      time.Now,
			NewID:    rand.Text,
			Logger:   logger,
			Sessions: hub, // disable/revoke close the runner's live gateway connection
		},
		Limiter: ratelimit.New(ratelimit.Config{Burst: 10, Every: time.Minute, MaxKeys: 10_000}, time.Now),
		Logger:  logger,
	}
	return []server.Module{&projectshttp.Handler{Service: svc, Logger: logger}, invitations, runnerMgmt}
}

// runnerProjects gives the runners context the projects context's app API, translating its errors.
type runnerProjects struct{ svc *projectsapp.Service }

func (c runnerProjects) Lookup(ctx context.Context, id string) (runnersapp.ProjectRef, error) {
	p, err := c.svc.Lookup(ctx, id)
	if errors.Is(err, projects.ErrNotFound) {
		return runnersapp.ProjectRef{}, runners.ErrNotFound
	}
	if err != nil {
		return runnersapp.ProjectRef{}, err
	}
	return runnersapp.ProjectRef{ID: p.ID, Name: string(p.Name)}, nil
}

// projectCatalog gives identity's invitations the projects context's app API, translating its errors.
type projectCatalog struct{ svc *projectsapp.Service }

func (c projectCatalog) Lookup(ctx context.Context, id string) (app.ProjectRef, error) {
	p, err := c.svc.Lookup(ctx, id)
	if errors.Is(err, projects.ErrNotFound) {
		return app.ProjectRef{}, identity.ErrNotFound
	}
	if err != nil {
		return app.ProjectRef{}, err
	}
	return app.ProjectRef{ID: p.ID, Name: string(p.Name)}, nil
}

func (c projectCatalog) ValidateRole(role string) error {
	var ve *projects.ValidationError
	if err := c.svc.ValidateRole(role); errors.As(err, &ve) {
		return &identity.ValidationError{Field: ve.Field, Code: ve.Code}
	} else if err != nil {
		return err
	}
	return nil
}

func (c projectCatalog) Grant(ctx context.Context, projectID, userID, role string) error {
	err := c.svc.Grant(ctx, projectID, userID, role)
	if errors.Is(err, projects.ErrNotFound) {
		return identity.ErrNotFound
	}
	return err
}

// userDirectory lets the projects context resolve accounts through the identity context's port, so it never
// reads identity's tables itself.
type userDirectory struct{ users app.UserRepository }

func (d userDirectory) ByLogin(ctx context.Context, login string) (projectsapp.UserRef, error) {
	l, err := identity.NewLogin(login)
	if err != nil {
		return projectsapp.UserRef{}, projects.ErrUserNotFound
	}
	u, err := d.users.ByLogin(ctx, l)
	if errors.Is(err, identity.ErrNotFound) {
		return projectsapp.UserRef{}, projects.ErrUserNotFound
	}
	if err != nil {
		return projectsapp.UserRef{}, err
	}
	return userRef(u), nil
}

func userRef(u identity.User) projectsapp.UserRef {
	return projectsapp.UserRef{ID: u.ID, Login: string(u.Login), Owner: u.Role == identity.RoleOwner, Disabled: u.Disabled}
}

func (d userDirectory) ByIDs(ctx context.Context, ids []string) (map[string]projectsapp.UserRef, error) {
	out := make(map[string]projectsapp.UserRef, len(ids))
	for _, id := range ids {
		u, err := d.users.ByID(ctx, id)
		if errors.Is(err, identity.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = userRef(u)
	}
	return out, nil
}
