// Command ai-runner is the kindevai Runner daemon: it holds the machine's Ed25519 identity, enrolls with a
// Control Plane, keeps an outbound WSS connection to it and (later) runs AI CLIs in git worktrees.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/configfile"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/controlplane"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/keyfile"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/probe"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/spool"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const maxTokenInput = 4 << 10

// env is the process environment the commands use; tests substitute it.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// readSecret prompts on the terminal without echo; nil when stdin is not a terminal.
	readSecret     func(prompt string) (string, error)
	defaultDataDir string
	hostname       func() (string, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	home, _ := os.UserHomeDir()
	e := &env{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, hostname: os.Hostname,
		defaultDataDir: datadir.Default(os.Geteuid(), home, os.Getenv("XDG_DATA_HOME"))}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) { //nolint:gosec // G115: fd fits int
		e.readSecret = func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
	}
	err := run(ctx, os.Args[1:], e)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ai-runner:", err)
		os.Exit(exitCode(err))
	}
}

// exitConfig (EX_CONFIG) marks failures a restart cannot fix (not enrolled, insecure or mismatched key, revoked
// runner, untrusted TLS certificate, unsupported protocol); the service units do not restart on it.
const exitConfig = 78

func exitCode(err error) int {
	for _, target := range []error{domain.ErrNotEnrolled, domain.ErrFingerprintMismatch, domain.ErrInvalidKey,
		domain.ErrServerURL, domain.ErrPlainHTTP, app.ErrNoKey, secfile.ErrInsecure, domain.ErrRunnerRevoked,
		domain.ErrUnsupportedProtocol, domain.ErrTLSVerification, domain.ErrAudienceMismatch} {
		if errors.Is(err, target) {
			return exitConfig
		}
	}
	return 1
}

const usage = `usage: ai-runner <command> [flags]

commands:
  enroll       register this machine with a Control Plane using a one-time token:
               ai-runner enroll --server https://kindevai.example.com
               the token is read from the prompt, stdin or --token-file, never from arguments
  fingerprint  print the key fingerprint to compare with the runner's card in Dashboard -> Runners
  run          run the daemon (systemd/launchd services start this)
  version      print the version

common flags:
  --data-dir   runner data directory (default /var/lib/kindevai-runner as root,
               ~/.local/share/kindevai-runner otherwise)
`

func run(ctx context.Context, args []string, e *env) error {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "enroll":
		return enroll(ctx, args[1:], e)
	case "fingerprint":
		return fingerprint(ctx, args[1:], e)
	case "run":
		return daemon(ctx, args[1:], e)
	case "version":
		fmt.Fprintf(e.stdout, "ai-runner %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, usage)
		return nil
	}
	fmt.Fprint(e.stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func flags(name string, e *env) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("ai-runner "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs, fs.String("data-dir", e.defaultDataDir, "runner data directory")
}

func service(d *datadir.Dir, e *env) (*app.Service, error) {
	host, err := e.hostname()
	if err != nil {
		return nil, fmt.Errorf("reading hostname: %w", err)
	}
	return &app.Service{
		Keys:         &keyfile.Store{Path: d.KeyPath()},
		Enrollments:  &configfile.Store{Path: d.ConfigPath()},
		ControlPlane: controlplane.New(),
		Host:         domain.Host{Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version},
	}, nil
}

var errTokenArg = errors.New("the enrollment token is never accepted as an argument (it would be visible in shell history " +
	"and the process list); enter it at the prompt, pipe it on stdin or use --token-file")

func enroll(ctx context.Context, args []string, e *env) error {
	fs, dataDir := flags("enroll", e)
	server := fs.String("server", "", "Control Plane URL (https://, or http:// on this host only)")
	tokenFile := fs.String("token-file", "", "read the one-time token from this file (mode 0600)")
	fs.Func("token", "not supported: see --token-file", func(string) error { return errTokenArg })
	fs.SetOutput(io.Discard) // flag errors would echo the offending value, which may be the token
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(e.stderr)
			fs.PrintDefaults()
			return nil
		}
		if strings.Contains(err.Error(), errTokenArg.Error()) {
			return errTokenArg
		}
		return errors.New("invalid flags; see `ai-runner help`")
	}
	if fs.NArg() > 0 {
		return errTokenArg
	}
	if *server == "" {
		return errors.New("--server is required, e.g. --server https://kindevai.example.com")
	}
	if _, err := domain.ParseServerURL(*server); err != nil { // fail before asking for the token
		return err
	}
	tok, err := readToken(*tokenFile, e)
	if err != nil {
		return err
	}
	d, err := datadir.Open(*dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	svc, err := service(d, e)
	if err != nil {
		return err
	}
	res, err := svc.Enroll(ctx, *server, tok)
	if res.KeyCreated {
		fmt.Fprintf(e.stderr, "Generated a new Ed25519 key: %s\n", d.KeyPath())
	}
	if errors.Is(err, domain.ErrKeyEnrolled) {
		return fmt.Errorf("%w. To enroll this machine as a new runner, revoke the old one in the Dashboard and move %s "+
			"away yourself; ai-runner never replaces a key", err, d.KeyPath())
	}
	if err != nil {
		return err
	}
	en := res.Enrollment
	fmt.Fprintf(e.stdout, "Enrolled runner %s with %s (status: %s).\nFingerprint: %s\n", en.RunnerID, en.Server, en.Status,
		en.Fingerprint)
	fmt.Fprintln(e.stdout, "Compare this fingerprint with the runner's card in Dashboard -> Runners before approving it.")
	return nil
}

// readToken takes the token from --token-file (owner-only), the no-echo prompt on a terminal, or stdin.
func readToken(path string, e *env) (domain.Token, error) {
	var raw string
	switch {
	case path != "":
		b, err := secfile.ReadPrivate(path, maxTokenInput)
		if err != nil {
			return domain.Token{}, fmt.Errorf("token file: %w", err)
		}
		raw = string(b)
	case e.readSecret != nil:
		s, err := e.readSecret("Enrollment token (input hidden): ")
		if err != nil {
			return domain.Token{}, fmt.Errorf("reading the token: %w", err)
		}
		raw = s
	default:
		line, err := bufio.NewReader(io.LimitReader(e.stdin, maxTokenInput)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return domain.Token{}, fmt.Errorf("reading the token from stdin: %w", err)
		}
		raw = line
	}
	return domain.ParseToken(raw)
}

func fingerprint(ctx context.Context, args []string, e *env) error {
	fs, dataDir := flags("fingerprint", e)
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := datadir.Inspect(*dataDir) // read-only: works while the daemon holds the lock
	if err != nil {
		return err
	}
	svc, err := service(d, e)
	if err != nil {
		return err
	}
	fp, err := svc.Fingerprint(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(e.stdout, fp)
	return nil
}

// daemon validates the data directory, key and registration, then keeps the outbound WebSocket connection to the
// Control Plane (challenge-response, reconnect with backoff) until shutdown or a fatal error such as revocation.
func daemon(ctx context.Context, args []string, e *env) error {
	fs, dataDir := flags("run", e)
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := datadir.Open(*dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	svc, err := service(d, e)
	if err != nil {
		return err
	}
	en, id, err := svc.Ready(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "ai-runner %s: runner %s enrolled with %s (fingerprint %s); data dir %s\n", version, en.RunnerID,
		en.Server, en.Fingerprint, d.Path())
	logger := slog.New(slog.NewTextHandler(e.stderr, nil))
	ch, closeSpool, err := commandChannel(d, logger)
	if err != nil {
		return err
	}
	defer closeSpool()
	conn := &app.Connector{Dialer: controlplane.New(), Logger: logger, Handlers: []app.LinkHandler{ch,
		&app.Reporter{Probe: probe.New(d.Path())}}} // KIN-20 heartbeat, capabilities, storage
	return conn.Run(ctx, en, id)
}

// commandChannel opens the event spool (events/, 256 MiB) and the command cursor for the KIN-22 channel. Until an
// executor exists (KIN-23) commands are logged and acknowledged: there is no process to start or stop.
func commandChannel(d *datadir.Dir, logger *slog.Logger) (*app.Channel, func(), error) {
	dir := filepath.Join(d.Path(), "events")
	sp, err := spool.Open(spool.Config{Dir: dir})
	if err != nil {
		return nil, nil, err
	}
	ch := &app.Channel{Spool: sp, Cursor: spool.CursorFile{Path: filepath.Join(dir, "commands.json")}, Logger: logger,
		Commands: func(_ context.Context, c protocol.Command) error {
			logger.Warn("command received but this runner has no executor yet", "id", c.ID, "kind", c.Kind)
			return nil
		}}
	return ch, func() { _ = sp.Close() }, nil
}
