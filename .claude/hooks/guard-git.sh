#!/usr/bin/env bash
# PreToolUse(Bash) guard: main/master are owner-only (KIN-A-20).
# Blocks agent pushes, PRs and merges that target main/master. Exit 2 = block, stderr goes back to Claude.
cmd=$(jq -r '.tool_input.command // empty')
[ -z "$cmd" ] && exit 0

block() { echo "BLOCKED by .claude/hooks/guard-git.sh: $1. main/master are owner-only; target dev instead." >&2; exit 2; }
protected='(main|master)'

# git push ... main | HEAD:main | --force to protected, or pushing while checked out on main
if grep -Eq '(^|[;&|[:space:]])git[[:space:]]+push' <<<"$cmd"; then
  grep -Eq "git[[:space:]]+push.*([[:space:]:]|refs/heads/)${protected}([[:space:]]|$)" <<<"$cmd" && block "push to main/master"
  branch=$(git branch --show-current 2>/dev/null)
  [[ "$branch" =~ ^(main|master)$ ]] && block "push from checked-out $branch"
fi

# gh pr create must explicitly target dev (default base is main)
if grep -Eq 'gh[[:space:]]+pr[[:space:]]+create' <<<"$cmd"; then
  grep -Eq -- "(--base|-B)[[:space:]=]+${protected}([[:space:]]|$)" <<<"$cmd" && block "PR into main/master"
  grep -Eq -- '(--base|-B)[[:space:]=]+' <<<"$cmd" || block "gh pr create without --base (defaults to main)"
fi

# gh pr merge / gh api merges are owner decisions for main; allow dev merges only via explicit policy
grep -Eq "gh[[:space:]]+pr[[:space:]]+edit.*--base[[:space:]=]+${protected}" <<<"$cmd" && block "retarget PR to main/master"
grep -Eq "git[[:space:]]+(checkout|switch)[[:space:]].*&&.*git[[:space:]]+merge" <<<"$cmd" && \
  grep -Eq "(checkout|switch)[[:space:]]+${protected}([[:space:]]|$)" <<<"$cmd" && block "merge into main/master"

exit 0
