#!/usr/bin/env bash
# music-upload-loop.sh — one round: sync lab's staged PD music videos +
# merge lab's post state, then upload at most one item to bilibili.
#
# The music staging runs on lab (post-loop --no-upload); uploads must run
# here (aturing) where the bilibili web session lives. bilibili's preupload
# endpoint rate-limits to ~1 per 20 min, so one round = one upload, and the
# loop sleeps 25 min between rounds.
#
# State: lab's ~/.videocrawl/repost-state.jsonl holds the downloaded items;
# the local one holds the posted history (dedup). Merge = union by id,
# non-queued status wins — so lab's "downloaded" items appear here without
# losing the local "posted" dedup entries.
set -u
LOOP_PIDFILE="$HOME/.videocrawl/music-upload-loop.pid"
LOG="$HOME/.videocrawl/music-upload-loop.log"

round() {
  echo "=== round $(date +%H:%M:%S) ==="
  rsync -a --ignore-existing lab:~/Videos/Post/ "$HOME/Videos/Post/" || true
  rsync -a lab:~/.videocrawl/repost-state.jsonl "$HOME/.videocrawl/lab-state.jsonl" 2>/dev/null || true
  python3 - "$HOME/.videocrawl/repost-state.jsonl" "$HOME/.videocrawl/lab-state.jsonl" <<'EOF' || true
import json, sys
out = {}
for p in sys.argv[1:]:
    try:
        for line in open(p):
            line = line.strip()
            if not line:
                continue
            it = json.loads(line)
            cur = out.get(it["id"])
            if cur is None or (it.get("status") in ("downloaded", "posted") and cur.get("status") == "queued"):
                out[it["id"]] = it
    except FileNotFoundError:
        pass
with open(sys.argv[1], "w") as f:
    f.write("\n".join(json.dumps(out[k]) for k in sorted(out)) + "\n")
EOF
  cd "$HOME/videocrawl" || return 1
  ./videocrawl post-loop --upload-only --limit 1 --check-bili
}

case "${1:-}" in
  start)
    setsid nohup "$0" loop >> "$LOG" 2>&1 < /dev/null &
    echo $! > "$LOOP_PIDFILE"
    echo "started pid $(cat "$LOOP_PIDFILE") → $LOG"
    ;;
  stop)
    # pidfile first; the bracket pattern (safe since the status branch) is
    # the fallback for a stale pidfile — setsid forks under job control, so
    # $! can record a short-lived parent while the loop itself survives.
    if [ -f "$LOOP_PIDFILE" ]; then
      kill "$(cat "$LOOP_PIDFILE")" 2>/dev/null
      rm -f "$LOOP_PIDFILE"
    fi
    if pgrep -f 'music-upload-loop[.]sh loop' >/dev/null; then
      pkill -f 'music-upload-loop[.]sh loop' 2>/dev/null
      echo stopped
    else
      echo "not running (no pidfile)"
    fi
    ;;
  loop)
    while true; do round >> "$LOG" 2>&1; sleep 1500; done
    ;;
  status)
    pgrep -af 'music-upload-loop[.]sh loop' | grep -v grep | head -2
    ;;
esac
