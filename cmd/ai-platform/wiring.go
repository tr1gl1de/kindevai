package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/tokenfile"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/server"
)

// identityModule is the wired identity bounded context.
type identityModule struct {
	Modules    []server.Module
	Middleware func(http.Handler) http.Handler
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
	}, nil
}
