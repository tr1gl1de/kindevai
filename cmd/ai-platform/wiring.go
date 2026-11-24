package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
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

// newIdentity wires the identity bounded context and prepares setup mode.
func newIdentity(ctx context.Context, inst *bootstrap.Installation, logger *slog.Logger) ([]server.Module, error) {
	tokenPath := filepath.Join(inst.Dir.Path(), "secrets", "setup-token")
	setup := &app.SetupService{
		Repo:    sqlite.NewSetupRepository(inst.DB),
		Hasher:  password.New(password.DefaultParams),
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
	attempts := ratelimit.New(ratelimit.Config{Burst: 10, Every: time.Minute, MaxKeys: 10_000}, time.Now)
	status := ratelimit.New(ratelimit.Config{Burst: 30, Every: time.Second, MaxKeys: 10_000}, time.Now)
	return []server.Module{&httpapi.SetupHandler{
		Service: setup, Limiter: attempts, StatusLimiter: status, Logger: logger,
	}}, nil
}
