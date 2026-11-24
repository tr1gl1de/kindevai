# kindevai — AI Development Platform

[Русская версия](README.ru.md)

Self-hosted platform that turns YouTrack tasks into code changes executed by AI CLIs (Codex, Claude Code, ZCode)
on external runners. The project is under active development; the specification lives in the YouTrack Knowledge
Base (project `KIN`, root article `KIN-A-1`).

## Architecture at a glance

- **Control Plane** — one Go process (modular monolith), SQLite in WAL mode, React Dashboard embedded with `go:embed`.
  Targets a 1 vCPU / 1 GB RAM VPS. No PostgreSQL, Redis, Temporal or RabbitMQ.
- **Runner** — Go daemon on Linux/WSL2/macOS; connects to the Control Plane over outbound WSS and runs AI CLIs
  in isolated git worktrees.
- **Web UI** — React + TypeScript, also served as a Telegram Mini App.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/ai-platform` | Control Plane binary (`serve`, `version`) |
| `internal/` | Go packages of the Control Plane |
| `web/` | React + TypeScript Dashboard (Vite); `web/embed.go` embeds the build |
| `scripts/` | Developer helpers (task worktrees, time tracking) |
| `.claude/` | Claude Code rules, agents and skills used to develop this repo |

## Requirements

Go (version in `go.mod`), Node.js 24+ and npm (only to build the web UI), `make`.

## Quick start

```bash
make build            # builds web/dist/app, then bin/ai-platform with the UI embedded
./bin/ai-platform serve   # http://127.0.0.1:8080
```

`ai-platform serve --addr 127.0.0.1:9000` changes the listen address. A binary built without the web UI
still starts and serves a placeholder page.

### Data directory and encryption key

On first start `serve` creates `./data` (`--data-dir` to change it) with the SQLite database in WAL mode
(`data/db/platform.db`), applies schema migrations and generates the encryption key `data/secrets/master.key`
(mode 0600; `--key-file` keeps it elsewhere). Later starts reuse them. **Back up the key together with the database:**
if the database exists but the key is missing or different, the Control Plane refuses to start and never generates
a replacement. Only one process can use a data directory at a time.

### First-run setup

A new installation starts in setup mode. Creating the first Owner requires a one-time **setup token**, which is
never shown in logs or over HTTP: it is written to `data/secrets/setup-token` (mode 0600; the log only prints the
path). Read it on the host (`ai-platform setup-token`, or `docker compose exec cp ai-platform setup-token`),
then enter it in the Dashboard wizard together with the Owner login, password (12+ characters) and platform name.
The token expires after an hour and is renewed automatically; after setup it is deleted and setup can never be
run again, including after a restart.

### Signing in

After setup, sign in with the Owner login and password. Sessions are server-side: the browser keeps an HttpOnly,
SameSite=Strict cookie (Secure over HTTPS), and the session ends after 12 hours of inactivity or 7 days at most,
or on sign-out. Every API endpoint except health, setup and login requires a session; cross-origin state-changing
requests are rejected, and login attempts are rate-limited per address and per account.

### Projects and access

The Owner creates projects and grants memberships (`project_admin` or `member`); in the MVP only the Owner
manages memberships. Users see only projects they belong to — any other project answers `404`, exactly like a
missing one. API: `GET/POST /api/projects`, `GET /api/projects/{id}`, `GET /api/projects/{id}/members`,
`PUT/DELETE /api/projects/{id}/members/{login}`.

## Docker Compose

```bash
docker compose up -d                                        # one container, http://127.0.0.1:8080
DOMAIN=ai.example.com docker compose --profile https up -d  # + Caddy with automatic HTTPS on 80/443
docker compose exec cp ai-platform setup-token        # first-run setup token
```

No `.env` is needed. The plain-HTTP port is published on the host's loopback only; public access goes through
the `https` profile (DNS for `DOMAIN` must point to the host). Data and the encryption key live in the `data`
volume — back it up as a whole. The image is distroless, runs as a non-root user with a read-only root
filesystem, and has a built-in health check (`ai-platform health`).
Behind Caddy, per-client rate limits use `X-Forwarded-For` only from the compose network
(`KINDEV_TRUSTED_PROXIES`, default `172.29.240.0/24`; change `KINDEV_SUBNET` if it collides). Base images are
pinned by digest. Without `DOMAIN` the https profile serves `https://localhost` with Caddy's internal CA.

## Development

```bash
make help             # list targets
make dev              # run the Control Plane on this worktree's port
npm --prefix web run dev   # Vite dev server, proxies /api to the Control Plane
make test             # Go tests (-race -shuffle=on) + web unit tests
make lint             # go vet + TypeScript typecheck
make clean            # remove build and test leftovers
```

Branches start from `dev` as `feature/KIN-<n>-<slug>`; pull requests target `dev`. `main` is owner-only.

## CI and releases

| Trigger | What runs |
|---|---|
| PR to `dev`, push to `dev` | `make lint` (go vet, golangci-lint, oxlint, tsc), tests with coverage, `make vuln` (govulncheck, npm audit), `make build`, GoReleaser config check, actionlint, Docker image build + smoke test |
| Green push to `dev` | **Dev pre-release** `dev`: binaries for linux/darwin × amd64/arm64 + `SHA256SUMS`; image `ghcr.io/tr1gl1de/kindevai:dev` |
| Tag `v*` on a commit of `main` (owner only) | **Production release**: verifies the tag is on `main`, runs tests, publishes the GitHub Release (changelog, checksums, provenance) and image `:X.Y.Z`, `:X.Y`, `:latest` through the protected `production` environment |

Automation never pushes to `main` or opens PRs against it. Dependabot proposes weekly updates to `dev`.
Locally: `make lint vuln build`, `make release-snapshot` (needs GoReleaser).

## License

See [LICENSE](LICENSE).
