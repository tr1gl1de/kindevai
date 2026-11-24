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
- Aggregates are the transaction boundary; repositories load and save whole aggregates.
- Shared kernel (`internal/platform/...`): DB, clock, ids, crypto, HTTP helpers — infrastructure only, no business rules.
- External API calls happen in adapters fed by the durable outbox, never inline in HTTP handlers.

## Tests per layer
- `domain`: fast table-driven unit tests, no fakes needed.
- `app`: use-case tests with in-memory fakes of the ports.
- `adapters`: real SQLite per test (`t.TempDir()`), `httptest` for HTTP.
