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
| `cmd/ai-runner` | Демон раннера (`enroll`, `fingerprint`, `run`, `version`) |
| `internal/` | Go-пакеты Control Plane (`internal/runner/...` — раннер, `internal/protocol` — общие wire-сообщения) |
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
или при выходе. Все API, кроме health, setup, входа, принятия приглашения и регистрации раннера, требуют сессию; изменяющие запросы с чужого origin
отклоняются, попытки входа ограничены по адресу и по учётной записи.

### Проекты и доступ

Owner создаёт проекты и выдаёт участие (`project_admin` или `member`); в MVP участниками управляет только Owner.
Пользователь видит только свои проекты — любой другой отвечает `404`, так же как несуществующий. API:
`GET/POST /api/projects`, `GET /api/projects/{id}`, `GET /api/projects/{id}/members`,
`PUT/DELETE /api/projects/{id}/members/{login}`.

### Раннеры

Owner добавляет машину на странице **Раннеры**: **Добавить раннер** показывает одноразовый токен регистрации (256 бит,
действует 10 минут, хранится только SHA-256) и команду для этой машины — `ai-runner enroll --server <URL>`; токен она
запрашивает через stdin, поэтому он не попадает в argv и историю shell. Раннер создаёт пару ключей Ed25519 и регистрирует
открытый ключ через публичный `POST /api/runners/enroll` с ограничением частоты; раннер получает статус **ожидает
одобрения** и не может подключаться и получать работу. Owner сверяет отпечаток в стиле OpenSSH (`SHA256:…`, как у
`ssh-keygen -lf`) с выводом `ai-runner fingerprint` на машине и одобряет раннер для явного списка проектов; позже
можно менять проекты, отключать/включать и отзывать раннер (отзыв окончателен; повторная регистрация — с новым ключом).
Удалённым раннерам нужен HTTPS (профиль Caddy); обычный HTTP — только для раннера на том же хосте. API Owner:
`POST/GET /api/runners/enrollment-tokens`, `POST /api/runners/enrollment-tokens/{id}/revoke`, `GET /api/runners`,
`GET /api/runners/{id}`, `POST /api/runners/{id}/{approve|disable|enable|revoke}`, `PUT /api/runners/{id}/projects`.

### Демон раннера (`ai-runner`)

`ai-runner` — второй бинарник (`cmd/ai-runner`, linux/darwin × amd64/arm64, архивы релиза
`kindevai-runner_<версия>_<os>_<arch>.tar.gz`). Команды: `enroll`, `fingerprint`, `run`, `version`.

```bash
ai-runner enroll --server https://kindevai.example.com   # спросит токен без отображения ввода
ai-runner fingerprint                                    # сверьте с карточкой раннера перед одобрением
```

- Токен читается из скрытого prompt, из stdin (`… | ai-runner enroll --server …`) или из `--token-file <путь>`
  (файл с правами 0600); `--token` и позиционные аргументы отклоняются.
- Ключ Ed25519 создаётся один раз при первом `enroll` (`keys/ed25519.pem`, PKCS#8, права 0600) и никогда не
  заменяется: повреждённый ключ — ошибка, а ключ или конфиг, доступные другим пользователям, отклоняются (`chmod 600`).
  Повтор с тем же токеном после обрыва использует тот же ключ. Fingerprint, который вернул Control Plane, обязан
  совпасть с локальным ключом, иначе регистрация не сохраняется.
- Сертификаты TLS проверяются всегда; флага insecure нет. `http://` работает только для Control Plane на
  `localhost`/loopback, и соединение по http возможно только с loopback-адресом. Переменные прокси (`HTTPS_PROXY`) для канала управления игнорируются, редиректы не выполняются.
- Каталог данных (`--data-dir`): `/var/lib/kindevai-runner` под root, иначе `$XDG_DATA_HOME/kindevai-runner` или
  `~/.local/share/kindevai-runner`. В нём `config.json` (сервер, id раннера, fingerprint; 0600), `keys/` и
  `repos/ workspaces/ sessions/ events/ proxy/`, все с правами 0700; одновременно — только один процесс (lock-файл).
- Сервисы: `deploy/runner/kindevai-runner.service` (системный сервис systemd под отдельным пользователем),
  `kindevai-runner-user.service` (пользовательский сервис systemd, например в WSL2, чтобы AI CLI использовали ваши
  логины) и `io.kindevai.runner.plist` (агент launchd для macOS); шаги установки — в каждом файле. Пока `ai-runner run`
  проверяет ключ и регистрацию и ждёт; WSS-подключение к Control Plane появится в KIN-19.

### Интеграции и внешние вызовы

Вызовы YouTrack, GitHub, Telegram и runner никогда не выполняются внутри API-запроса. Исходящие изменения
записываются в durable outbox в той же транзакции SQLite, что и вызвавшее их изменение, и доставляются в фоне
с повторами (экспоненциальный backoff с jitter, после 10 попыток — `dead`); входящие webhooks сохраняются в inbox,
дедуплицируются по delivery id и обрабатываются асинхронно. Поэтому медленный или недоступный сервис не блокирует
dashboard. Owner видит состояние каждого адаптера (`ok`, `degraded`, `circuit_open`) и размеры очередей в
`GET /api/integrations/status`, а `dead`-сообщения — в `GET /api/messaging/{outbox|inbox}/dead`
(`POST …/dead/{id}/retry` ставит сообщение в очередь снова). Доставленные сообщения удаляются через 7 дней, `dead` — через 30.

### Журнал аудита

Действия, важные для безопасности, пишутся в журнал аудита (только добавление) в той же транзакции, что и само
действие: завершение первичной настройки; вход, неудачный вход (адрес и причина; логин — только если такая учётная
запись существует, пароль — никогда; попытки, отклонённые ограничителем частоты, не пишутся) и выход; создание,
отключение и включение учётной записи, сброс и смена пароля; создание, отзыв и принятие приглашений; создание проекта,
назначение и снятие роли участника; создание и отзыв токенов регистрации раннеров, регистрация раннера (или отклонённая попытка с известным токеном), одобрение, смена проектов, отключение, включение и отзыв раннера (следующие контексты — approvals, интеграции — добавят свои события). Записи нельзя изменить или
удалить (это запрещают триггеры БД, и API для этого нет); значения, похожие на пароли и токены, сохраняются как
`[redacted]`. Owner читает журнал на странице **Аудит** или через
`GET /api/audit?actor=&project=&kind=&from=&to=&before=&limit=` (сначала новые, `next` — курсор следующей страницы);
остальным пользователям — `403`. Автоочистки в MVP нет.

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
| PR или push в `dev` / `main` | `make lint` (go vet, golangci-lint, oxlint, tsc), тесты с покрытием, `make vuln` (govulncheck, npm audit), `make build`, проверка конфига GoReleaser, actionlint, сборка Docker-образа и смоук-тест |
| Зелёный push в `dev` | **Dev-сборка**: бинарники Control Plane и раннера linux/darwin × amd64/arm64 + `SHA256SUMS` как артефакт прогона CI `ai-platform-dev-<sha>` (хранится 14 дней); образ `ghcr.io/tr1gl1de/kindevai-dev:latest` (отдельный dev-пакет) |
| Тег `v*` на коммите `main` (только владелец) | **Prod-релиз**: проверка, что тег на `main`, тесты, GitHub Release (changelog, checksums, provenance) и образ `:X.Y.Z`, `:X.Y`, `:latest` через защищённый environment `production` |

Автоматизация никогда не пушит в `main` и не создаёт PR в `main`. Dependabot еженедельно предлагает обновления в `dev`.
Все сторонние actions и образы закреплены по SHA/digest.

**Обязательные настройки репозитория (владелец, один раз)** — workflow сами их обеспечить не могут:
1. Rulesets: защитить `main` (push/merge только владелец) и теги `v*` (создавать, менять, удалять — только владелец).
2. Environment `production`: обязательный ревьюер — владелец, без обхода администраторами, deployment tags `v*`.
3. Пакет `ghcr.io/tr1gl1de/kindevai`: права записи из Actions только для этого репозитория (dev-образы — в
   `kindevai-dev`).
Локально: `make lint vuln build`, `make release-snapshot` (нужен GoReleaser).

## Лицензия

См. [LICENSE](LICENSE).
