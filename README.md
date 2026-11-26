<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/lockup-dark.svg">
    <img src="docs/brand/lockup-light.svg" alt="Kindevai" width="320">
  </picture>
</p>

# Kindevai

**Collective intelligence. Individual care.** Autonomous AI development platform.

[Русская версия](README.ru.md)

Kindevai is a self-hosted platform that turns YouTrack tasks into code changes executed by AI CLIs (Codex, Claude
Code, ZCode) on external runners, under the owner's approval rules. The project is under active development; the
specification lives in the YouTrack Knowledge Base (project `KIN`, root article `KIN-A-1`).

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
| `docs/brand/` | Brand assets: Kindevai lockup, mark and wordmark (SVG; light, dark, mono); Telegram bot avatar, Mini App photo and bot texts in `telegram/` |
| `scripts/` | Developer helpers (task worktrees, time tracking, `measure-cp.sh` load measurement) |
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

To deploy on a server, follow [Installation](#installation).

### Data directory and encryption key

On first start `serve` creates `./data` (`--data-dir` to change it) with the SQLite database in WAL mode
(`data/db/platform.db`), applies schema migrations and generates the encryption key `data/secrets/master.key`
(mode 0600; `--key-file` keeps it elsewhere). Later starts reuse them. **Keep the key safe — backups need it (see Backup, restore and moving):**
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
  `io.kindevai.runner.plist` (macOS launchd agent); install steps are in each file. Stopping or restarting the
  service stops only the daemon (`KillMode=process`, launchd `AbandonProcessGroup`): running attempts keep going and
  the next start adopts them, recognising each process by its pid and start time so a reused pid is never taken for
  it. To stop the attempts as well, disable the runner in the Dashboard or use `systemctl kill` (KIN-64).
- `ai-runner run` keeps an outbound WebSocket to `wss://<server>/runner/connect` (`ws://` only for a Control Plane on
  loopback). Each connection is authenticated by challenge-response: the Control Plane sends a single-use 32-byte
  nonce bound to the connection id and its own address, the runner signs
  `kindevai-runner-auth/1\n<runner id>\n<audience>\n<nonce>\n<expiry>\n<connection id>` with its Ed25519 key, and
  only an **approved** runner is let in (pending/disabled are refused and retried, revoked stops the daemon with exit
  code 78). Disabling or revoking a runner closes its live connection immediately and, in the same transaction, fails
  its running attempts (retried on another runner); the runner stops their processes itself as soon as it is told it
  was disabled or revoked, without waiting for the Control Plane (KIN-63). The runner reconnects with jittered
  exponential backoff (1 s … 60 s); a TLS verification failure or a challenge for another address stops it (78).
- Limits on the Control Plane: 64 KiB per message, 10 s to authenticate, ping every 30 s (15 s to answer), 30
  connection attempts per minute per address, 256 connections (32 still authenticating); browsers (an `Origin`
  header) are refused. Plain `ws://` is accepted only from loopback (in Docker also from the compose network,
  `KINDEV_RUNNER_PLAIN_WS_PEERS`); remote runners use the HTTPS profile, where Caddy proxies the WebSocket upgrade.
  `--public-url` / `KINDEV_PUBLIC_URL` (e.g. `https://ai.example.com`) pins the origins runners may dial; a connection
  addressed to another host is refused (421), so a relay cannot obtain a signature for itself. A revoked or disabled
  runner that ignores the close frame is cut off after 1 s.
- Command/event channel: commands from the Control Plane (a per-runner transactional log written with the domain
  change, monotonic seq) are applied once each, in commit order, and re-sent until the runner acknowledges them. Runner events (accepted, started,
  output, completed, failed) are written to an on-disk spool first (`events/`, fsync, 256 MiB; when it is full the
  output is dropped and reported as `execution.output.dropped`, lifecycle events are kept) and deleted only after the
  Control Plane committed and acknowledged them, so a disconnect or a restart of either side loses nothing. Every
  event carries its attempt's fencing token: a stale runner's events are rejected and it is told to cancel the
  process. Each session starts with `runner.reconcile` (running attempts, unacknowledged results, spool offsets).
- Secrets in runner output: the runner masks the secrets it holds (the assignment's git token, proxy passwords) by
  exact value and secret shapes (tokens, API keys, passwords in URLs, `key=value` pairs, PEM keys) before output is
  spooled, holding a partial line back (at most 8 KiB, up to 1 s) so a secret split across writes is masked too; the
  Control Plane masks every output chunk and failure reason again before it is stored, and once more when the task
  UI reads them. Lines are kept: a masked value becomes `[redacted]` in place.
- Retention of recorded runner events: nothing of a running execution is deleted. Output goes 14 days after its
  execution finished (`--runner-output-retention-days` / `KINDEV_RUNNER_OUTPUT_RETENTION_DAYS`), other events after
  30 days (`--runner-event-retention-days` / `KINDEV_RUNNER_EVENT_RETENTION_DAYS`); acknowledged commands after 7
  days; the whole command log of a revoked runner (it never connects again) within an hour. Output is capped at 16 MiB per attempt and 1 GiB in total (`--runner-output-limit-mb` /
  `KINDEV_RUNNER_OUTPUT_LIMIT_MB`): over the limit the oldest output of finished executions is evicted first, and if
  only running executions hold output, new output is stored without its body (`output_storage_limit`) and a
  warning is logged. The cleanup runs at start and hourly in small batches.
- Mock execution (no AI yet): `POST /api/tasks` creates a manual task, `POST /api/tasks/{id}/mock-run`
  `{"scenario":"success|fail|flaky|crash|hang|gate"}` plans it with a fixed one-step plan on the project's first
  repository (L1, so it starts without an approval unless the risk policy raises it), and `GET /api/tasks[/{id}]` shows
  the task with its steps and attempts; `POST /api/tasks/{id}/cancel` `{"reason":"…"}` stops it. The scheduler assigns
  READY steps only to APPROVED, connected, reachable runners granted the project, with the step's capabilities and
  no critical volume; without one the task is `BLOCKED` (reason `runner`) until one appears. Among those it prefers a
  runner without 2+ failed or lost attempts in the last hour, then the emptier disk (ok, warning, high), then the
  fewest held steps; a runner started with `ai-runner run --max-parallel-steps N` gets at most N workflow steps at
  once (planning runs are not counted; a step only such full runners could take waits as `WAITING(runner)`). The runner checks every
  assignment again (`ai-runner run --projects <id,…>` / `KINDEV_RUNNER_PROJECTS` limits the projects it accepts) and
  runs the mock agent as a child process in its own process group; a cancel sends SIGTERM to the group, SIGKILL
  after 5 s. A lost runner's attempt becomes `UNKNOWN` when its 60 s lease (renewed by heartbeats) runs out and
  nothing is started again until the runner reconciles: a live process continues (a restarted runner adopts it from
  its journal under `attempts/`), a gone one fails and is retried once.
- Tasks & Executions (`/tasks`, KIN-25): the task list filtered by project and state, and a task page with the plan
  revision, steps by stage, attempts (runner, CLI) and each attempt's output and events, read by polling in bounded
  pages. The Owner and project admins create tasks, plan them with the mock planner, cancel them and retry a failed
  one (`POST /api/tasks/{id}/retry`: the current plan becomes a new revision, checked by the risk policy again).
  **Agent time** is the time the agents' CLI processes ran (from the runner's `execution.started` to its
  completed/failed event, on the runner's clock, recorded once per attempt); it is shown per attempt, step, task and
  for the list's filter, apart from the task's calendar duration. A period without an end (a lost process) or a usable
  time is shown as "accounting incomplete", never as a complete record.
- Agents (`/agents` for the Owner, the "AI agents" panel of a project for the Owner and its admins, KIN-33): enable or
  disable each AI CLI (Claude Code, Codex) globally and per project — a project cannot enable a CLI the Owner disabled —
  and bind the planner, implementer and reviewer roles to a CLI and model with an ordered fallback (the reviewer
  prefers a CLI other than the author's). The scheduler runs a step on the first permitted CLI that an eligible runner
  has installed and is not cooling down on after a usage limit; otherwise the task is `BLOCKED` with the reason
  (`agent`, `cli_limit` or `runner`). Disabling lets running sessions finish; **Emergency stop** cancels them too.
- AI planner (KIN-34): "Plan with AI" on the task page (`POST /api/tasks/{id}/plan`, Owner / project admin) runs
  the CLI bound to the planner role on a runner, read-only in a worktree of the project's first repository (no scope,
  never the permission bypass). Its context package holds the task, the YouTrack issue text and up to 3 linked
  Knowledge Base articles (read through the outbox, bounded, fenced as untrusted text that cannot change the rules,
  scope or permissions) and the plan JSON Schema (`internal/workflow/domain/plan.schema.json`, v1: steps DAG, repos,
  scope, risk, contracts, migrations, tests, docs). The answer is parsed strictly; a valid plan becomes the next plan
  revision through the risk policy (L0/L1 READY, L2/L3 approval), an invalid one gets one repeat told why, then the
  task FAILS with the reason. The planner's native session is kept: a repeat or replan on the same runner and the plan
  chat continue it. `KINDEV_REAL_CLI_SMOKE=1 go test -run TestRealCLIPlannerSmoke ./cmd/ai-platform/` asks the
  installed Claude CLI for a real plan.
- Implementation and independent review (KIN-35): a plan step routed to an AI CLI runs a loop on its work branch
  `feature/<task>--<step>`. The implementer gets the approved step (task title and plan text fenced as untrusted, the
  scope, the plan's tests, the repository's test command) and ends with a handoff (summary, documentation impact, open
  questions). After its CLI, `agent-exec` runs the repository's **test command** (Project → Repositories, e.g.
  `make test`) with `/bin/sh` in the worktree, inside the attempt's confinement and with an allowlisted environment
  (no CLI credentials); the runner reports exit code, duration and the output's tail (`execution.tests`) — the
  model's claim is never evidence. A failing or missing result fails the attempt (one retry with the output), no
  test command is shown as "not configured". A reviewer then runs on the same runner (it holds the branch) with a CLI
  other than the author's when routing allows, read-only (Claude `--permission-mode default` without write/shell
  tools, Codex `--sandbox read-only`), with the bounded diff base..head fenced in its prompt; it answers a JSON verdict
  `approve | changes_requested` with findings. Changes requested start a fix round with the findings (at most 2), after
  which the step FAILS with the reason; a reviewer that changes the branch fails its attempt. The task page shows the
  phase, the handoff (base/head, changed files, test evidence) and the verdict. Pushing and the PR are KIN-36.
- CI fix loop and merge into dev (KIN-37): the Control Plane follows CI on each pull request it opened (completed
  check suites from the GitHub webhook plus a reconciliation every minute; the status combines all latest check runs
  of the head — none yet or one running is pending). Such CI never raises an Alert Center alert. Red CI reopens the
  execution: the step implements again on the runner that holds its branch, told which checks failed, is reviewed and
  pushed again (fast-forward) — at most 3 CI fix rounds per step, then the task is BLOCKED (reason `ci`) with an
  alert; a later green re-run resumes it. Green CI goes through the merge gate: integrate_dev granted by policy
  (L0/L1) or approved by the Owner (L2/L3) — without it nothing is merged, the PR shows "merge waiting". The merge is a
  merge commit with the expected head SHA (a moved head, a conflict or a protected base is never merged: the task is
  BLOCKED with the reason); a multi-repository change set merges in order and stops at the first failure. When every
  PR is merged the task is COMPLETED. `main`/`master` are never a merge target.
- CI-blocked tasks and repositories without CI (KIN-105): a repository without CI is never merged automatically — a
  head with no check run at all shows "No CI checks", and after 30 minutes without one the task is BLOCKED (reason
  `ci`). On a task blocked on CI the Owner or a project admin can **Resume** (`POST /api/tasks/{id}/resume`: CI is
  checked at once, red CI blocks again, a repository without CI gets another 30 minutes) or **Retry merge**
  (`POST /api/tasks/{id}/retry-merge`, after a refused merge). Neither skips the merge gate. Legacy commit statuses
  are not read: CI must report check runs (GitHub Actions does).
- CI fix rounds in practice (KIN-106): the implementer of a CI fix round is told the failed checks' output title and
  summary (bounded, as untrusted text), not only their names; audit entries, block reasons and the PR card keep the
  names only (a summary may echo log lines). If the runner holding the step's branch is disabled or revoked, the round
  continues on another runner from the published head of the remote branch (fetched into its cache), so the push
  stays a fast-forward; a branch that someone else moved is never overwritten. A runner that only went offline keeps
  the step until it returns — disable it to move the work. When a task reaches VERIFYING with a pending integrate_dev approval (L2/L3), the Owners get a Telegram
  notification that the verified work waits for the merge into dev.
- Plan discussion API (KIN-43; the chat UI is KIN-44): the Owner and project admins write about the current plan
  revision of a task awaiting approval or ready (`POST /api/tasks/{id}/discussion/messages`, 202; members and viewers
  read `GET /api/tasks/{id}/discussion`). Each message is one turn run by the planner role's CLI like a planner
  attempt (durable runner command log, read-only, no scope), resuming the planner's native session on its runner or
  starting a fresh one with the plan, the context package and the last 20 messages, all fenced as untrusted text. The
  answer is stored with its usage and CLI time (agent time); a ```json plan in it becomes the next revision through
  the plan validation and the risk policy (the AI cannot lower risk; approvals of the old revision go stale), an
  invalid one is noted and ignored. Status PENDING → DELIVERED → ANSWERED | FAILED; a turn without an answer after the
  timeout FAILS and may be sent again (`…/messages/{msg}/retry`). Limits (Owner: `PUT /api/plan-discussion/limits`):
  4000 characters, one answer in flight, 20 messages per task, 30 per user per hour, 10 min timeout; exceeding one is
  429 with its code. Messages, answers, revisions and limit hits are audited.
- AI CLIs (runner, KIN-79): an assignment for `claude` or `codex` (chosen by routing) runs the CLI found on the
  runner's PATH with the runner user's HOME (its local login) and a clean environment, as `ai-runner agent-exec` in the
  attempt's process group: the prompt/context package is a file in the attempt's state directory (outside the
  worktree) read on stdin, the CLI's output goes to files there (read on after a runner restart, nothing is sent
  twice) and the timeout (default 2 h) holds without the runner. The permission bypass is granted only inside the
  verified strict egress sandbox (KIN-31), refused elsewhere. The CLI's typed events (session, text, tool use, result,
  failure) appear in the attempt's log; its usage (tokens, cost, turns), native session id and CLI version are stored
  per attempt and shown next to the agent time. A usage limit or logout (`cli_limit`) parks the step and BLOCKs the task
  (reason `cli_limit`, an alert) until the limit resets (30 min when the CLI names no time) — or at once on another permitted CLI or runner,
  while this runner's CLI cools down. A logged-out or billing-refused CLI is retried after 30 min, 2 h and 6 h, then
  waits for the Owner's **ready again** on the Agents page; its alert names the runner and CLI and turns critical at
  the 3rd hold in a row.
- Workspaces (runner): when the step's repository is registered for the project, the attempt runs in its own git
  worktree under `workspaces/` on the branch `feature/<task>--<step>` (never main/master), started from the
  repository's integration branch fetched into a bare cache under `repos/`; the base commit is recorded. The Control
  Plane adds the branch and a short-lived installation token for that repository to the assignment only when it is
  sent; the runner hands the token to git only through its credential helper (`ai-runner git-credential`, a unix
  socket the runner serves during the fetch) and only for the host of `--git-base-url` (default
  `https://github.com`, `KINDEV_RUNNER_GIT_BASE_URL`): never in arguments, environment, files or the remote URL.
  Instruction files (`AGENTS.md`, `CLAUDE.md`, `.claude/{agents,skills,rules,commands,settings*}`, `.codex/`,
  `.mcp.json`) are reported with their SHA-256 as untrusted data (symlinks are not followed, nothing is parsed).
  When the attempt ends, uncommitted work is committed to the branch, the changed files against the base and any
  changed instruction file are reported, and the worktree is removed; the branch stays. One writer per workspace;
  a workspace needs `--workspace-min-free-mib` (default 2048) free on the data dir's filesystem.
- Storage (Owner): the Overview shows every machine's disk space; **Infrastructure → Storage** (`/infrastructure/storage`)
  adds the hourly fill history for 7 or 30 days with the 75/85/95% levels, every volume with both forecasts and the
  space consumers. A runner measures its consumers (repos, workspaces and worktrees, CLI sessions, the event spool,
  `docker system df`) only when started with `ai-runner run --measure-space`: every 6 h, metadata only, paced to
  2 000 entries/s, at most 1 000 000 entries and 10 min, no symlinks, one filesystem. Nothing is ever deleted.

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
platform account the Owner linked to the YouTrack login of **whoever applied the trigger tag** (read from the issue's
tag history, not the reporter), and only if that account may start AI in the project (the Owner or a project
member) — creating or tagging an issue in YouTrack grants nothing. If the history does not show clearly who applied
the tag, no task is created either (`tag_not_allowed`); members see the reason on the project page. Members can also **run by issue ID** (no tag needed; the member is
the initiator). API: `GET|PUT /api/integrations/youtrack/intake`, `PUT|DELETE /api/integrations/youtrack/webhook`,
`POST /api/integrations/youtrack/users`, `DELETE /api/integrations/youtrack/users/{userId}`,
`PUT /api/projects/{id}/integrations/youtrack/trigger` (Owner); `GET /api/projects/{id}/youtrack`,
`POST /api/projects/{id}/youtrack/runs` (members).

#### YouTrack write-back

A task of a YouTrack issue reports back to the issue, as the integration user (the connection's token): a comment
with the accepted plan (summary, risk, steps) and the link to approve it on the task page (`--public-url`), every
state change if the project maps task states to YouTrack states, and a final comment with the pull requests, the
agent time (CLI process time) and the documentation decision. By default the map is empty, so only comments are
written; the Owner or the project's admin sets it under **YouTrack states** on the project page (one field per task
state; empty keeps the issue's state) or with `PUT /api/projects/{id}/integrations/youtrack/states`
(`{"states":{"RUNNING":"In Progress","COMPLETED":"Fixed"}}`). A task's writes reach YouTrack one at a time in the order
they were made, so the final comment never lands before the state change. Every write is an outbox message keyed by the change
(its correlation id): a comment carries a `kindev-…` marker, so a retry after a lost response never posts twice, and
the platform's own writes coming back through the webhook or the poller never create new tasks. While YouTrack is
unavailable the writes are retried and the task page shows **Awaiting publication to YouTrack**; writes given up
show as not published with the reason and raise a **YouTrack write-back failed** alert in the Alert Center, which
closes when the task's next write is published. The implementer may update Knowledge Base sections — only of articles the
approved plan names in its documentation impact — or state `docsNotApplicable` with a reason (`DOCS_NOT_APPLICABLE`);
the decision is stored and shown on the task page. A section is recomputed on the article as it is right before the
write, and a concurrent edit makes it re-read and recompute (after three conflicts the write fails visibly).

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

### Network profiles and Test Network

**Dashboard → Network** (Owner) manages network profiles: an upstream proxy (`http://`, `https://` or `socks5://`,
entered as a URL — credentials in it are taken out — or as fields over the API), the expected public egress IPv4/IPv6,
the enforcement mode (`off`, `monitor`, `strict`), DNS policy (runner resolver or remote DNS through the proxy),
fail-closed and an allowlist of destinations (host, `*.suffix` or CIDR with ports). The proxy password is stored
encrypted in the credentials vault and never returned; audit entries record only that it changed.

Profiles are assigned along **System → Runner → Project → CLI**. A lower level may switch the proxy but never relaxes
security: enforcement, DNS and fail-closed only tighten and an allowlist only narrows, so strict egress set for the
whole installation cannot be lifted below. Allowlists refuse `*`, top-level wildcards, `0.0.0.0/0`, `::/0`, overly
wide prefixes and loopback/link-local/multicast ranges. Every change re-delivers each runner's resolved profiles with
the `runner.config.update` command (the password sealed in the command log and opened only on the authenticated
session; the runner keeps it in a `0600` file and never logs it).

**Test Network** runs a preflight on a runner — proxy reachability and authentication, the observed egress IP from the
diagnostic endpoint (default `https://api64.ipify.org`, configurable per profile) compared with the expected one, and
reachability of the AI providers, GitHub and YouTrack — and shows the result, failure reason and time. On the runner
machine itself: `ai-runner test-network` (it also says whether strict egress is available there and why not).

**Strict egress** (Linux and WSL2): a step whose effective profile is `strict` runs only in a per-attempt sandbox —
new user, network and mount namespaces created by the runner without root. Inside, the only interface is the
sandbox's own loopback; nftables rejects everything else (IPv4, IPv6, DNS, UDP). The filesystem is a minimal view
(KIN-80): system directories (`/usr`, `/etc`, `/lib`, …) read-only, the attempt's worktree and state directory
writable, the repository mirror read-only (only the worktree's own index/HEAD writable), a private `/tmp`, and a
private per-attempt `HOME` holding copies of the CLI's login files only (`~/.claude/.credentials.json`,
`~/.claude.json`, `~/.claude/settings.json`, `~/.codex/auth.json`, `~/.codex/config.toml`; nothing is copied back,
so an agent cannot change the login or settings of later runs). Of `/etc` only what programs need (users, name
resolution, TLS roots, time zone, linker) is shown. The runner's real `HOME` (SSH keys, other repositories), its data
directory (Ed25519 key, other attempts), `/opt`, `/run` (Docker, D-Bus, WSL interop), `/mnt` (Windows drives) and
everything else are absent; a CLI installed outside the system directories is exposed read-only. The only way out is the
runner's local proxy adapter, which applies the profile's allowlist and connects through the upstream proxy (HTTP
CONNECT, HTTPS proxy or SOCKS5 with remote DNS; the credentials never enter the sandbox). Before the CLI starts, a
preflight inside the sandbox must pass: proxy reachable, egress IP as expected, and direct TCP (IPv4/IPv6), DNS, a
client ignoring `HTTP_PROXY` and host sockets all blocked, the runner's `HOME` and data directory invisible, and `/`
and the system directories unwritable. Otherwise the CLI never starts and the step waits
(WAITING, reason `network`) and is retried after two minutes, doubling with every further hold in a row on the
same runner (up to 30 minutes; from the third one the Owner gets an egress alert for that runner); there is no direct
fallback. While the CLI runs, the runner checks the egress IP again every 5 minutes: a wrong IP (or two failed checks
in a row) makes the adapter cut every connection and the step waits for the network the same way. Connections the
allowlist denied are counted and reported at the end of the attempt's output. After a runner restart an adopted
strict attempt gets its proxy adapter back on the same socket, or is stopped and waits for the network. The runner reports
`strictEgress` only after a self-test sandbox verified, and the scheduler gives strict steps only to such runners
(macOS: unsupported). CLI permission bypass (`--dangerously-skip-permissions` and the like) is allowed only inside this
sandbox. Requirements: `nft` installed and unprivileged user namespaces enabled (Ubuntu 24.04:
`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`). Known limits: inside the sandbox `git add`/`git commit` by the agent fail (the mirror's
objects are read-only; the runner commits after the run); a CLI login refreshed inside the sandbox is not kept;
Linux 5.12+ is required. **Without the sandbox** (macOS, no user namespaces, or a profile
that is not `strict`) the CLI runs on the host with the runner's uid and its real `HOME`: it can read everything the
runner user can (SSH keys, the runner's key and data directory, other repositories). That is why the permission bypass
stays off there and the CLI keeps asking before acting; use strict egress for untrusted tasks.

**CLI logins and sessions on a runner** (KIN-86). The CLIs run with a CLI home: the runner user's `HOME` by default,
or a dedicated directory with `ai-runner run --cli-home <dir>` (or `KINDEV_RUNNER_CLI_HOME`; outside the data
directory, created 0700), so the runner's logins stay apart from yours. Log the CLIs in there explicitly:

```bash
claude setup-token                          # on any machine with a browser: prints a token valid for one year
ai-runner cli-login claude --cli-home <dir> # paste it (hidden prompt; or stdin, or --token-file <0600 file>)
ai-runner cli-login codex --cli-home <dir> -- --with-api-key   # runs `codex login` with HOME=<dir>
ai-runner cli-login status --cli-home <dir>                    # what each CLI is logged in with, token expiry
```

The Claude token stays on the host (`<dir>/.kindev/claude-oauth-token`, 0600) and reaches Claude Code as
`CLAUDE_CODE_OAUTH_TOKEN`; it needs no refresh, so nothing is lost in the sandbox, and it is masked in attempt
output. Inside the sandbox an agent can read the login it runs with but never write the host's: login files remain
copies. A short-lived interactive login (`claude` → `/login`, Codex's ChatGPT login) also works but its refresh inside
the sandbox is lost — prefer the token (Codex: an API key). A sandboxed attempt whose CLI has no login (or an expired
token) fails before the CLI starts as a logged-out CLI: the step is held and the Owner alerted (`cli_limit`, cause
`auth`); the runner also warns at start-up and a month before the token expires. For `--resume` between attempts,
each task gets its own session store in the data directory (`cli-sessions/<project>/<task>/<cli>`), bound at
`~/.claude/projects` / `~/.codex/sessions` in the sandbox: an attempt sees its own task's sessions only, never another
task's or yours; stores unused for 30 days are removed. A task's store is limited to 256 MiB and 20,000 entries
(files, directories, symlinks): before each attempt the oldest session files go until it is within 3/4 of both (the
attempt's output says so), and while the attempt runs the runner measures the store every 2 seconds and stops an
attempt that goes over either limit, or whose store it cannot measure (it fails with the reason; it may overshoot
by what it writes in those seconds).
On macOS there is no strict sandbox, so no session store and no such limit: the CLI keeps its sessions in its own
home directory. A strict profile needs `api.anthropic.com` in its allowlist
for Claude Code (telemetry to `*.datadoghq.com` is optional).

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
Any plan revision of a task, with what changed since the previous one (steps added, removed or changed; scope,
risk, tests, migrations), is at `/tasks/{id}/plan/revisions/{n}` for everyone who sees the task
(`GET /api/tasks/{id}/revisions/{n}?base=m` compares with revision `m` instead).
### Audit log

Security-relevant actions are recorded in an append-only audit log in the same transaction as the action itself:
first-run setup completed; sign-in, failed sign-in (address and reason; the login only if that account exists,
never the password; attempts refused by the rate limiter are not recorded) and sign-out; account created, disabled,
enabled, password reset or changed; invitations created, revoked or accepted; project created and member role set or
removed; runner enrollment tokens created or revoked, runners enrolled (or rejected attempts with a known token), approved, given projects, disabled, enabled or revoked (later contexts — approvals, integrations — add theirs). Records cannot be changed or deleted (database triggers
refuse it, and there is no API for it); values that look like passwords or tokens are stored as `[redacted]`. The
Owner reads the log on the **Audit log** page or at `GET /api/audit?actor=&project=&kind=&from=&to=&before=&limit=`
(newest first, `next` is the cursor for the following page); other users get `403`. No automatic retention in the MVP.

### Backup, restore and moving the installation

Automatic backups are not part of the MVP; `export`/`import` move an installation by hand. The Control Plane is a
single instance: besides the lock file `data/ai-platform.lock` it keeps an instance record (host, pid, heartbeat) in
the database, so a second process on the same data refuses to start — even over a network file system. A restart on the same
host takes the record over at once; from another host it expires 30 seconds after the last heartbeat, and an instance
that loses its record stops.

1. **Maintenance mode** (Owner, *Maintenance* in the menu): no new steps are handed out, running ones finish, runners
   keep reconciling, and changes in the UI/API are paused for everyone (signing in, webhooks and safety actions still work:
   cancelling a task, revoking/disabling a runner, disabling a user, rejecting an approval). A banner shows the reason and how many steps are still running. Wait until none are.
2. **Export** (works while serving; an online, consistent SQLite snapshot):
   ```bash
   ai-platform export --data-dir ./data --out backup.tar.gz --key-out backup.key
   # Docker: docker compose exec cp ai-platform export --data-dir /data --out /data/backup.tar.gz --key-out /data/backup.key
   #         docker compose cp cp:/data/backup.tar.gz . && docker compose cp cp:/data/backup.key .
   ```
   The archive holds a manifest (format, version, schema, installation id, SHA-256 of every file), the database and
   `data/artifacts/`. **The encryption key is never inside it.** You choose explicitly: `--key-out` writes it to a
   separate 0600 file, `--without-key` leaves it out and you copy the key file yourself. Whoever has both the archive
   and the key can read every stored secret — move them over different channels and delete leftover copies.
3. **Stop the old Control Plane** (`docker compose down`) so that two instances never run at once.
4. **Import** into an empty data directory on the new host:
   ```bash
   ai-platform import --data-dir ./data --archive backup.tar.gz --key backup.key
   # Docker (files readable by uid 65532):
   # docker compose run --rm --no-deps -v "$PWD:/backup:ro" cp import --data-dir /data --archive /backup/backup.tar.gz --key /backup/backup.key
   ```
   Import checks the manifest, every checksum, the database integrity, the schema version (a database from a newer
   binary is refused before anything is written), and that the key matches the database. Without the key it stops
   with an error — a new key is never generated. Then it applies pending migrations.
5. **Start** the new Control Plane, point DNS/`--public-url` at it and switch maintenance mode off. Runners reconnect
   by themselves; every Control Plane start uses a new command-channel epoch, so a restored (older) database never makes
   a runner skip new commands.

## Installation

A step-by-step deployment. The sections above describe each part in detail.

### 1. Choose the delivery profile

| Profile | Command | Use |
|---|---|---|
| Compose + HTTPS (Caddy) | `DOMAIN=ai.example.com docker compose --profile https up -d` | **A server.** The only public profile in the MVP. |
| Compose, plain HTTP | `docker compose up -d` | This machine only: `http://127.0.0.1:8080`. |
| Standalone, plain HTTP | `ai-platform serve` | This machine only, without Docker. |
| Standalone HTTPS (built-in TLS/ACME) | — | Not in the MVP. Use the Compose HTTPS profile. |

Public access, the Telegram Mini App and **remote runners need HTTPS**. Plain HTTP (and `ws://` for runners) is only
for a browser and a runner on the same host. Never publish the plain-HTTP port: Compose binds it to `127.0.0.1`.

**Control Plane host:** a Linux VPS with 1 vCPU and 1 GB RAM is enough. It needs Docker with Compose v2, a DNS
name pointing to it, and ports 80/443 open for the ACME certificate. Do not run runners, AI CLIs or other heavy
services there: agents work on the runners. Measured under `--cpus=1 --memory=1g` (KIN-42), with 8 runners running
1000 mock tasks in 33 s, the Control Plane used at most 50 MiB of RAM and 67% of the vCPU (about 11 MiB at idle).
The mock tasks skipped the pull request, CI and merge path and the YouTrack writes (no GitHub App), which add a few
outbox calls per task. Real AI tasks take minutes each, so the load is far lower. Repeat the measurement with `scripts/measure-cp.sh`.

### 2. Start the Control Plane

```bash
git clone --branch vX.Y.Z https://github.com/tr1gl1de/kindevai.git && cd kindevai   # a release: compose.yml matches it
export KINDEV_IMAGE_TAG=X.Y.Z                       # the same release
export DOMAIN=ai.example.com KINDEV_PUBLIC_URL=https://ai.example.com
docker compose --profile https pull && docker compose --profile https up -d
docker compose exec cp ai-platform setup-token     # one-time setup token, valid 1 h
```

Open `https://ai.example.com`, enter the token, and create the Owner (password: 12+ characters). `KINDEV_PUBLIC_URL`
pins the origin runners may dial, and it is the link the YouTrack comments use. Keep both variables in your shell
profile or in a systemd unit, because every later `docker compose` command needs the same values. Back up the
encryption key right away. It is generated on first start, and nothing can be decrypted without it (step 9).

### 3. Protect `main`/`master` on GitHub (required)

> **Required in every deployment.** For each repository the platform works in, a GitHub ruleset or branch protection
> must block direct pushes to `main`/`master` and merges into them. Only you may bypass it.

Why the platform cannot do this alone:

- A runner gets a short-lived GitHub App installation token with **Contents: write** for the repository it works
  on. GitHub scopes that token to the repository, not to a branch, so a runner or agent that leaks it can push to any
  branch, `main` included.
- GitHub's merge API (`PUT /repos/{owner}/{repo}/pulls/{n}/merge`) takes only the pull request number and does not
  pin the base branch. If someone retargets the pull request at `main` between the platform's check and the merge,
  the merge lands on `main`.
- The platform refuses every write to `main`/`master` in code (the main/master guard, the integration branch is
  never `main`/`master`), and the runner refuses to push there. Those checks protect the platform's own requests,
  but only GitHub can stop a leaked token.

Settings for each repository (**Settings → Rules → Rulesets → New branch ruleset**):

1. Target branches: `main` and `master` (add "Include default branch" if your default branch is one of them).
   Enforcement status: **Active**.
2. Rules: **Restrict updates**, **Restrict deletions**, **Block force pushes**, and **Require a pull request before
   merging**.
3. Bypass list: only you (or the repository admin role). **Never add the GitHub App.** With "Restrict updates"
   every change of the branch, a pull request merge included, is yours alone.
4. If you release from the repository, add a tag ruleset for `v*` that only you may create, update or delete, and a
   `production` environment with you as the required reviewer.

On a plan without rulesets, classic branch protection on `main`/`master` (require a pull request, restrict who can
push to you, do not allow bypassing) does the same job. After **Refresh** on the Integrations page, the repository
must no longer show the "main/master not protected" warning.

### 4. Connect GitHub (GitHub App)

1. **GitHub → Settings → Developer settings → GitHub Apps → New GitHub App**:
   - webhook URL `https://ai.example.com/api/webhooks/github`, with a random webhook secret of 16+ characters;
   - only the permissions listed in [GitHub App](#github-app) (Contents and Pull requests read and write, Checks and
     Metadata read; **no** Administration or Workflows);
   - events Push, Pull request, Check suite and Check run.
2. Generate a private key (`.pem`), then install the App **only on the repositories** the platform works in.
3. **Integrations** page: enter the App ID, the private key and the webhook secret.
4. Per project: register each repository as `owner/name`. The integration branch is `dev` and must exist, never
   `main`/`master`. Add the test command (e.g. `make test`). The repository needs CI that reports **check runs**
   on pull requests (GitHub Actions does; legacy commit statuses are not read). Without CI nothing is merged: the
   task is BLOCKED (reason `ci`) after 30 minutes.

### 5. Connect YouTrack

1. Create a dedicated YouTrack user for the platform and a permanent token for it. The user needs to read issues
   and Knowledge Base articles, comment, apply commands (state changes), and update articles if you want KB
   write-back.
2. **Integrations** page: enter the YouTrack base URL and the token, then **Test connection**.
3. Per project: link the YouTrack project by short name and choose the trigger tag (`kindevai`). Map platform users
   to their YouTrack logins: only the account of whoever applied the tag can start a task. Optionally map task
   states to YouTrack states (`PUT /api/projects/{id}/integrations/youtrack/states`) and set up the webhook. Polling
   works without it.

### 6. Telegram (optional)

1. Create a bot with [@BotFather](https://t.me/BotFather) (`/newbot`) and note its token.
2. Create the Mini App with `/newapp` and the site's HTTPS address, `https://ai.example.com/`. Telegram accepts
   only HTTPS.
3. **Settings → Telegram** (`/settings/telegram`, Owner): enter the bot username, the Mini App short name and the
   bot token. The token is stored encrypted.
4. Each user links their own Telegram account on the **Account** page (a one-time link plus a 6-digit code). Alerts
   reach linked users in Telegram.
5. Optionally give the bot the Kindevai look. BotFather keeps these settings, so the platform never changes them;
   the files are in `docs/brand/telegram/`:
   - `/setuserpic` → your bot → send `docs/brand/telegram/bot-avatar-640.png` as a photo (Telegram crops it to a
     circle; the mark stays inside).
   - `/setname` → `en.name` (`Kindevai`); `/setabouttext` → `en.short_description` and `/setdescription` → `en.description` from
     `docs/brand/telegram/bot-profile.json`.
   - Mini App photo: `/newapp` (step 2) asks for a 640×360 image, later `/myapps` → your Mini App → **Edit Photo** (in some BotFather versions under **Edit App**);
     send `docs/brand/telegram/miniapp-photo-640x360.png`.
   - BotFather sets the default texts, shown in every language without its own. To show the Russian ones to users
     with Russian Telegram, run the Bot API yourself from the repository root (needs `curl` and `jq`; the token goes
     to curl through stdin, so it stays out of the shell history and the process list; each call prints `true`):

     ```bash
     read -rs BOT_TOKEN; P=docs/brand/telegram/bot-profile.json
     for f in Name:name ShortDescription:short_description Description:description; do
       printf 'url = "https://api.telegram.org/bot%s/setMy%s"\n' "$BOT_TOKEN" "${f%%:*}" |
         curl -sS -K - --data-urlencode language_code=ru \
           --data-urlencode "${f#*:}=$(jq -er ".ru.${f#*:}" "$P")" | jq -e .ok
     done
     ```

### 7. Add a runner

**Runner host:** Linux x86_64/aarch64, WSL2 with systemd, or macOS arm64/x86_64. It needs `git` and the AI CLIs
(`claude`, `codex`) on the PATH, and at least 2 GiB free per workspace. **Strict egress** (the sandbox that the CLI
permission bypass requires) is Linux/WSL2 only and needs kernel 5.12+, `nft` and unprivileged user namespaces.
Ubuntu 24.04 restricts those, so allow them persistently:

```bash
echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-kindevai-userns.conf
sudo sysctl --system
```

1. Download `kindevai-runner_<version>_<os>_<arch>.tar.gz` and `SHA256SUMS` from the same GitHub Release, verify,
   and install:
   ```bash
   sha256sum --check --ignore-missing SHA256SUMS
   tar -xzf kindevai-runner_*_linux_amd64.tar.gz ai-runner && sudo install -m 0755 ai-runner /usr/local/bin/
   ```
2. **Runners → Add runner** shows a one-time token (valid 10 minutes). Enroll with the commands at the top of
   `deploy/runner/kindevai-runner.service` (systemd, its own user), `kindevai-runner-user.service` (WSL2 or your own
   user) or `io.kindevai.runner.plist` (macOS). The token is read from a hidden prompt or a 0600 file, never from
   the command line.
3. Compare `ai-runner fingerprint` with the runner's card, then approve it for its projects.
4. Log the CLIs in where the runner runs, as the runner's user. The credentials stay on that host:
   ```bash
   claude setup-token                               # on any machine with a browser
   ai-runner cli-login claude                       # paste the token (hidden prompt)
   ai-runner cli-login codex -- --with-api-key      # Codex: an API key
   ai-runner cli-login status
   ```
5. `ai-runner test-network` checks the egress and whether strict egress is available. Start the service with
   `ai-runner run --measure-space` if the Storage page should show the runner's space consumers.

Remote runners connect only over `wss://` (the HTTPS profile). A WSL2 machine or a laptop that sleeps is fine: its
attempts become `UNKNOWN` and are reconciled when it returns. Upgrade the Control Plane and the runners together,
to the same release.

### 8. Network profiles and proxy (optional)

**Network** page (Owner): create a profile with the upstream proxy, the expected egress IP and the allowlist, and
assign it to the installation, a runner, a project or a CLI. A `strict` profile runs the CLI in the sandbox, which
can reach only the allowlist through the proxy. It needs `api.anthropic.com` for Claude Code, plus your GitHub and
YouTrack hosts. **Test Network** checks the profile from a runner before you rely on it.

### 9. Backups and upgrades

There are no automatic backups in the MVP. Copy the encryption key once and keep it apart from the archives (it
never changes), then export regularly. Export works while the Control Plane is serving:

```bash
docker compose cp cp:/data/secrets/master.key ./kindevai-master.key && chmod 600 ./kindevai-master.key  # once
f=backup-$(date +%F).tar.gz
docker compose exec cp ai-platform export --data-dir /data --out /data/$f --without-key
docker compose cp cp:/data/$f .
docker run --rm -v kindevai_data:/data busybox rm /data/$f   # the image has no shell: remove the copy from the volume
```

To move or restore, follow [Backup, restore and moving the installation](#backup-restore-and-moving-the-installation),
and never run two Control Planes on the same data. To upgrade, export first, then set the new `KINDEV_IMAGE_TAG`,
`docker compose --profile https pull && docker compose --profile https up -d`. Migrations run on start. Then
upgrade the runners.

### Checklist

- [ ] Compose HTTPS profile with a real `DOMAIN`, `KINDEV_PUBLIC_URL` set, plain-HTTP port not public.
- [ ] Encryption key exported and stored apart from the backups.
- [ ] **Rulesets or branch protection on `main`/`master` in every repository**, bypass only you, the App not
      listed. The Integrations page shows no protection warning.
- [ ] GitHub App with the minimal permissions, installed on the platform's repositories only. `dev` exists, and CI
      runs on pull requests.
- [ ] YouTrack integration user with a permanent token, projects linked, users mapped.
- [ ] Runners verified by fingerprint, approved for explicit projects, CLIs logged in, `ai-runner test-network` passes.
- [ ] Regular `ai-platform export`.

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
make test             # Go tests (-race -shuffle=on) + web unit tests + hook/script tests
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

## Brand

Kindevai Brand System v1.0: write the name as **Kindevai** in text (the lowercase *kindevai* exists only inside the
logo artwork; identifiers such as `kindevai-runner` keep their spelling). Palette: Honey `#FFB928` (main action),
Charcoal `#242420` (text), Cream `#F9F6EC` (background), Sage `#899680` (supporting graphics). Use
`docs/brand/lockup-light.svg` on light backgrounds and `lockup-dark.svg` on dark ones; do not stretch, recolour or
set the wordmark in a font. The web UI uses Nunito Sans and JetBrains Mono, bundled from Fontsource (SIL Open Font
License 1.1) — the browser never loads fonts from a third-party CDN.

## License

See [LICENSE](LICENSE).
