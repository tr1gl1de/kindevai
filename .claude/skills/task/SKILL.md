---
name: task
description: Pick up, implement and hand off a YouTrack task (KIN-<n>) end to end — worktree, spec, plan, code, tests, review, PR to dev, time tracking, docs and journal. Also creates new KIN tasks. Use when the user says "take/возьми KIN-12", "/task KIN-12", "создай задачу", or asks to finish/hand off a task.
argument-hint: KIN-<n> | new "<summary>"
---
# Task workflow ($ARGUMENTS)

Autonomy: the owner pre-approved this workflow for this repo. Do NOT wait for approval to plan, start, move Stage,
comment, log time, create tasks or update the journal/KB. Still stop and ask for: anything touching `main`/`master`,
production/infra secrets, deleting YouTrack data, or a genuine spec contradiction that blocks the work.

`YT` = `python3 ~/.claude/plugins/cache/youtrack-plugin/youtrack/*/skills/youtrack/scripts/yt.py`
`TT` = `./scripts/task-time.sh` (time segments; types `research|dev|test|docs`)

## 0. Creating tasks (`new`, or when work is discovered mid-task)
- Search first to avoid duplicates (`YT issue search "<keywords>"`).
- `YT issue create -s "<summary>" -d @desc.md --command "Priority Normal Stage Backlog Оценка <e.g. 2h>"`.
  Description: goal, acceptance criteria (testable bullets), links to KB articles (`KIN-A-*`).
- Out-of-scope work found during a task → new issue + `relates to <ID>` (or `subtask of <ID>`); do not expand the PR.

## 1. Workspace
- If the current branch is not `feature/<ID>-*`: `./scripts/task-worktree.sh <ID> <slug>` (slug = 2–4 kebab words),
  then continue inside that worktree (`cd` into it for all further commands). One task per session.

## 2. Understand
- `TT start <ID> research`. Delegate to `spec-reader`: issue + linked KB articles → brief.
- If the issue has no `Оценка`, set one (`YT issue command "Оценка 3h" <ID>`).
- `YT issue command "Stage Develop Assignee me" <ID>`.

## 3. Plan
- Write the plan as a short YouTrack comment (3–8 bullets) and proceed immediately.

## 4. Implement — `TT start <ID> dev`
- Follow `CLAUDE.md` and `.claude/rules/*`. Smallest change that satisfies the acceptance criteria.
- Commit in logical steps: `<ID>: <imperative summary>`.

## 5. Verify — `TT start <ID> test`
- Delegate to `test-runner`; fix and re-run until green. Never claim green without a run.
- Run `reviewer` on `git diff origin/dev...HEAD`; fix real findings, re-verify.

## 6. Hand off — `TT start <ID> docs`
- `make clean`; `git status` clean. Stop anything you started (servers, compose stacks, background jobs).
- `git push -u origin HEAD`; `gh pr create --base dev` — summary, `Closes <ID>`, verification, time spent.
- KB sync: if behaviour described in a KB article changed, `YT article get` it, edit only the affected section,
  `YT article update` with the full preserved body.
- Journal: fetch `KIN-A-28` right before writing, insert a new entry directly after `<!-- entries -->`, update.
  Entry: `**<ID> — <summary>** · <date> · PR #n · <spent> (оценка <est>)` + 2–4 bullets: done / verified / docs.
- `TT flush <ID> "<one-line summary>"` → posts time as work items. Use `TT status <ID>` for the totals.
- Final issue comment: what changed, PR link, how verified, spent vs estimate. Then `YT issue command "Stage Review" <ID>`.
- Never target `main`/`master`.

## 7. After merge into dev
- `YT issue command "Stage Done" <ID>` if not moved automatically.
- From the main checkout: `./scripts/task-worktree.sh --prune` (removes merged worktrees, branches, docker stacks).
