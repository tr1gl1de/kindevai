#!/usr/bin/env bash
# Create an isolated worktree for a YouTrack task so several Claude sessions can work in parallel.
#   scripts/task-worktree.sh KIN-12 runner-enrollment      -> .claude/worktrees/KIN-12, branch feature/KIN-12-runner-enrollment
#   scripts/task-worktree.sh --remove KIN-12               -> remove the worktree (branch is kept)
#   scripts/task-worktree.sh --prune [--dry-run]           -> remove worktrees, local branches and docker stacks of
#                                                             tasks already merged into dev (or whose remote branch is gone)
set -euo pipefail
root=$(git rev-parse --show-toplevel)
cd "$root"

down_stack() { # stop and delete a task's docker compose project (containers, networks, volumes)
  command -v docker >/dev/null || return 0
  local p; p=$(tr '[:upper:]' '[:lower:]' <<<"kindevai-$1")
  docker ps -aq --filter "label=com.docker.compose.project=$p" | grep -q . || return 0
  docker compose -p "$p" down -v --remove-orphans >/dev/null 2>&1 && echo "  docker stack $p removed"
}

if [ "${1:-}" = "--remove" ]; then
  id=${2:?issue id}
  down_stack "$id"
  git worktree remove ".claude/worktrees/$id"
  exit 0
fi

if [ "${1:-}" = "--prune" ]; then
  dry=${2:-}
  base=${BASE:-dev}
  git fetch --quiet --prune origin 2>/dev/null || true
  merged=$(git branch --format='%(refname:short)' --merged "origin/$base" 2>/dev/null || true)
  gone=$(git branch --format='%(refname:short) %(upstream:track)' | awk '$2=="[gone]"{print $1}')
  for b in $(printf '%s\n%s\n' "$merged" "$gone" | grep -E '^feature/KIN-[0-9]+-' | sort -u); do
    id=$(sed -E 's#^feature/(KIN-[0-9]+)-.*#\1#' <<<"$b")
    if [ "$dry" = "--dry-run" ]; then echo "would prune $b"; continue; fi
    echo "pruning $b"
    down_stack "$id"
    [ -d ".claude/worktrees/$id" ] && git worktree remove --force ".claude/worktrees/$id"
    git branch -D "$b" >/dev/null
  done
  git worktree prune
  exit 0
fi

id=${1:?usage: task-worktree.sh KIN-<n> <slug>}
slug=${2:?usage: task-worktree.sh KIN-<n> <slug>}
[[ "$id" =~ ^KIN-[0-9]+$ ]] || { echo "issue id must look like KIN-12" >&2; exit 1; }
branch="feature/${id}-${slug}"
dir=".claude/worktrees/${id}"
base=${BASE:-dev}

git fetch --quiet origin "$base" 2>/dev/null || true
start=$(git rev-parse --verify --quiet "origin/$base" || git rev-parse --verify --quiet "$base") \
  || { echo "base branch '$base' not found (create dev first, or BASE=main for bootstrap)" >&2; exit 1; }

if [ -d "$dir" ]; then
  echo "exists: $dir"
else
  if git rev-parse --verify --quiet "$branch" >/dev/null; then
    git worktree add "$dir" "$branch"
  else
    git worktree add -b "$branch" "$dir" "$start"
  fi
  # Copy git-ignored local files listed in .worktreeinclude (e.g. .env for YouTrack).
  if [ -f .worktreeinclude ]; then
    grep -Ev '^\s*(#|$)' .worktreeinclude | while read -r pat; do
      for f in $pat; do [ -e "$f" ] && mkdir -p "$dir/$(dirname "$f")" && cp --update=none "$f" "$dir/$f"; done
    done
  fi
  # Per-worktree runtime isolation so dev servers / compose stacks of parallel tasks don't collide.
  n=${id#KIN-}
  cat >"$dir/.worktree.env" <<EOF
KINDEV_PORT=$((8100 + n % 800))
KINDEV_DATA_DIR=./data
COMPOSE_PROJECT_NAME=kindevai-$(tr '[:upper:]' '[:lower:]' <<<"$id")
EOF
fi
echo "worktree: $root/$dir"
echo "branch:   $branch"
echo "next:     cd $dir && claude \"/task $id\""
