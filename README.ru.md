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

### Первая настройка

Новая установка стартует в режиме setup. Для создания первого Owner нужен одноразовый **setup token**. Он никогда
не выводится в логи и не отдаётся по HTTP: он записывается в `data/secrets/setup-token` (права 0600; в логе только
путь). Прочитайте его на хосте (`ai-platform setup-token` или `docker compose exec cp ai-platform setup-token`)
и введите в мастере Dashboard вместе с логином Owner, паролем (от 12 символов) и названием платформы. Токен
действует час и обновляется автоматически; после настройки он удаляется, и повторить setup нельзя — в том числе
после рестарта.

### Вход

После настройки войдите с логином и паролем Owner. Сессии хранятся на сервере: браузер получает cookie HttpOnly,
SameSite=Strict (Secure при HTTPS); сессия завершается после 12 часов бездействия, не позже чем через 7 дней,
или при выходе. Все API, кроме health, setup и входа, требуют сессию; изменяющие запросы с чужого origin
отклоняются, попытки входа ограничены по адресу и по учётной записи.

### Проекты и доступ

Owner создаёт проекты и выдаёт участие (`project_admin` или `member`); в MVP участниками управляет только Owner.
Пользователь видит только свои проекты — любой другой отвечает `404`, так же как несуществующий. API:
`GET/POST /api/projects`, `GET /api/projects/{id}`, `GET /api/projects/{id}/members`,
`PUT/DELETE /api/projects/{id}/members/{login}`.

## Docker Compose

```bash
docker compose up -d                                        # один контейнер, http://127.0.0.1:8080
DOMAIN=ai.example.com docker compose --profile https up -d  # + Caddy с автоматическим HTTPS на 80/443
docker compose exec cp ai-platform setup-token        # setup token для первой настройки
```

`.env` не нужен. HTTP-порт публикуется только на loopback хоста; публичный доступ — через профиль `https`
(DNS для `DOMAIN` должен указывать на хост). Данные и ключ шифрования лежат в volume `data` — сохраняйте его
целиком. Образ distroless, работает от непривилегированного пользователя с read-only корневой ФС и встроенной
проверкой здоровья (`ai-platform health`).
За Caddy ограничения частоты по клиенту используют `X-Forwarded-For` только от сети compose
(`KINDEV_TRUSTED_PROXIES`, по умолчанию `172.29.240.0/24`; при конфликте задайте `KINDEV_SUBNET`). Базовые образы
закреплены по digest. Без `DOMAIN` профиль https отдаёт `https://localhost` с внутренним CA Caddy.

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

## CI и релизы

| Событие | Что выполняется |
|---|---|
| PR в `dev`, push в `dev` | `make lint` (go vet, golangci-lint, oxlint, tsc), тесты с покрытием, `make vuln` (govulncheck, npm audit), `make build`, проверка конфига GoReleaser, actionlint, сборка Docker-образа и смоук-тест |
| Зелёный push в `dev` | **Dev pre-release** `dev`: бинарники linux/darwin × amd64/arm64 + `SHA256SUMS`; образ `ghcr.io/tr1gl1de/kindevai:dev` |
| Тег `v*` на коммите `main` (только владелец) | **Prod-релиз**: проверка, что тег на `main`, тесты, GitHub Release (changelog, checksums, provenance) и образ `:X.Y.Z`, `:X.Y`, `:latest` через защищённый environment `production` |

Автоматизация никогда не пушит в `main` и не создаёт PR в `main`. Dependabot еженедельно предлагает обновления в `dev`.
Локально: `make lint vuln build`, `make release-snapshot` (нужен GoReleaser).

## Лицензия

См. [LICENSE](LICENSE).
