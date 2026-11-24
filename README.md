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
| `cmd/ai-runner` | Runner daemon (`enroll`, `fingerprint`, `run`, `version`) |
| `internal/` | Go packages of the Control Plane (`internal/runner/...`: the runner, `internal/protocol`: shared wire messages) |
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
or on sign-out. Every API endpoint except health, setup, login, invitation acceptance and runner enrollment requires a session; cross-origin state-changing
requests are rejected, and login attempts are rate-limited per address and per account.

### Projects and access

The Owner creates projects and grants memberships (`project_admin` or `member`); in the MVP only the Owner
manages memberships. Users see only projects they belong to — any other project answers `404`, exactly like a
missing one. API: `GET/POST /api/projects`, `GET /api/projects/{id}`, `GET /api/projects/{id}/members`,
`PUT/DELETE /api/projects/{id}/members/{login}`.

### Runners

The Owner adds a machine on the **Runners** page: **Add runner** shows a one-time enrollment token (256 bits, valid
10 minutes, only its SHA-256 is stored) and the command to run there, `ai-runner enroll --server <URL>`, which asks
for the token on stdin, so it never appears in argv or shell history. The runner generates an Ed25519 key pair and
registers its public key at the public, rate-limited `POST /api/runners/enroll`; it becomes **pending** and cannot
connect or receive work. The Owner compares the OpenSSH-style fingerprint (`SHA256:…`, the same as `ssh-keygen -lf`)
with `ai-runner fingerprint` on the machine and approves it for an explicit list of projects; later they can change the
projects, disable/enable or revoke it (revocation is final; re-enroll with a new key). Remote runners need HTTPS
(the Caddy profile); plain HTTP is for a runner on the same host only. Owner API: `POST/GET /api/runners/enrollment-tokens`,
`POST /api/runners/enrollment-tokens/{id}/revoke`, `GET /api/runners`, `GET /api/runners/{id}`,
`POST /api/runners/{id}/{approve|disable|enable|revoke}`, `PUT /api/runners/{id}/projects`.

### Runner daemon (`ai-runner`)

`ai-runner` is the second binary (`cmd/ai-runner`, linux/darwin × amd64/arm64, release archives
`kindevai-runner_<version>_<os>_<arch>.tar.gz`). Commands: `enroll`, `fingerprint`, `run`, `version`.

```bash
ai-runner enroll --server https://kindevai.example.com   # prompts for the token without echo
ai-runner fingerprint                                    # compare with the runner's card before approving
```

- The token is read from the hidden prompt, from stdin (`… | ai-runner enroll --server …`) or from
  `--token-file <path>` (the file must be mode 0600); `--token` and positional arguments are refused.
- The Ed25519 key is generated once at the first `enroll` (`keys/ed25519.pem`, PKCS#8, mode 0600) and never replaced:
  a corrupt key is an error, and a key or config file that other users can access is refused (`chmod 600`).
  A retry with the same token after a dropped connection reuses the key. The fingerprint the Control Plane returns must
  match the local key, otherwise the enrollment is not saved.
- TLS certificates are always verified; there is no insecure flag. `http://` works only for a Control Plane on
  `localhost`/loopback, and plain-http connections may only reach a loopback address. Proxy variables (`HTTPS_PROXY`) are ignored for the management connection and redirects are
  not followed.
- Data directory (`--data-dir`): `/var/lib/kindevai-runner` as root, otherwise `$XDG_DATA_HOME/kindevai-runner` or
  `~/.local/share/kindevai-runner`. It holds `config.json` (server, runner id, fingerprint; 0600), `keys/` and
  `repos/ workspaces/ sessions/ events/ proxy/`, all mode 0700; one process at a time (lock file).
- Services: `deploy/runner/kindevai-runner.service` (systemd system service with its own user),
  `kindevai-runner-user.service` (systemd user service, e.g. WSL2, so AI CLIs use your logins) and
  `io.kindevai.runner.plist` (macOS launchd agent); install steps are in each file.
- `ai-runner run` keeps an outbound WebSocket to `wss://<server>/runner/connect` (`ws://` only for a Control Plane on
  loopback). Each connection is authenticated by challenge-response: the Control Plane sends a single-use 32-byte
  nonce bound to the connection id and its own address, the runner signs
  `kindevai-runner-auth/1\n<runner id>\n<audience>\n<nonce>\n<expiry>\n<connection id>` with its Ed25519 key, and
  only an **approved** runner is let in (pending/disabled are refused and retried, revoked stops the daemon with exit
  code 78). Disabling or revoking a runner closes its live connection immediately. The runner reconnects with jittered
  exponential backoff (1 s … 60 s); a TLS verification failure or a challenge for another address stops it (78).
- Limits on the Control Plane: 64 KiB per message, 10 s to authenticate, ping every 30 s (15 s to answer), 30
  connection attempts per minute per address, 256 connections (32 still authenticating); browsers (an `Origin`
  header) are refused. Plain `ws://` is accepted only from loopback (in Docker also from the compose network,
  `KINDEV_RUNNER_PLAIN_WS_PEERS`); remote runners use the HTTPS profile, where Caddy proxies the WebSocket upgrade.
  `--public-url` / `KINDEV_PUBLIC_URL` (e.g. `https://ai.example.com`) pins the origins runners may dial; a connection
  addressed to another host is refused (421), so a relay cannot obtain a signature for itself. A revoked or disabled
  runner that ignores the close frame is cut off after 1 s.

### Integrations and external calls

Calls to YouTrack, GitHub, Telegram and runners never happen inside an API request. Outgoing changes are stored
in a durable outbox in the same SQLite transaction as the change that caused them and delivered in the background
with retries (exponential backoff with jitter, then `dead` after 10 attempts); incoming webhooks are stored in an
inbox, deduplicated by delivery id and processed asynchronously. A slow or unavailable service therefore never
blocks the dashboard. The Owner sees each adapter's state (`ok`, `degraded`, `circuit_open`) and the queue sizes
at `GET /api/integrations/status`, and dead messages at `GET /api/messaging/{outbox|inbox}/dead`
(`POST …/dead/{id}/retry` queues one again). Delivered messages are deleted after 7 days, dead ones after 30.

#### Credentials and integration settings

The Owner manages integrations on the **Integrations** page. The YouTrack connection (one per installation) is a
base URL and a permanent token; each project is linked to a YouTrack project by short name and has a registry of
GitHub repositories (`owner/name`, integration branch `dev` by default; `main` and `master` are protected and can
never be an integration branch). Credentials are encrypted one by one with the installation key (AES-256-GCM, the
credential's name bound as associated data) and are write-only: the API only says whether a token is set and when it
changed, and leaving the token field empty keeps the stored one. **Test connection** queues the check in the outbox;
the page shows the result when the background delivery finishes. Every change is audited without the secret. Owner
API: `GET /api/integrations`, `PUT|DELETE /api/integrations/youtrack`, `POST /api/integrations/youtrack/check`,
`GET /api/projects/{id}/integrations`, `PUT /api/projects/{id}/integrations/youtrack`,
`POST /api/projects/{id}/repositories`, `PUT|DELETE /api/projects/{id}/repositories/{repoId}`.

#### YouTrack intake

Issues of a linked YouTrack project become platform tasks when they carry the project's trigger tag (`kindevai` by
default, changeable per project). **Polling is the primary channel**: every 60 s (15–3600 s, set on the Integrations
page) the Control Plane asks YouTrack for the tagged issues updated since the project's cursor, hands each one to the
durable inbox (deduplicated by issue and `updated`) and then moves the cursor; duplicate or out-of-order deliveries
are harmless, and failures back off up to 15 minutes (honouring `Retry-After`) and show up as the integration status.
The **webhook is optional**: once the Owner sets a secret, a YouTrack workflow may `POST /api/webhooks/youtrack` with
the header `X-Kindevai-Webhook-Secret` and `{"issueId":"KIN-12","updated":<ms>}`; the platform keeps only the issue
ID and re-reads the issue through the REST API, so a webhook body never decides anything. An issue runs for the
platform account the Owner linked to its reporter's YouTrack login, and only if that account may start AI in the
project (the Owner or a project member) — creating an issue in YouTrack grants nothing (note: whoever applies the tag starts intake on behalf of the
reporter; the task still needs a plan and, for risky plans, the Owner's approval); otherwise no task is created
and members see the reason on the project page. Members can also **run by issue ID** (no tag needed; the member is
the initiator). API: `GET|PUT /api/integrations/youtrack/intake`, `PUT|DELETE /api/integrations/youtrack/webhook`,
`POST /api/integrations/youtrack/users`, `DELETE /api/integrations/youtrack/users/{userId}`,
`PUT /api/projects/{id}/integrations/youtrack/trigger` (Owner); `GET /api/projects/{id}/youtrack`,
`POST /api/projects/{id}/youtrack/runs` (members).

#### GitHub App

The platform talks to GitHub only as a **GitHub App** (fine-grained PATs are post-MVP). On the **Integrations** page
the Owner enters the App ID, the App's private key (`.pem`) and the webhook secret (16+ characters); both credentials
go into the encrypted vault and are write-only. Minimal App permissions — grant only these:

| Repository permission | Access |
|---|---|
| Contents | read and write |
| Pull requests | read and write |
| Checks | read |
| Metadata | read |

Do **not** grant Administration or Workflows: agents must not change repository settings or `.github/workflows`.
Subscribe the App to the events Push, Pull request, Check suite and Check run, set the webhook URL to
`https://<your host>/api/webhooks/github`, and install it only on the repositories the platform works in.

- **Installation tokens** are minted on demand from the App's JWT, restricted to the one repository being worked on
  and to the minimal permissions above,
  cached in memory only (at most 50 minutes and never within 5 minutes of expiry) and never stored or logged.
  A project can only obtain a token for a repository registered to that project.
- **Repository state:** after the App is configured, a repository is registered, or **Refresh** is pressed, the
  platform reads (in the background, via the outbox) whether the App is installed, the default branch and whether
  `main`/`master` are protected by a ruleset or branch protection. Unprotected `main`/`master` show a warning — add a
  ruleset that only you can bypass.
- **main/master guard:** independently of GitHub settings, every GitHub request passes a guard in code: only the
  platform's known writes are allowed (default deny — no GraphQL, settings or branch protection changes), and a write
  is refused when it targets `main` or `master` (ref update, pull request base, merge, file commit, including commits
  without an explicit branch). Only the Owner, by hand, promotes to `main`/`master`.
- **Webhooks** (`POST /api/webhooks/github`, no session) are verified with `X-Hub-Signature-256` (HMAC-SHA256,
  constant-time compare) before anything else — unsigned or forged deliveries get 401 —, scoped to registered
  repositories of the right installation (others are acknowledged and dropped), reduced to a small event and stored in
  the durable inbox, deduplicated by `X-GitHub-Delivery`. Bodies are limited to 5 MB, at most 4 are read at once.
- The GitHub API URL is `--github-api-url` / `KINDEV_GITHUB_API_URL` (default `https://api.github.com`; GitHub
  Enterprise Server: `https://<host>/api/v3`).

Owner API: `PUT|DELETE /api/integrations/github`, `POST /api/integrations/github/check`,
`POST /api/projects/{id}/repositories/{repoId}/sync`.

### Approvals and risk policy

Every plan revision gets its effective risk from a deterministic **Policy Evaluator**: the project's rules (paths,
migrations, auth/security, database schema — globs, `**` matches any folders) can only raise the level the AI proposed,
never lower it. L0/L1 plans go straight to READY and are merged into dev by policy; L2/L3 plans wait in the
**Approvals** page for the Owner, plus a separate *merge into dev* request decided after verification. A decision
names the plan revision and its `planHash`; a newer revision supersedes pending approvals and approving an older one
returns 409. A running step that changes files outside its approved scope (or above the approved risk) is stopped
until the Owner approves the extra files. Only the Owner decides and edits the policy; Project Admins see their
projects' approvals and policy. API: `GET /api/approvals?state=pending|decided|all`, `GET /api/approvals/{id}`,
`POST /api/approvals/{id}/{approve|reject}` (`planRevision`, `planHash`, `comment` — required to reject),
`GET/PUT /api/projects/{id}/risk-policy`.
### Audit log

Security-relevant actions are recorded in an append-only audit log in the same transaction as the action itself:
first-run setup completed; sign-in, failed sign-in (address and reason; the login only if that account exists,
never the password; attempts refused by the rate limiter are not recorded) and sign-out; account created, disabled,
enabled, password reset or changed; invitations created, revoked or accepted; project created and member role set or
removed; runner enrollment tokens created or revoked, runners enrolled (or rejected attempts with a known token), approved, given projects, disabled, enabled or revoked (later contexts — approvals, integrations — add theirs). Records cannot be changed or deleted (database triggers
refuse it, and there is no API for it); values that look like passwords or tokens are stored as `[redacted]`. The
Owner reads the log on the **Audit log** page or at `GET /api/audit?actor=&project=&kind=&from=&to=&before=&limit=`
(newest first, `next` is the cursor for the following page); other users get `403`. No automatic retention in the MVP.

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
| PR or push to `dev` / `main` | `make lint` (go vet, golangci-lint, oxlint, tsc), tests with coverage, `make vuln` (govulncheck, npm audit), `make build`, GoReleaser config check, actionlint, Docker image build + smoke test |
| Green push to `dev` | **Dev build**: Control Plane and runner binaries for linux/darwin × amd64/arm64 + `SHA256SUMS` as the CI run's artifact `ai-platform-dev-<sha>` (kept 14 days); image `ghcr.io/tr1gl1de/kindevai-dev:latest` (separate dev package) |
| Tag `v*` on a commit of `main` (owner only) | **Production release**: verifies the tag is on `main`, runs tests, publishes the GitHub Release (changelog, checksums, provenance) and image `:X.Y.Z`, `:X.Y`, `:latest` through the protected `production` environment |

Automation never pushes to `main` or opens PRs against it. Dependabot proposes weekly updates to `dev`.
All third-party actions and images are pinned by commit SHA / digest.

**Required repository settings (owner, one-time)** — workflows cannot enforce these themselves:
1. Rulesets: protect `main` (owner-only push/merge) and tags `v*` (only the owner may create, update or delete).
2. Environment `production`: required reviewer = owner, prevent self-review bypass by admins off, deployment tags `v*`.
3. Package `ghcr.io/tr1gl1de/kindevai`: grant Actions write access only to this repository (dev images go to
   `kindevai-dev`).
Locally: `make lint vuln build`, `make release-snapshot` (needs GoReleaser).

## License

See [LICENSE](LICENSE).
