---
paths:
  - "**/*_test.go"
  - "**/testdata/**"
  - "**/*.test.ts"
  - "**/*.test.tsx"
  - "Makefile"
---
# Testing rules — every test must be safe to run in parallel, in any order, in several worktrees at once
- Go: call `t.Parallel()` in every test and subtest unless it truly mutates process-global state; then name the
  reason in a comment. Suite runs with `-race -shuffle=on`, so hidden order dependencies fail fast.
- No shared filesystem: use `t.TempDir()`; never write into the repo, `data/`, `/tmp/<fixed-name>` or `$HOME`.
- SQLite: a fresh DB per test (`filepath.Join(t.TempDir(), "test.db")`) with real migrations applied — no shared file.
- Network: `httptest.NewServer` / listen on `127.0.0.1:0`; never a fixed port. No real external calls (YouTrack,
  GitHub, Telegram) in unit tests — fake them behind interfaces.
- Time and randomness are injected (clock interface, seeded rand); no `time.Sleep` for synchronisation —
  use channels, `t.Context()` deadlines or polling with a timeout.
- Config via function parameters, not `os.Setenv`; if env is unavoidable use `t.Setenv` (that test cannot be parallel).
- Layers: unit tests next to code (fast, default `make test`); integration tests behind `//go:build integration`
  (`make test-integration`); fixtures in `testdata/`. Golden files updated only with `-update` flag.
- Frontend: Vitest + Testing Library, tests query by role/label, network mocked (MSW); no fixed ports.
- Leave nothing behind: every resource a test creates is released via `t.Cleanup` (servers, DBs, goroutines,
  child processes, containers). Child processes are killed with their process group; goroutines stop on
  `t.Context()` cancel. A test that leaves files outside `t.TempDir()` or a running process is a failing test.
- Integration tests needing Docker use the worktree's `COMPOSE_PROJECT_NAME` and are torn down with
  `docker compose down -v --remove-orphans` even on failure (`make test-integration` does this).
- A test for every acceptance criterion; a regression test for every bug fix.
