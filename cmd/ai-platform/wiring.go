package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/tokenfile"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	identity "github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	projectshttp "github.com/tr1gl1de/kindevai/internal/projects/adapters/httpapi"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
)

// identityModule is the wired identity bounded context.
type identityModule struct {
	Modules    []server.Module
	Middleware func(http.Handler) http.Handler
	Users      app.AccountRepository // for other contexts, via the adapters below
	Hasher     *password.Hasher      // shared so the Argon2id concurrency cap is global
}

// newIdentity wires the identity bounded context and prepares setup mode.
func newIdentity(ctx context.Context, inst *bootstrap.Installation, logger *slog.Logger) (*identityModule, error) {
	hasher := password.New(password.DefaultParams)
	tokenPath := filepath.Join(inst.Dir.Path(), "secrets", "setup-token")
	setup := &app.SetupService{
		Repo:    sqlite.NewSetupRepository(inst.DB),
		Hasher:  hasher,
		Channel: tokenfile.New(tokenPath),
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
			Hasher:   hasher,
			Rand:     rand.Reader,
			Now:      time.Now,
			NewID:    rand.Text,
		},
		Limiter: perMinute(10),
		Logger:  logger,
	}
	return &identityModule{
		Modules: []server.Module{
			&httpapi.SetupHandler{Service: setup, Limiter: perMinute(10), StatusLimiter: status, Logger: logger},
			authn,
			accounts,
		},
		Middleware: authn.Middleware,
		Users:      sqlite.NewUserRepository(inst.DB),
		Hasher:     hasher,
	}, nil
}

// newProjects wires the projects bounded context and the identity invitations that grant its memberships.
func newProjects(inst *bootstrap.Installation, id *identityModule, logger *slog.Logger) []server.Module {
	users := id.Users
	svc := &projectsapp.Service{
		Repo:  projectsdb.New(inst.DB),
		Users: userDirectory{users},
		Now:   time.Now,
		NewID: rand.Text,
	}
	invitations := &httpapi.InvitationsHandler{
		Service: &app.InvitationsService{
			Invitations: sqlite.NewInvitationRepository(inst.DB),
			Accounts:    users,
			Projects:    projectCatalog{svc},
			Tx:          db.Transactor{DB: inst.DB},
			Hasher:      id.Hasher,
			Rand:        rand.Reader,
			Now:         time.Now,
			NewID:       rand.Text,
			Logger:      logger,
		},
		Limiter: ratelimit.New(ratelimit.Config{Burst: 10, Every: time.Minute, MaxKeys: 10_000}, time.Now),
		Logger:  logger,
	}
	return []server.Module{&projectshttp.Handler{Service: svc, Logger: logger}, invitations}
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
