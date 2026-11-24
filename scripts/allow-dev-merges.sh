#!/usr/bin/env bash
# Owner-run (KIN-12): lets Claude merge its own PRs into dev. Moves the merge rule from "deny" to "allow" in
# .claude/settings.json and installs the guard hook that checks every merge's base branch on GitHub (dev only).
# Applies to the main checkout and, if present, the KIN-12 worktree so the change can be committed there.
set -euo pipefail
root=$(git -C "$(dirname "$0")" rev-parse --path-format=absolute --git-common-dir)/..
root=$(cd "$root" && pwd)
src=$root/.claude/worktrees/KIN-12/.claude/hooks/guard-git.sh
grep -q 'only dev is allowed' "$src" || { echo "new hook not found in $src" >&2; exit 1; }

for dir in "$root" "$root/.claude/worktrees/KIN-12"; do
  f=$dir/.claude/settings.json
  [ -f "$f" ] || continue
  jq '.permissions.deny -= ["Bash(gh pr merge:*)"]
      | .permissions.allow = ((.permissions.allow // []) - ["Bash(gh pr merge:*)"] + ["Bash(gh pr merge:*)"])' \
    "$f" >"$f.tmp" && mv "$f.tmp" "$f"
  [ "$dir" = "$root" ] && cp "$src" "$dir/.claude/hooks/guard-git.sh"
  echo "updated: $f"
done
echo "Done. Merges into dev are allowed; the hook still blocks main/master and unknown bases."
