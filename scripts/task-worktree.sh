#!/usr/bin/env bash
# Create an isolated worktree for a YouTrack task so several Claude sessions can work in parallel.
#   scripts/task-worktree.sh KIN-12 runner-enrollment      -> .claude/worktrees/KIN-12, branch feature/KIN-12-runner-enrollment
#   scripts/task-worktree.sh --remove KIN-12               -> remove the worktree (branch is kept)
#   scripts/task-worktree.sh --prune [--dry-run]           -> remove worktrees, local branches and docker stacks of
#                                                             tasks whose own commits are merged into dev; worktrees with
#                                                             uncommitted/untracked changes and fresh branches are kept
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
  # A branch is pruned only when its work is provably on dev: a merged PR whose head is the branch tip, or a tip that
  # is an ancestor of origin/dev AND is a commit of the branch's own (not its creation point, not a commit on dev's
  # first-parent line — a fresh or fast-forwarded branch has nothing of its own and looks "merged" to git).
  # A worktree with uncommitted or untracked changes is never removed, and never with --force. Git-ignored files
  # (data/, node_modules, the copied .env, build output) count as disposable and go with a merged worktree.
  dry=${2:-}
  base=${BASE:-dev}
  git fetch --quiet --prune origin 2>/dev/null || true
  git rev-parse --verify --quiet "origin/$base" >/dev/null || { echo "origin/$base not found" >&2; exit 1; }
  devline=$(git rev-list --first-parent "origin/$base")
  for b in $(git branch --format='%(refname:short)' --list 'feature/KIN-*' | grep -E '^feature/KIN-[0-9]+-'); do
    id=$(sed -E 's#^feature/(KIN-[0-9]+)-.*#\1#' <<<"$b")
    tip=$(git rev-parse "refs/heads/$b")
    created=$(git config --get "branch.$b.taskbase" || git reflog show --format=%H "refs/heads/$b" 2>/dev/null | tail -1 || true)
    why=
    if command -v gh >/dev/null &&
      gh pr list --state merged --base "$base" --head "$b" --json headRefOid --jq '.[].headRefOid' 2>/dev/null |
      grep -qx "$tip"; then
      why="merged PR"
    elif [ -n "$created" ] && [ "$tip" != "$created" ] && ! grep -qx "$tip" <<<"$devline" &&
      git merge-base --is-ancestor "$tip" "origin/$base"; then
      why="merged into origin/$base"
    fi
    if [ -z "$why" ]; then
      [ "$dry" = "--dry-run" ] && echo "keep $b (not provably merged into $base)"
      continue
    fi
    wt=$(git worktree list --porcelain | awk -v ref="refs/heads/$b" '/^worktree /{w=substr($0,10)} $0=="branch " ref{print w}')
    if [ -n "$wt" ] && [ -d "$wt" ] && [ -n "$(git -C "$wt" status --porcelain 2>/dev/null || echo error)" ]; then
      echo "skip $b: worktree $wt has uncommitted or untracked changes"
      continue
    fi
    if [ "$dry" = "--dry-run" ]; then echo "would prune $b ($why)"; continue; fi
    echo "pruning $b ($why)"
    if [ -n "$wt" ] && [ -d "$wt" ]; then
      git worktree remove "$wt" || { echo "skip $b: could not remove $wt" >&2; continue; }
    fi
    down_stack "$id" || echo "  docker stack of $id not removed" >&2
    git branch -D "$b" >/dev/null
  done
  [ "$dry" = "--dry-run" ] || git worktree prune
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
    git config "branch.$branch.taskbase" "$start" # creation point: --prune never treats it as merged work
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
