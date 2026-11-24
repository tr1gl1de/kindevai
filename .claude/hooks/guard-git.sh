#!/usr/bin/env bash
# PreToolUse(Bash) guard: main/master are owner-only (KIN-A-20); the agent may merge PRs into dev (KIN-12).
# The logic lives in guard_git.py (shlex tokenizing, fail closed); this wrapper keeps the settings.json path.
# If python3 is missing, every command is blocked rather than silently unguarded.
command -v python3 >/dev/null || { echo "BLOCKED by .claude/hooks/guard-git.sh: python3 not found" >&2; exit 2; }
exec python3 -B "$(dirname "$0")/guard_git.py" # -B: no __pycache__ in the repo
