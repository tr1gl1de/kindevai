---
paths:
  - "**/*.go"
  - "go.mod"
---
# Go rules
- Go stdlib first; every new dependency needs a reason in the PR description.
- Errors: wrap with `fmt.Errorf("doing x: %w", err)`; no panics outside `main`.
- Pass `context.Context` as the first argument to anything doing I/O.
- SQLite: one writer; schema changes only via numbered migrations; no silent key regeneration.
- Memory budget matters (1 GB box): stream CLI output with backpressure, never buffer unbounded logs.
- Tests: table-driven, next to the code (`_test.go`); run `go test ./<pkg>/...` for the touched package
  before the full suite. `gofmt` runs automatically after edits.
- Secrets never reach logs, API responses or error messages.
