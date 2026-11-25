#!/bin/sh
# Fake AI CLI for the cliagent tests: records its argv, environment and stdin into $FAKE_OUT, then replays the
# recorded stream $FAKE_STREAM and exits with $FAKE_EXIT. FAKE_MODE changes the ending:
#   hang     - after the stream, ignore SIGTERM and wait (the adapter must SIGKILL the group)
#   leftover - leave a grandchild holding stdout open after exiting
#   stderr   - print $FAKE_STDERR to stderr
printf '%s\n' "$@" > "$FAKE_OUT/argv"
env > "$FAKE_OUT/env"
pwd -P > "$FAKE_OUT/pwd"
cat > "$FAKE_OUT/stdin"
if [ -n "$FAKE_STREAM" ]; then cat "$FAKE_STREAM"; fi
case "$FAKE_MODE" in
hang)
	trap '' TERM
	while :; do sleep 1; done
	;;
leftover)
	sleep 300 &
	echo $! > "$FAKE_OUT/leftover"
	;;
stderr)
	printf '%s\n' "$FAKE_STDERR" >&2
	;;
esac
exit "${FAKE_EXIT:-0}"
