#!/usr/bin/env bash
# PostToolUse(Edit|Write) formatter: keeps formatting out of the model's job (and its tokens). Silent on success.
f=$(jq -r '.tool_input.file_path // empty')
[ -f "$f" ] || exit 0
case "$f" in
  *.go) command -v gofmt >/dev/null && gofmt -w "$f" ;;
  *.ts|*.tsx|*.css|*.json)
    root=$(git -C "$(dirname "$f")" rev-parse --show-toplevel 2>/dev/null)
    p="$root/web/node_modules/.bin/prettier"
    [ -x "$p" ] && "$p" --write --log-level silent "$f" ;;
esac
exit 0
