#!/usr/bin/env bash
# Tests for guard-git.sh. A fake `gh` on PATH answers `gh pr view` with the base branch from FAKE_BASE
# (empty = gh fails), so no network or real repository is needed. Run: make test-hooks
set -u
here=$(cd "$(dirname "$0")" && pwd)
hook="$here/guard-git.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/gh" <<'EOF'
#!/usr/bin/env bash
[ "$1 $2" = "pr view" ] || exit 1
[ -n "${FAKE_BASE:-}" ] || { echo "no pull requests found" >&2; exit 1; }
echo "$FAKE_BASE"
EOF
chmod +x "$tmp/gh"

fail=0
run() { # run <expect allow|block> <base> <command>
  local expect=$1 base=$2 cmd=$3 rc
  jq -n --arg c "$cmd" '{tool_input:{command:$c}}' |
    PATH="$tmp:$PATH" FAKE_BASE="$base" "$hook" >/dev/null 2>&1
  rc=$?
  if { [ "$expect" = allow ] && [ $rc -ne 0 ]; } || { [ "$expect" = block ] && [ $rc -ne 2 ]; }; then
    echo "FAIL: expected $expect (base=${base:-none}), got rc=$rc: $cmd"
    fail=1
  fi
}

# --- gh pr merge: allowed only as `gh pr merge <number> [method flags]` with a dev base ---
run allow dev    'gh pr merge 12 --merge'
run allow dev    'gh pr merge 12 --squash --delete-branch'
run allow dev    'gh pr merge 12 --merge && git pull'
run allow dev    'cd x; gh pr merge 12 --merge'
run block main   'gh pr merge 12 --merge'
run block master 'gh pr merge 12'
run block ''     'gh pr merge 12 --merge'                      # base unknown → fail closed
run block dev    'gh pr merge https://github.com/o/r/pull/12'  # URL may name another repo
run block dev    'gh pr merge feature/x'
run block dev    'gh pr merge --merge 12'
run block dev    'gh pr merge'
run block dev    'gh pr merge 12 --repo other/repo'
run block dev    'gh pr merge 12 -R other/repo'
run block dev    'gh pr merge 12 -Rother/repo'
run block dev    'gh pr merge 12 --admin'
run block dev    'gh pr merge 12 --subject x'
run block dev    'gh -R other/repo pr merge 12'
run allow dev    'gh pr "merge" 12'                              # quoting is normalised, then the base check applies
run block main   'gh pr "merge" 12'
run block main   "gh pr m''erge 12"
run block dev    'GH_REPO=other/repo gh pr merge 12'
run block dev    'env GH_REPO=other/repo gh pr merge 12'
run block dev    'echo $(gh pr merge 12)'
run block dev    'echo `gh pr merge 12`'
run block main   'bash -c "gh pr merge 12"'                      # nested shells are analysed recursively
run block main   'eval gh pr merge 12'
run block dev    'sudo gh pr merge 12'
run block dev    'gh pr merge 12 && gh pr merge 13'              # one merge per command keeps the audit trail simple
run block dev    'gh alias set pm "pr merge"'
run block dev    'gh pm 12'                                      # unknown top-level gh commands may be aliases

# --- raw API writes are never allowed; reads are ---
run block dev 'gh api -X PUT repos/o/r/pulls/12/merge'
run block dev 'gh api "repos/o/r/pulls/12/merge" -X PUT'
run block dev 'gh api -XPUT repos/o/r/pulls/12/merge'
run block dev 'gh api --method=PATCH repos/o/r/git/refs/heads/main -f sha=1'
run block dev 'gh api repos/o/r/merges -f base=main -f head=x'
run block dev "gh api graphql -f query='mutation{mergePullRequest(input:{}){clientMutationId}}'"
run allow dev 'gh api repos/o/r/pulls/12'
run allow dev 'gh api -X GET repos/o/r/actions/runs'

# --- git push ---
run block dev 'git push origin main'
run block dev 'git push origin HEAD:refs/heads/main'
run block dev 'git push origin +main'
run block dev "git push origin 'main'"
run block dev 'git -C . push origin main'
run block dev 'git push --all'
run block dev 'git push --mirror origin'
run block dev 'git status && git push origin master'
run allow dev 'git push -u origin feature/x && gh pr create --base dev --title "keep main owner-only"'
run allow dev 'git push -q origin feature/KIN-12-dev-merge-policy'
run allow dev 'git push origin --delete feature/x'

# --- gh pr create / edit ---
run block dev 'gh pr create --title x'
run block dev 'gh pr create --base main --title x'
run block dev 'gh pr create --base "main" --title x'
run block dev 'gh pr create -Bmain --title x'
run block dev 'gh pr create --base=master --title x'
run allow dev 'gh pr create --base dev --title x'
run allow dev 'gh pr create -B dev --title "merge main fixes"'
run block dev 'gh pr edit 12 --base main'
run block dev "gh pr edit 12 -B 'master'"

# --- local merge into main after checking it out ---
run block dev 'git checkout main && git merge dev'
run block dev 'git switch master; git merge dev'
run allow dev 'git checkout feature/x && git merge origin/dev'

# --- unrelated commands are untouched ---
run allow dev 'git status'
run allow dev 'gh pr view 12'
run allow dev 'git commit -q -F msg.txt'
run allow dev 'git commit -m "docs: explain gh pr merge (dev only)"'
run allow dev 'grep -n "gh pr merge" CLAUDE.md'
run allow dev 'git merge -q --no-edit origin/dev'
run allow dev 'ls'

[ $fail -eq 0 ] && echo "guard-git: ok"
exit $fail
