#!/usr/bin/env bash
# KIN-42: measures the Control Plane's CPU and memory on the MVP's target box (1 vCPU / 1 GB) under load.
# The Control Plane runs in a container limited with --cpus=1 --memory=1g; RUNNERS real ai-runner daemons on this host
# connect to it and run TASKS mock tasks (the mock agent, no AI), while a poller reads the Dashboard's task and
# runner lists like an open browser tab. docker stats is sampled every second; the summary goes to stdout and the
# samples to <out>/stats.csv. Everything it starts is stopped on exit.
#
#   scripts/measure-cp.sh [--runners 4] [--tasks 200] [--port <free port>] [--out <dir>]
#
# Needs docker, curl, jq, git and Go (it builds static binaries into <out>/bin). Linux/WSL2 only.
set -euo pipefail

runners=4 tasks=200 port=${KINDEV_PORT:-18080} out=""
while [ $# -gt 0 ]; do
  case $1 in
    --runners) runners=$2; shift 2 ;;
    --tasks) tasks=$2; shift 2 ;;
    --port) port=$2; shift 2 ;;
    --out) out=$2; shift 2 ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
for tool in docker curl jq git go; do command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }; done

root=$(cd "$(dirname "$0")/.." && pwd)
out=${out:-$(mktemp -d -t kindev-measure.XXXXXX)}
mkdir -p "$out/bin" "$out/data" "$out/runners"
name=kindev-measure-$$
image=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
base=http://127.0.0.1:$port
jar=$out/cookies
pids=()

cleanup() {
  for p in "${pids[@]}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  docker rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "building static binaries…"
(cd "$root" && CGO_ENABLED=0 go build -o "$out/bin/" ./cmd/ai-platform ./cmd/ai-runner)

# Host networking: the Control Plane listens on the host's loopback, so the runners connect over plain ws:// as a
# runner on the same host does (no port publishing in between); the limits are the container's.
docker run -d --name "$name" --cpus=1 --memory=1g --user "$(id -u):$(id -g)" --network host \
  -v "$out/bin/ai-platform:/ai-platform:ro" -v "$out/data:/data" \
  "$image" /ai-platform serve --addr "127.0.0.1:$port" --data-dir /data >/dev/null

stats() { docker stats --no-stream --format '{{.CPUPerc}},{{.MemUsage}}' "$name" 2>/dev/null | sed 's/ \/ .*//'; }
for _ in $(seq 100); do curl --noproxy '*' -m 2 -fsS "$base/api/health" >/dev/null 2>&1 && break; sleep 0.2; done
idle=$(stats)
echo "cpu,mem" >"$out/stats.csv"
(while true; do stats >>"$out/stats.csv"; sleep 1; done) &
pids+=($!)

api() { # api METHOD PATH [BODY]
  curl --noproxy '*' -m 30 -fsS -b "$jar" -X "$1" -H 'Content-Type: application/json' ${3:+--data "$3"} "$base$2"
}
# Only the login writes the cookie jar: the poller below reads it concurrently.
api POST /api/setup "$(jq -nc --arg t "$(cat "$out/data/secrets/setup-token")" \
  '{token:$t, login:"owner", password:"measure the control plane", platformName:"measure"}')" >/dev/null
curl --noproxy '*' -m 30 -fsS -c "$jar" -H 'Content-Type: application/json' \
  --data '{"login":"owner","password":"measure the control plane"}' "$base/api/auth/login" >/dev/null
project=$(api POST /api/projects '{"name":"Load","slug":"load"}' | jq -r .id)
api POST "/api/projects/$project/repositories" '{"owner":"acme","name":"load"}' >/dev/null

# The repository the mock tasks run in: a local bare repository with a dev branch, served to the runners as file://.
git init -q --bare -b dev "$out/git/acme/load.git"
seed=$out/git/seed
git init -q -b dev "$seed" && git -C "$seed" -c user.name=measure -c user.email=measure@localhost \
  commit -q --allow-empty -m init && git -C "$seed" push -q "$out/git/acme/load.git" dev

echo "enrolling $runners runners…"
for i in $(seq "$runners"); do
  dir=$out/runners/r$i
  mkdir -p "$dir" && chmod 700 "$dir"
  api POST /api/runners/enrollment-tokens '' | jq -r .token >"$dir.token"
  chmod 600 "$dir.token"
  "$out/bin/ai-runner" enroll --server "$base" --data-dir "$dir" --token-file "$dir.token" >/dev/null
done
for id in $(api GET /api/runners '' | jq -r '.[] | select(.status == "pending") | .id'); do
  api POST "/api/runners/$id/approve" "$(jq -nc --arg p "$project" '{projectIds:[$p]}')" >/dev/null
done
for i in $(seq "$runners"); do
  "$out/bin/ai-runner" run --data-dir "$out/runners/r$i" --git-base-url "file://$out/git" >"$out/runners/r$i.log" 2>&1 &
  pids+=($!)
done
for _ in $(seq 150); do
  [ "$(api GET /api/runners '' | jq '[.[] | select(.availability == "online")] | length')" -ge "$runners" ] && break
  sleep 0.2
done
connected=$(stats)

(while true; do api GET "/api/tasks?projectId=$project" '' >/dev/null; api GET /api/runners '' >/dev/null; sleep 1; done) &
pids+=($!)

echo "running $tasks mock tasks…"
start=$(date +%s)
: >"$out/tasks"
for i in $(seq "$tasks"); do
  id=$(api POST /api/tasks "$(jq -nc --arg p "$project" --arg t "load $i" '{projectId:$p, title:$t}')" | jq -r .task.id)
  api POST "/api/tasks/$id/mock-run" '{"scenario":"success"}' >/dev/null
  echo "$id" >>"$out/tasks"
done
# Open tasks: the list returns at most 200 tasks, so count each open state on its own.
open_tasks() {
  local n=0 st
  for st in CREATED ANALYZING AWAITING_APPROVAL READY RUNNING BLOCKED; do
    n=$(( n + $(api GET "/api/tasks?projectId=$project&state=$st" '' | jq '.tasks | length') ))
  done
  echo "$n"
}
while :; do
  left=$(open_tasks)
  [ "$left" -eq 0 ] && break
  if [ $(( ($(date +%s) - start) % 20 )) -lt 2 ]; then
    api GET "/api/tasks?projectId=$project" '' |
      jq -c '[.tasks[] | .state + (if .blockReason then "/" + .blockReason else "" end)] | group_by(.) | map({(.[0]): length}) | add' >&2
  fi
  [ $(( $(date +%s) - start )) -gt 1800 ] && { echo "tasks still open after 30 min: $left" >&2; break; }
  sleep 2
done
elapsed=$(( $(date +%s) - start ))
# Without a GitHub App a finished mock execution stays VERIFYING (no pull request to follow): its runner work is done.
done_tasks=0
while read -r id; do
  case $(api GET "/api/tasks/$id" '' | jq -r .task.state) in COMPLETED|VERIFYING) done_tasks=$((done_tasks + 1)) ;; esac
done <"$out/tasks"
final=$(stats)

peak_cpu=$(tail -n +2 "$out/stats.csv" | cut -d, -f1 | tr -d % | sort -n | tail -1)
peak_mem=$(tail -n +2 "$out/stats.csv" | cut -d, -f2 | awk '
  /GiB/ {v = $0 * 1024} /MiB/ {v = $0 + 0} /KiB/ {v = $0 / 1024} /[0-9]B$/ && !/iB/ {v = $0 / 1048576}
  v > m {m = v} END {printf "%.1fMiB", m}')
avg_cpu=$(tail -n +2 "$out/stats.csv" | cut -d, -f1 | tr -d % | awk '{s+=$1} END {if (NR) printf "%.1f", s/NR}')
db=$(du -sh "$out/data/db" | cut -f1)
printf '%s\n' \
  "Control Plane under --cpus=1 --memory=1g, $runners runners, $tasks mock tasks ($done_tasks executed) in ${elapsed}s" \
  "  idle after start:        $idle (cpu, memory)" \
  "  runners connected:       $connected" \
  "  whole run: peak cpu ${peak_cpu}%, mean cpu ${avg_cpu}%, peak memory $peak_mem" \
  "  after the load:          $final" \
  "  database directory:      $db" \
  "samples: $out/stats.csv"
