#!/usr/bin/env bash
# Time tracking for YouTrack tasks. Segments are stored per task in the shared git dir, so every worktree sees them.
#   task-time.sh start KIN-12 dev|test|docs|research   close the open segment (if any) and open a new one of that type
#   task-time.sh stop  KIN-12                          close the open segment
#   task-time.sh flush KIN-12 ["comment"]              stop, then post unposted segments to YouTrack as work items
#   task-time.sh status KIN-12                         show segments and totals
set -euo pipefail
YT=$(ls -d ~/.claude/plugins/cache/youtrack-plugin/youtrack/*/skills/youtrack/scripts/yt.py 2>/dev/null | tail -1)
store="$(git rev-parse --path-format=absolute --git-common-dir)/kindevai-time"
mkdir -p "$store"

cmd=${1:?usage: task-time.sh start|stop|flush|status KIN-<n> [type]}
id=${2:?issue id}
[[ "$id" =~ ^KIN-[0-9]+$ ]] || { echo "issue id must look like KIN-12" >&2; exit 1; }
f="$store/$id.tsv"   # columns: type  start_epoch  end_epoch|-  posted(0/1)
touch "$f"
now=$(date +%s)

yt_type() { case "$1" in dev) echo "Разработка";; test) echo "Тестирование";; docs) echo "Документирование";;
  research) echo "Исследование";; *) echo "unknown type $1 (dev|test|docs|research)" >&2; exit 1;; esac; }

close_open() { awk -v t="$now" 'BEGIN{OFS="\t"} $3=="-"{$3=t} {print}' "$f" >"$f.tmp" && mv "$f.tmp" "$f"; }

case "$cmd" in
  start)
    type=${3:?type: dev|test|docs|research}; yt_type "$type" >/dev/null
    close_open; printf '%s\t%s\t-\t0\n' "$type" "$now" >>"$f"; echo "$id: $type started" ;;
  stop) close_open; echo "$id: stopped" ;;
  status)
    awk -v n="$now" '{e=($3=="-")?n:$3; m=int((e-$2+59)/60); tot[$1]+=m; all+=m;
      printf "%-9s %s  %4d min %s%s\n",$1,strftime("%F %H:%M",$2),m,($3=="-")?"(open) ":"",($4)?"posted":""}
      END{for(k in tot) printf "total %-9s %d min\n",k,tot[k]; printf "total all       %d min\n",all}' "$f" ;;
  flush)
    close_open
    text=${3:-}
    # aggregate unposted closed segments by type, round each type up to whole minutes
    awk '$4==0 && $3!="-"{s[$1]+=$3-$2} END{for(k in s) print k, int((s[k]+59)/60)}' "$f" | while read -r type min; do
      [ "$min" -gt 0 ] || continue
      python3 "$YT" time add "$id" "${min}m" --type "$(yt_type "$type")" ${text:+-t "$text"} >/dev/null
      echo "$id: posted ${min}m $(yt_type "$type")"
    done
    awk 'BEGIN{OFS="\t"} $3!="-"{$4=1} {print}' "$f" >"$f.tmp" && mv "$f.tmp" "$f" ;;
  *) echo "unknown command $cmd" >&2; exit 1 ;;
esac
