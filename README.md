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

## License

See [LICENSE](LICENSE).
