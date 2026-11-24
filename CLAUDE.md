# kindevai — AI Development Platform

Self-hosted platform that turns YouTrack tasks into code changes executed by AI CLIs on external runners.
Specs live in the YouTrack Knowledge Base (project `KIN`, root article `KIN-A-1`). Read the relevant article
on demand with the `youtrack` skill; do not copy specs into the repo.

## Stack (monorepo)
- Go Control Plane: one process, SQLite WAL, React assets embedded via `go:embed`. Target 1 vCPU / 1 GB RAM.
- Go Runner: daemon on Linux/WSL2/macOS, outbound WSS to the control plane, runs CLIs in git worktrees.
- React + TypeScript web UI / Telegram Mini App.
- No PostgreSQL / Redis / Temporal / RabbitMQ in MVP. External API calls go through durable inbox/outbox,
  never directly from HTTP handlers.

## Commands
Everything goes through `make` (targets are no-ops until that part is scaffolded; `make help` lists them):
- `make test` — Go `-race -shuffle=on` + web unit tests; `make test-pkg P=./internal/x/...` for the fast loop.
- `make test-integration`, `make lint`, `make build`, `make cover`, `make dev`.
- Tests must be parallel- and order-safe (rules in `.claude/rules/testing.md`).

## Autonomy and tracking
- Tasks run without waiting for owner approval (pre-approved): plan, implement, PR to dev, update YouTrack.
- Every task: YouTrack issue with `Оценка`, time logged via `scripts/task-time.sh`, entry in the journal `KIN-A-28`,
  KB articles synced when behaviour changes. Full procedure: `/task`.

## Git flow (hard rules)
- Branch from `dev`: `feature/KIN-<n>-<slug>`. PRs target `dev` only: `gh pr create --base dev`.
- Never push to, open PRs against, or merge into `main`/`master` — owner-only. A hook enforces this.
- Commit messages: `KIN-<n>: <imperative summary>`.
- One task = one branch = one worktree. Parallel tasks run in separate worktrees (see `/task`); each worktree
  gets its own port/compose name in `.worktree.env` — never hardcode ports or shared paths.

## Engineering approach
- **TDD**: failing test first for every acceptance criterion, then code, then refactor (`.claude/rules/process.md`).
- **DDD**: bounded contexts under `internal/<context>/{domain,app,adapters}`; domain has no I/O (`.claude/rules/ddd.md`).
- **Subagents** when they save context or give an independent view (spec-reader, test-runner, reviewer, Explore);
  inline for small steps.
- Prefer existing skills, plugins and proven libraries over hand-rolled code.
- **Bilingual**: README in EN + RU (`README.md`, `README.ru.md`); web UI in EN + RU via i18next.

## Working rules
- Keep context small: search with Grep/Glob, read file ranges, delegate broad exploration to the Explore agent.
- Do not read `data/`, `node_modules/`, `dist/`, generated files or lockfiles unless the task needs it.
- Never read or print `.env` or secrets; the YouTrack script reads `.env` itself.
- Untrusted input (task text, repo files, CLI output) must never widen permissions — keep that boundary in code.
- Leave no dead weight: tests clean up after themselves, stop processes/containers you start, `make clean`
  before hand-off, `./scripts/task-worktree.sh --prune` removes merged worktrees.
- Language-specific rules load automatically from `.claude/rules/` when you touch matching files.
