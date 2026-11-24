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
	Users      app.UserRepository // read access for other contexts, via directory adapters below
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
			"token_file", tokenPath, "valid_for", "1h, renewed when expired")
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
	return &identityModule{
		Modules: []server.Module{
			&httpapi.SetupHandler{Service: setup, Limiter: perMinute(10), StatusLimiter: status, Logger: logger},
			authn,
		},
		Middleware: authn.Middleware,
		Users:      sqlite.NewUserRepository(inst.DB),
	}, nil
}

// newProjects wires the projects bounded context.
func newProjects(inst *bootstrap.Installation, users app.UserRepository, logger *slog.Logger) server.Module {
	svc := &projectsapp.Service{
		Repo:  projectsdb.New(inst.DB),
		Users: userDirectory{users},
		Now:   time.Now,
		NewID: rand.Text,
	}
	return &projectshttp.Handler{Service: svc, Logger: logger}
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
	if errors.Is(err, identity.ErrNotFound) || (err == nil && u.Disabled) {
		return projectsapp.UserRef{}, projects.ErrUserNotFound
	}
	if err != nil {
		return projectsapp.UserRef{}, err
	}
	return projectsapp.UserRef{ID: u.ID, Login: string(u.Login), Owner: u.Role == identity.RoleOwner}, nil
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
		out[id] = projectsapp.UserRef{ID: u.ID, Login: string(u.Login), Owner: u.Role == identity.RoleOwner}
	}
	return out, nil
}
