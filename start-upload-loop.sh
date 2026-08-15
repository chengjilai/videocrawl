#!/usr/bin/env bash
# start-upload-loop.sh — start/stop/status the lab→aturing sync + bilibili
# upload loop (run on aturing, where the bilibili web session lives).
#
# The loop (started with nohup so it survives this script exiting):
#   while true; do sync-upload.sh; sleep 1200; done
#   * one sync-upload.sh round every $SLEEP seconds (default 1200 = 20 min;
#     bilibili's preupload endpoint rate-limits to ~1 upload per 20 min and
#     each round uploads at most one video: --limit 1);
#   * every round's output goes to $LOG (~/upload-loop.log) with timestamps.
#
# stop matches processes by command line (pkill -f), so no pidfile is
# needed. The patterns (bracket tricks) and why:
#   * 'sync-upload[.]sh' — matches the loop (its cmdline embeds the
#     sync-upload.sh path) and any in-flight round; the [.] trick keeps
#     the pattern from matching itself. This script's own argv
#     (bash .../start-upload-loop.sh stop) contains neither "sync-upload"
#     nor the pattern text, so it is never self-matched.
#   * 'upload-loop[.]log' — belt-and-suspenders for any loop variant whose
#     cmdline embeds the log path. Plain 'upload-loop' is deliberately NOT
#     used: this script's own argv contains "start-upload-loop.sh", which
#     already contains the plain text "upload-loop", so even the bracketed
#     form upload[-]loop self-matches (verified 2026-08-14: the stopper
#     was killed by its own pkill). The [.]log pattern does not appear in
#     this script's argv, so it is safe.
#
# A SIGTERM'd in-flight round dies safely: the video row stays status=done
# (the upload marks 'uploaded' only on a parsed "SUBMIT OK" bvid) and is
# retried next round.
#
# Usage:
#   start-upload-loop.sh [start|stop|status]     (default: start)
#
# Config via env (defaults):
#   SYNC_SCRIPT   $HOME/videocrawl/sync-upload.sh
#   SLEEP         1200       seconds between rounds (bilibili 601 gate)
#   LOG           $HOME/upload-loop.log
#   DRY_RUN       0|1        passed through to sync-upload.sh (1 = --dry-run)
#   UPLOAD_LIMIT  1          passed through to sync-upload.sh
set -euo pipefail

CMD="${1:-start}"

SYNC_SCRIPT="${SYNC_SCRIPT:-$HOME/videocrawl/sync-upload.sh}"
SLEEP="${SLEEP:-1200}"
LOG="${LOG:-$HOME/upload-loop.log}"
DRY_RUN="${DRY_RUN:-0}"
UPLOAD_LIMIT="${UPLOAD_LIMIT:-1}"

log() { echo "[$(date '+%F %T')] $*" | tee -a "$LOG"; }

loop_alive() { pgrep -f 'sync-upload[.]sh' >/dev/null 2>&1; }

cmd_start() {
  if loop_alive; then
    echo "upload loop already running; use 'start-upload-loop.sh stop' first" >&2
    exit 1
  fi
  [ -x "$SYNC_SCRIPT" ] || { echo "error: $SYNC_SCRIPT not executable" >&2; exit 1; }
  export DRY_RUN UPLOAD_LIMIT   # visible to each sync-upload.sh round
  # shellcheck disable=SC2086  # $SYNC_SCRIPT/$SLEEP deliberately word-split into bash -c
  nohup bash -c 'while true; do '"$SYNC_SCRIPT"'; sleep '"$SLEEP"'; done' \
    >> "$LOG" 2>&1 < /dev/null &
  sleep 1
  if loop_alive; then
    log "upload loop started (pid $(pgrep -f 'sync-upload[.]sh' | head -1 || true)); sleep ${SLEEP}s, limit $UPLOAD_LIMIT, dry-run $DRY_RUN"
  else
    log "WARNING: upload loop failed to start (see $LOG)"
  fi
}

cmd_stop() {
  log "stopping upload loop: pkill -f 'sync-upload[.]sh' (loop + in-flight round), pkill -f 'upload-loop[.]log' (leftover variants)"
  pkill -f 'sync-upload[.]sh' 2>/dev/null || true
  pkill -f 'upload-loop[.]log' 2>/dev/null || true
  sleep 2
  if loop_alive; then
    log "WARNING: upload loop still alive after SIGTERM; use: pkill -KILL -f 'sync-upload[.]sh'"
    exit 1
  fi
  log "upload loop stopped"
}

cmd_status() {
  if loop_alive; then
    echo "upload loop: RUNNING"
    pgrep -af 'sync-upload[.]sh' | sed 's/^/  /' || true
    echo "log: $LOG ($(wc -l < "$LOG" 2>/dev/null || echo 0) lines)"
    tail -n 3 "$LOG" 2>/dev/null || true
  else
    echo "upload loop: DOWN"
  fi
}

case "$CMD" in
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  status) cmd_status ;;
  *) echo "usage: $0 [start|stop|status]" >&2; exit 2 ;;
esac
