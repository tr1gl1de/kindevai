#!/usr/bin/env bash
# Tests for guard-git.sh. A fake `gh` on PATH answers `gh pr view` with the base branch from FAKE_BASE
# (empty = gh fails), so no network or real repository is needed. Run: .claude/hooks/guard-git.test.sh
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

# gh pr merge: allowed only when the PR's base is dev
run allow dev    'gh pr merge 12 --merge'
run allow dev    'gh pr merge https://github.com/o/r/pull/12 --squash --delete-branch'
run block main   'gh pr merge 12 --merge'
run block master 'gh pr merge 12'
run block ''     'gh pr merge 12 --merge'          # base unknown → fail closed
run block dev    'gh pr merge 12 --repo other/repo' # another repo cannot be checked against the local policy
run block dev    'gh pr merge 12 -R other/repo'
run block dev    'gh pr merge 12 && gh pr merge 13' # one check per command; chains are refused
run block dev    'cd x; gh pr merge 12'

# merge endpoints through the raw API are never allowed
run block dev 'gh api -X PUT repos/o/r/pulls/12/merge'
run block dev 'gh api repos/o/r/merges -f base=main -f head=x'

# existing rules keep working
run block dev 'git push origin main'
run block dev 'git push origin HEAD:refs/heads/main'
run block dev 'git status && git push origin master'
run allow dev 'git push -u origin feature/x && gh pr create --base dev --title "keep main owner-only"'
run block dev 'gh pr create --title x'
run block dev 'gh pr create --base main --title x'
run allow dev 'gh pr create --base dev --title x'
run block dev 'gh pr edit 12 --base main'
run allow dev 'git status'
run allow dev 'gh pr view 12'

[ $fail -eq 0 ] && echo "guard-git: ok"
exit $fail
