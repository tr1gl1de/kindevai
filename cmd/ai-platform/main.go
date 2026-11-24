// Command ai-platform is the AI Development Platform Control Plane.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/server"
	"github.com/tr1gl1de/kindevai/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "ai-platform:", err)
		os.Exit(1)
	}
}

const usage = `usage: ai-platform <command> [flags]

commands:
  serve     run the Control Plane (default 127.0.0.1:8080, data in ./data)
  health    check a running Control Plane (used by the Docker HEALTHCHECK)
  version   print the version
`

// run executes a subcommand. ready, if non-nil, receives the bound address once serving.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready chan<- net.Addr) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr, ready)
	case "health":
		return health(ctx, args[1:], stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context, args []string, stderr io.Writer, ready chan<- net.Addr) error {
	fset := flag.NewFlagSet("serve", flag.ContinueOnError)
	fset.SetOutput(stderr)
	addr := fset.String("addr", "127.0.0.1:8080", "listen address")
	dataDir := fset.String("data-dir", "./data", "data directory (SQLite, keys, artifacts); created if missing")
	keyFile := fset.String("key-file", "", "encryption key file (default <data-dir>/secrets/master.key)")
	if err := fset.Parse(args); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))

	inst, err := bootstrap.Run(ctx, bootstrap.Options{DataDir: *dataDir, KeyFile: *keyFile})
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer func() {
		if cerr := inst.Close(); cerr != nil {
			logger.Error("closing installation", "err", cerr)
		}
	}()
	if inst.Fresh {
		logger.Info("new installation created", "data_dir", inst.Dir.Path(), "installation_id", inst.ID)
	} else {
		logger.Info("installation loaded", "data_dir", inst.Dir.Path(), "installation_id", inst.ID)
	}

	identity, err := newIdentity(ctx, inst, logger)
	if err != nil {
		return err
	}

	assets, built := web.Assets()
	if !built {
		logger.Warn("web UI not built into this binary; serving placeholder page")
	}
	srv := &http.Server{
		Handler: server.New(server.Options{
			Version: version, Assets: assets, Logger: logger, Modules: append(identity.Modules, newProjects(inst, identity.Users, logger)),
			Middleware: identity.Middleware,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}
	logger.Info("control plane listening", "addr", ln.Addr().String(), "version", version)
	if ready != nil {
		ready <- ln.Addr()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// health exits non-zero unless the Control Plane answers 200 on its health endpoint.
func health(ctx context.Context, args []string, stderr io.Writer) error {
	fset := flag.NewFlagSet("health", flag.ContinueOnError)
	fset.SetOutput(stderr)
	url := fset.String("url", "http://127.0.0.1:8080/api/health", "health endpoint")
	timeout := fset.Duration("timeout", 3*time.Second, "request timeout")
	if err := fset.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: status %d", resp.StatusCode)
	}
	return nil
}
