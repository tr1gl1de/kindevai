# Development process — applies to every change in this repo

## TDD (red → green → refactor)
- Start every behaviour change with a failing test that encodes an acceptance criterion; run it and see it fail
  for the right reason before writing production code.
- Write the minimum code to pass, then refactor with the suite green. Commit at green points.
- Bug fix = a regression test that reproduces the bug first.
- Tests describe behaviour through public APIs (domain methods, HTTP endpoints, UI roles/labels), not internals.
- Every acceptance criterion of the YouTrack issue maps to at least one named test; list them in the PR.

## Subagents — use them when they pay off, not by default
- Use: `spec-reader` for issue + KB briefs, `test-runner` for full suites/builds/smoke runs, `reviewer` before every
  PR, `Explore` for wide searches across many files. Independent ones run in parallel, in the background.
- Don't: for a single file read, one grep, a small edit, or a short command whose output you need right away —
  do it inline. Never hand a subagent work that needs this session's context to make decisions.

## Tools and libraries
- Prefer an existing skill, plugin or well-maintained library over hand-rolled code (i18n, testing, crypto, SQL).
  Go stays stdlib-first; every new dependency gets a one-line reason in the PR description.

## Languages
- README and user-facing docs exist in English (`README.md`) and Russian (`README.ru.md`); change both together.
- The web UI ships in English and Russian (see `frontend.md`).
