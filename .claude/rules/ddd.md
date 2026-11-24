---
paths:
  - "**/*.go"
---
# Domain-Driven Design — Go Control Plane and Runner

## Bounded contexts
The Control Plane is a modular monolith; each bounded context is one package tree under `internal/<context>/`.
Contexts come from the architecture spec (KB `KIN-A-13`): identity (auth, users, sessions, setup), projects
(projects, memberships), tasks, plans, approvals, policy, workflow, routing (agent router), runners (gateway,
enrollment), alerts, integrations (YouTrack, GitHub, Telegram), audit, knowledge, releases. Add a context only
when the spec introduces one; name it with the spec's ubiquitous language.

## Layers inside a context
```
internal/<context>/
  domain/   entities, value objects, aggregates, domain errors, repository interfaces — pure Go, no I/O imports
  app/      use cases (commands/queries); orchestrate domain + ports; own transactions
  adapters/ sqlite repositories, HTTP handlers, outbox consumers, external API clients
```
- Dependencies point inward: adapters → app → domain. `domain` imports only the stdlib (no `database/sql`,
  `net/http`, other contexts' adapters). Enforce invariants in constructors/methods, not in handlers.
- Contexts talk through the other context's `app` API or domain events — never through its tables or adapters.
- A use case that must change several contexts atomically (e.g. accepting an invitation creates an identity user
  and a projects membership) runs in `db.Transactor.InTx`; repositories always execute via `db.Conn(ctx, d)`, so
  the other context's app call joins the same transaction without touching its tables.
- Aggregates are the transaction boundary; repositories load and save whole aggregates.
- Shared kernel (`internal/platform/...`): DB, clock, ids, crypto, HTTP helpers — infrastructure only, no business rules.
- External API calls happen in adapters fed by the durable outbox, never inline in HTTP handlers.

## Tests per layer
- `domain`: fast table-driven unit tests, no fakes needed.
- `app`: use-case tests with in-memory fakes of the ports.
- `adapters`: real SQLite per test (`t.TempDir()`), `httptest` for HTTP.

## Runner
The Runner daemon (`cmd/ai-runner`, `internal/runner/...`) is its own deployable with contexts session (CLI
processes), workspace (git worktrees), connection (WSS to the Control Plane) and egress (proxy policy), using the
same domain/app/adapters layering. Wire messages shared with the Control Plane live in one protocol package; neither
side imports the other's domain.
