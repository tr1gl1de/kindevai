# kindevai — AI Development Platform

[English version](README.md)

Self-hosted платформа, которая превращает задачи YouTrack в изменения кода. Код пишут AI CLI (Codex, Claude Code,
ZCode) на внешних runners. Проект в активной разработке; спецификация находится в Knowledge Base YouTrack
(проект `KIN`, корневая статья `KIN-A-1`).

## Архитектура кратко

- **Control Plane** — один Go-процесс (модульный монолит), SQLite в режиме WAL, React Dashboard встроен через
  `go:embed`. Цель — VPS 1 vCPU / 1 GB RAM. Без PostgreSQL, Redis, Temporal и RabbitMQ.
- **Runner** — Go-демон для Linux/WSL2/macOS; подключается к Control Plane исходящим WSS и запускает AI CLI
  в изолированных git worktree.
- **Web UI** — React + TypeScript, работает и как Telegram Mini App.

## Структура репозитория

| Путь | Содержимое |
|---|---|
| `cmd/ai-platform` | Бинарник Control Plane (`serve`, `version`) |
| `internal/` | Go-пакеты Control Plane |
| `web/` | Dashboard на React + TypeScript (Vite); `web/embed.go` встраивает сборку |
| `scripts/` | Вспомогательные скрипты (worktree задач, учёт времени) |
| `.claude/` | Правила, агенты и skills Claude Code для разработки репозитория |

## Требования

Go (версия в `go.mod`), Node.js 24+ и npm (только для сборки web UI), `make`.

## Быстрый старт

```bash
make build            # собирает web/dist/app, затем bin/ai-platform со встроенным UI
./bin/ai-platform serve   # http://127.0.0.1:8080
```

`ai-platform serve --addr 127.0.0.1:9000` меняет адрес. Бинарник, собранный без web UI, тоже запускается
и показывает страницу-заглушку.

### Каталог данных и ключ шифрования

При первом запуске `serve` создаёт `./data` (флаг `--data-dir`), SQLite в режиме WAL (`data/db/platform.db`),
применяет миграции схемы и генерирует ключ шифрования `data/secrets/master.key` (права 0600; `--key-file` позволяет
хранить его в другом месте). Следующие запуски используют существующие данные. **Сохраняйте ключ вместе с базой:**
если база есть, а ключа нет или он другой, Control Plane не запустится и никогда не создаст ключ взамен.
Каталог данных одновременно может использовать только один процесс.

## Разработка

```bash
make help             # список целей
make dev              # запустить Control Plane на порту этого worktree
npm --prefix web run dev   # Vite dev server, проксирует /api в Control Plane
make test             # Go-тесты (-race -shuffle=on) + unit-тесты web
make lint             # go vet + проверка типов TypeScript
make clean            # удалить артефакты сборки и тестов
```

Ветки создаются от `dev` как `feature/KIN-<n>-<slug>`; pull request — только в `dev`. `main` — только для владельца.

## Лицензия

См. [LICENSE](LICENSE).
