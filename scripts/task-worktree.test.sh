#!/usr/bin/env bash
# Tests for task-worktree.sh --prune. Each case builds a throwaway origin + clone under a temp dir, with fake `gh`
# (merged PRs come from $tmp/merged-prs: "<branch> <headRefOid>" lines) and fake `docker` on PATH, so no network,
# real repository or real worktree is touched. Run: make test-scripts
set -u
here=$(cd "$(dirname "$0")" && pwd)
script="$here/task-worktree.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin" "$tmp/home"
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
# fake: gh pr list --state merged --base <b> --head <branch> --json headRefOid --jq '.[].headRefOid'
[ "$1 $2" = "pr list" ] || exit 1
head=
while [ $# -gt 0 ]; do [ "$1" = --head ] && head=$2; shift; done
[ -f "$FAKE_GH_MERGED" ] || exit 0
awk -v b="$head" '$1==b{print $2}' "$FAKE_GH_MERGED"
EOF
printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/bin/docker"
chmod +x "$tmp/bin/gh" "$tmp/bin/docker"

export PATH="$tmp/bin:$PATH" HOME="$tmp/home" GIT_CONFIG_NOSYSTEM=1 FAKE_GH_MERGED="$tmp/merged-prs"
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
unset BASE

fail=0
check() { # check <description> <command...>
  local d=$1; shift
  if ! "$@"; then echo "FAIL: $d"; fail=1; fi
}

# setup: fresh origin (bare, default branch dev) + clone in $repo with one commit on dev.
setup() {
  local c=$tmp/case-$1
  rm -rf "$c"; mkdir -p "$c"; : >"$FAKE_GH_MERGED"
  git init -q --bare -b dev "$c/origin.git"
  git clone -q "$c/origin.git" "$c/repo" 2>/dev/null
  repo=$c/repo
  printf '.claude/worktrees/\n.worktree.env\ndata/\n' >"$repo/.gitignore"
  echo base >"$repo/file.txt"
  git -C "$repo" add -A && git -C "$repo" commit -qm base && git -C "$repo" push -q origin dev
}
new_task() { (cd "$repo" && "$script" "$1" "$2" >/dev/null 2>&1); } # new_task KIN-n slug
wt() { echo "$repo/.claude/worktrees/$1"; }
commit_in() { # commit_in <dir> <file>: commit a change and push the branch
  echo "$2" >"$1/$2" && git -C "$1" add -A && git -C "$1" commit -qm "$2" && git -C "$1" push -q -u origin HEAD 2>/dev/null
}
advance_dev() { # another task lands on origin/dev (merge commit), so dev moves on
  local o=$tmp/other; rm -rf "$o"
  git clone -q "$(git -C "$repo" remote get-url origin)" "$o" 2>/dev/null
  git -C "$o" checkout -qb other && echo x >"$o/other.txt" && git -C "$o" add -A && git -C "$o" commit -qm other
  git -C "$o" checkout -q dev && git -C "$o" merge -q --no-ff -m "merge other" other && git -C "$o" push -q origin dev
}
merge_into_dev() { # merge_into_dev <branch>: what `gh pr merge --merge` does on GitHub
  local o=$tmp/merger; rm -rf "$o"
  git clone -q "$(git -C "$repo" remote get-url origin)" "$o" 2>/dev/null
  git -C "$o" merge -q --no-ff -m "merge $1" "origin/$1" && git -C "$o" push -q origin dev
}
prune() { (cd "$repo" && "$script" --prune "$@" 2>&1); }
has_branch() { git -C "$repo" rev-parse --verify --quiet "refs/heads/$1" >/dev/null; }
no_branch() { ! has_branch "$1"; }
has_dir() { [ -d "$1" ]; }
no_dir() { [ ! -d "$1" ]; }
contains() { grep -q -- "$2" <<<"$1"; }

# 1. A just-created worktree (branch has no commits of its own, points at origin/dev) is kept.
setup fresh
new_task KIN-9001 fresh
out=$(prune)
check "fresh worktree kept" has_dir "$(wt KIN-9001)"
check "fresh branch kept" has_branch feature/KIN-9001-fresh

# 2. A fresh worktree with uncommitted and untracked work is kept with its files, and the skip is reported.
setup dirty-fresh
new_task KIN-9002 dirty
echo edit >>"$(wt KIN-9002)/file.txt"; echo new >"$(wt KIN-9002)/untracked.txt"
out=$(prune)
check "dirty fresh worktree kept" has_dir "$(wt KIN-9002)"
check "tracked edit survives" grep -q edit "$(wt KIN-9002)/file.txt"
check "untracked file survives" test -f "$(wt KIN-9002)/untracked.txt"

# 3. A fresh branch fast-forwarded to a newer origin/dev still has no commits of its own: kept.
setup ff
new_task KIN-9003 ff
advance_dev
git -C "$(wt KIN-9003)" fetch -q origin && git -C "$(wt KIN-9003)" merge -q --ff-only origin/dev
git -C "$repo" fetch -q origin
out=$(prune)
check "fast-forwarded fresh branch kept" has_branch feature/KIN-9003-ff
check "fast-forwarded fresh worktree kept" has_dir "$(wt KIN-9003)"

# 4. A branch merged into dev with a merge commit is pruned (worktree with only ignored files, and branch).
setup merged
new_task KIN-9004 merged
commit_in "$(wt KIN-9004)" feature.txt
mkdir -p "$(wt KIN-9004)/data" && echo db >"$(wt KIN-9004)/data/x.db"
merge_into_dev feature/KIN-9004-merged
out=$(prune)
check "merged worktree removed" no_dir "$(wt KIN-9004)"
check "merged branch deleted" no_branch feature/KIN-9004-merged
check "prune reported" contains "$out" "pruning feature/KIN-9004-merged"

# 5. A merged branch whose worktree has uncommitted work is skipped, reported, and nothing is forced.
setup merged-dirty
new_task KIN-9005 md
commit_in "$(wt KIN-9005)" feature.txt
merge_into_dev feature/KIN-9005-md
echo wip >"$(wt KIN-9005)/wip.txt"
out=$(prune)
check "dirty merged worktree kept" test -f "$(wt KIN-9005)/wip.txt"
check "dirty merged branch kept" has_branch feature/KIN-9005-md
check "skip reported" contains "$out" "skip feature/KIN-9005-md"

# 6. A squash-merged PR (tip not an ancestor of dev) is pruned when gh reports a merged PR with that head.
setup squash
new_task KIN-9006 sq
commit_in "$(wt KIN-9006)" feature.txt
echo "feature/KIN-9006-sq $(git -C "$repo" rev-parse feature/KIN-9006-sq)" >"$FAKE_GH_MERGED"
out=$(prune)
check "squash-merged worktree removed" no_dir "$(wt KIN-9006)"
check "squash-merged branch deleted" no_branch feature/KIN-9006-sq

# 7. A merged PR whose branch got further local commits afterwards is kept (the new commits are not merged).
setup after-merge
new_task KIN-9007 am
commit_in "$(wt KIN-9007)" feature.txt
echo "feature/KIN-9007-am $(git -C "$repo" rev-parse feature/KIN-9007-am)" >"$FAKE_GH_MERGED"
echo more >"$(wt KIN-9007)/more.txt" && git -C "$(wt KIN-9007)" add -A && git -C "$(wt KIN-9007)" commit -qm more
out=$(prune)
check "branch with commits after merged PR kept" has_branch feature/KIN-9007-am

# 8. An unmerged branch whose remote branch is gone (e.g. PR closed) is kept.
setup gone
new_task KIN-9008 gone
commit_in "$(wt KIN-9008)" feature.txt
git -C "$repo" push -q origin --delete feature/KIN-9008-gone
out=$(prune)
check "unmerged gone branch kept" has_branch feature/KIN-9008-gone
check "unmerged gone worktree kept" has_dir "$(wt KIN-9008)"

# 9. --dry-run reports but removes nothing.
setup dry
new_task KIN-9009 dry
commit_in "$(wt KIN-9009)" feature.txt
merge_into_dev feature/KIN-9009-dry
out=$(prune --dry-run)
check "dry-run reports" contains "$out" "would prune feature/KIN-9009-dry"
check "dry-run keeps worktree" has_dir "$(wt KIN-9009)"
check "dry-run keeps branch" has_branch feature/KIN-9009-dry

# 10. A merged branch without a worktree is deleted; a fresh one next to it is kept.
setup no-worktree
new_task KIN-9010 nw
commit_in "$(wt KIN-9010)" feature.txt
merge_into_dev feature/KIN-9010-nw
git -C "$repo" worktree remove "$(wt KIN-9010)"
new_task KIN-9011 fresh
out=$(prune)
check "merged branch without worktree deleted" no_branch feature/KIN-9010-nw
check "fresh neighbour kept" has_dir "$(wt KIN-9011)"

# 11. A branch with unknown creation point (no taskbase, reflog expired) pointing at an already merged feature
#     commit has no commits of its own that we can prove: kept (fail closed).
setup unknown-base
new_task KIN-9012 merged
commit_in "$(wt KIN-9012)" feature.txt
merge_into_dev feature/KIN-9012-merged
git -C "$repo" branch feature/KIN-9013-copy feature/KIN-9012-merged
git -C "$repo" config --unset branch.feature/KIN-9013-copy.taskbase 2>/dev/null
git -C "$repo" reflog expire --expire=now --expire-unreachable=now --all
out=$(prune --dry-run)
check "unknown creation point kept" contains "$out" "keep feature/KIN-9013-copy"
check "merged branch still pruned" contains "$out" "would prune feature/KIN-9012-merged"

[ $fail -eq 0 ] && echo "task-worktree.sh tests: ok"
exit $fail
