// Command ai-platform is the AI Development Platform Control Plane.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
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
  setup-token  print the one-time first-run setup token (needs access to the data directory)
  export    write a consistent backup archive of the installation (works while serving)
  import    restore a backup archive into an empty data directory
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
	case "setup-token":
		return setupToken(args[1:], stdout, stderr)
	case "health":
		return health(ctx, args[1:], stderr)
	case "export":
		return exportCmd(ctx, args[1:], stdout, stderr)
	case "import":
		return importCmd(ctx, args[1:], stdout, stderr)
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
	dataDir := fset.String("data-dir", defaultDataDir(), "data directory (SQLite, keys, artifacts); created if missing")
	keyFile := fset.String("key-file", "", "encryption key file (default <data-dir>/secrets/master.key)")
	proxies := fset.String("trusted-proxies", os.Getenv("KINDEV_TRUSTED_PROXIES"),
		"comma-separated CIDRs of reverse proxies whose X-Forwarded-For is trusted (e.g. the Caddy network)")
	plainPeers := fset.String("runner-plain-ws-peers", os.Getenv("KINDEV_RUNNER_PLAIN_WS_PEERS"),
		"comma-separated CIDRs treated as this host for plain ws:// runner connections besides loopback "+
			"(e.g. the Docker network when the port is published on 127.0.0.1 only); remote runners need wss://")
	publicURL := fset.String("public-url", os.Getenv("KINDEV_PUBLIC_URL"),
		"comma-separated public origins of the Control Plane (e.g. https://kindevai.example.com); when set, runner "+
			"connections for any other host are refused")
	githubAPI := fset.String("github-api-url", envOr("KINDEV_GITHUB_API_URL", github.DefaultBaseURL),
		"GitHub REST API base URL (GitHub Enterprise Server: https://<host>/api/v3)")
	retFlags := addRetentionFlags(fset, os.Getenv) // KIN-52
	if err := fset.Parse(args); err != nil {
		return err
	}
	retention, err := retFlags.policy()
	if err != nil {
		return err
	}
	audiences, err := parseAudiences(*publicURL)
	if err != nil {
		return err
	}
	ghURL, err := integrations.NewBaseURL(*githubAPI)
	if err != nil {
		return fmt.Errorf("--github-api-url: %w", err)
	}
	trusted, err := httpx.ParseTrustedProxies(*proxies)
	if err != nil {
		return err
	}
	localPeers, err := httpx.ParseTrustedProxies(*plainPeers)
	if err != nil {
		return fmt.Errorf("runner-plain-ws-peers: %w", err)
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

	audit := newAudit(inst, logger)
	identity, err := newIdentity(ctx, inst, audit, logger)
	if err != nil {
		return err
	}
	msg := newMessaging(inst, logger)
	hub := runnersapp.NewHub()
	tel := newTelemetry(inst, msg, logger)
	modules := append(identity.Modules, newProjects(inst, identity, audit, hub, tel, logger)...)
	integ := newIntegrations(inst, msg, audit, ghURL, logger)
	intake := newYouTrackIntake(inst, msg, integ, identity, logger)
	apv := newApprovals(inst, msg, audit, integ, logger)
	modules = append(modules, msg.HTTP, audit.HTTP, integ.HTTP, intake.HTTP, apv.HTTP, apv.Tasks)
	modules = append(modules, newAlerts(inst, msg, audit, identity.Users, "", logger)) // KIN-40
	gateway := newRunnerGateway(inst, hub, tel, localPeers, audiences, logger)
	channel := newRunnerChannel(inst, apv.Workflow, retention, logger)
	attachRunnerChannel(gateway, channel)
	// KIN-59: stored runner output in the Storage Overview, "output storage full" alerts.
	reportOutputStorage(inst, msg, tel, channel, retention)
	wireExecution(apv.Workflow, channel, gateway, tel, hub, logger)    // KIN-23 scheduler, leases, reconciliation
	wireAttemptLog(apv.Workflow, inst)                                 // KIN-25 attempt events in the task UI
	wireRunnerRelease(modules, apv.Workflow)                           // KIN-63 disable/revoke end the runner's attempts
	netSvc, netHTTP := newNetwork(inst, audit, integ, channel, logger) // KIN-30 network profiles
	netSvc.Outbox = msg.Outbox                                         // KIN-60 egress check alerts
	attachCheckoutSource(channel, integ.GitHub)                        // KIN-29 worktree base + repository token
	attachScopeCheck(channel, apv.Approvals, apv.Workflow)             // KIN-51 changed files vs the approved scope
	wirePlanner(apv.Workflow, inst, msg, integ)                        // KIN-34 planner runs and their context package
	intake.planWith(apv.Workflow)                                      // KIN-85 intake tasks auto-planned (per project)
	attachNetwork(gateway, netSvc, logger)
	wireStrictEgress(apv.Workflow, netSvc.StrictFor) // KIN-31 strict steps only on verified runners
	modules = append(modules, netHTTP)
	_, routingHTTP := newRouting(inst, audit, apv.Workflow, logger) // KIN-33 CLI switches, role bindings, routing
	modules = append(modules, routingHTTP)
	maint, err := newMaintenance(ctx, inst, audit, apv.Workflow, logger) // KIN-41 maintenance mode
	if err != nil {
		return err
	}
	modules = append(modules, maint)
	modules = append(modules, gateway)
	defer msg.start(ctx)()    // deferred after inst.Close, so the dispatchers stop before the database closes
	defer intake.start(ctx)() // the YouTrack poller stops first
	defer startTelemetry(ctx, tel, inst.Dir.Path())()
	defer startRetention(ctx, inst, apv.Workflow, retention, logger)() // KIN-52
	defer startAlertMonitors(ctx, inst, msg, logger)()                 // KIN-60
	defer startScheduler(ctx, apv.Workflow, DefaultSchedulerInterval, logger)()

	assets, built := web.Assets()
	if !built {
		logger.Warn("web UI not built into this binary; serving placeholder page")
	}
	srv := &http.Server{
		Handler: server.New(server.Options{
			Version: version, Assets: assets, Logger: logger, Modules: modules,
			Middleware: func(h http.Handler) http.Handler { return trusted.Middleware(identity.Middleware(maint.ReadOnly(h))) },
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	srv.RegisterOnShutdown(hub.Shutdown) // hijacked runner WebSockets are not closed by srv.Shutdown itself
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
	var lostErr error
	select {
	case err := <-errc:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	case <-inst.Lost(): // KIN-41: another instance owns the database now; stop writing to it
		logger.Error("instance record lost: another Control Plane instance may be using this database; stopping")
		lostErr = bootstrap.ErrInstanceActive
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	// Runner WebSockets are hijacked, so Shutdown does not wait for them; the hub already sent them "going away".
	if err := gateway.Wait(shutdownCtx); err != nil {
		logger.Warn("runner connections still open at shutdown", "count", gateway.Live())
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return lostErr
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

// setupToken prints the current setup token. Reading the data directory is the trusted local channel, so this
// works on the host (`ai-platform setup-token`) and in the distroless container (`docker compose exec cp
// ai-platform setup-token`), which has no shell or cat.
func setupToken(args []string, stdout, stderr io.Writer) error {
	fset := flag.NewFlagSet("setup-token", flag.ContinueOnError)
	fset.SetOutput(stderr)
	dataDir := fset.String("data-dir", defaultDataDir(), "data directory")
	if err := fset.Parse(args); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(*dataDir, "secrets", "setup-token"))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("no setup token: setup is completed, or the Control Plane has not started yet")
	}
	if err != nil {
		return fmt.Errorf("reading setup token: %w", err)
	}
	_, err = fmt.Fprintln(stdout, strings.TrimSpace(string(b)))
	return err
}

// defaultDataDir is /data inside the container image, ./data elsewhere.
func defaultDataDir() string {
	if v := os.Getenv("KINDEV_DATA_DIR"); v != "" {
		return v
	}
	return "./data"
}

// envOr returns the environment variable, or def when it is unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
