#!/usr/bin/env bash
# start-loop.sh — keep the videocrawl crawl-loop running unattended.
#
# Topology (default, matches README "Lab deployment"):
#   * this script runs on the machine that hosts the smart-proxy (aturing,
#     the box this repo lives on);
#   * it (re)establishes the reverse tunnel so the crawl machine can reach
#     the proxy at 127.0.0.1:$TUNNEL_PORT:
#         ssh -R 18888:127.0.0.1:8888 <TUNNEL_HOST>
#     (ExitOnForwardFailure + ServerAlive so a dead tunnel is detected and
#     replaced; the port probe runs from $TUNNEL_HOST, where it binds);
#   * it starts the crawl-loop with nohup on $LOOP_HOST (default: the same
#     host as the tunnel = lab), with
#     VIDEOCRAWL_PROXY=http://127.0.0.1:$TUNNEL_PORT;
#   * a watchdog loop restarts either piece when it dies, forever.
#
# Single-box mode: LOOP_HOST=local runs the crawl-loop on this machine and
# skips the tunnel entirely (the proxy is already at 127.0.0.1:8888).
#
# Usage:
#   start-loop.sh [start|stop|status]      (default: start)
#
# Config via env (defaults shown):
#   LOOP_HOST        lab|local    host running the crawl-loop (default lab)
#   TUNNEL_HOST      lab          ssh target for the reverse tunnel; should
#                                 equal LOOP_HOST (the proxy URL used by the
#                                 loop is 127.0.0.1:$TUNNEL_PORT on that host)
#   TUNNEL_PORT      18888        bind port on the tunnel's remote side
#   TUNNEL_LOCAL     127.0.0.1:8888  local smart-proxy endpoint
#   LOOP_DIR         ~/videocrawl dir holding the videocrawl binary (on LOOP_HOST)
#   LOOP_LOG         ~/videocrawl-loop.log
#   LOOP_ENV         "VIDEOCRAWL_PROXY=http://127.0.0.1:18888 ..."  (extra env)
#   LOOP_ARGS        "--every 3600 --limit 20 --workers 8 --max-time 3600"
#   LOG_DIR          ~/logs      this watchdog's own pid/log
#   WATCH_INTERVAL   30          watchdog poll seconds
#
# The crawl-loop is tracked by its command line (pgrep -f 'videocrawl
# crawl[-]loop' — the bracket trick prevents the killer shell from matching
# its own cmdline), so SIGTERM reliably reaches the Go process even through
# the nohup/env wrapper chain.
#
# Needs passwordless ssh to $TUNNEL_HOST/$LOOP_HOST (BatchMode=yes) and a
# bash login shell on the remote (for the /dev/tcp port probe).
set -u

CMD="${1:-start}"

LOOP_HOST="${LOOP_HOST:-lab}"
TUNNEL_HOST="${TUNNEL_HOST:-$LOOP_HOST}"
TUNNEL_PORT="${TUNNEL_PORT:-18888}"
TUNNEL_LOCAL="${TUNNEL_LOCAL:-127.0.0.1:8888}"
LOOP_DIR="${LOOP_DIR:-$HOME/videocrawl}"
LOOP_LOG="${LOOP_LOG:-$HOME/videocrawl-loop.log}"
LOG_DIR="${LOG_DIR:-$HOME/logs}"
WATCH_INTERVAL="${WATCH_INTERVAL:-30}"
WATCH_PID="$LOG_DIR/videocrawl-watchdog.pid"
WATCH_LOG="$LOG_DIR/videocrawl-watchdog.log"
mkdir -p "$LOG_DIR"

if [ "$LOOP_HOST" = local ]; then
  LOOP_ENV="${LOOP_ENV:-VIDEOCRAWL_PROXY=http://127.0.0.1:8888}"
else
  LOOP_ENV="${LOOP_ENV:-VIDEOCRAWL_PROXY=http://127.0.0.1:$TUNNEL_PORT}"
fi
LOOP_ARGS="${LOOP_ARGS:---every 3600 --limit 20 --workers 8 --max-time 3600}"

is_local() { [ "$LOOP_HOST" = local ]; }

log() { echo "[$(date '+%F %T')] $*" | tee -a "$WATCH_LOG"; }

# run_loop CMD — run CMD on the loop host (or locally).
run_loop() {
  if is_local; then
    bash -c "$*"
  else
    # shellcheck disable=SC2029
    ssh -o BatchMode=yes -o ConnectTimeout=10 "$LOOP_HOST" "$*"
  fi
}

# port_open HOST PORT — true if 127.0.0.1:PORT accepts connections on HOST.
port_open() {
  local h="$1" p="$2"
  if [ "$h" = local ]; then
    timeout 3 bash -c "exec 3<>/dev/tcp/127.0.0.1/$p" 2>/dev/null
  else
    ssh -o BatchMode=yes -o ConnectTimeout=10 "$h" \
      "timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/$p'" 2>/dev/null
  fi
}

loop_pid() { run_loop "pgrep -f 'videocrawl crawl[-]loop' | head -1"; }

loop_alive() {
  local pid
  pid="$(loop_pid)"
  [ -z "$pid" ] && return 1
  # the pid lives on $LOOP_HOST, so probe it there
  run_loop "kill -0 $pid 2>/dev/null"
}

ensure_tunnel() {
  if port_open "$TUNNEL_HOST" "$TUNNEL_PORT"; then return 0; fi
  log "reverse tunnel $TUNNEL_PORT:$TUNNEL_LOCAL -> $TUNNEL_HOST is down; (re)establishing"
  pkill -f "ssh .*-[R] ${TUNNEL_PORT}:${TUNNEL_LOCAL}" 2>/dev/null || true
  sleep 1
  ssh -o BatchMode=yes -o ConnectTimeout=10 -o ExitOnForwardFailure=yes \
      -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
      -fN -R "${TUNNEL_PORT}:${TUNNEL_LOCAL}" "$TUNNEL_HOST"
  sleep 3
  if port_open "$TUNNEL_HOST" "$TUNNEL_PORT"; then
    log "tunnel up"
  else
    log "WARNING: tunnel not reachable on ${TUNNEL_HOST}:${TUNNEL_PORT} (host down, ssh auth, or the proxy is not listening on $TUNNEL_LOCAL)"
  fi
}

ensure_loop() {
  if loop_alive; then return 0; fi
  log "crawl-loop not running; starting on $LOOP_HOST"
  # nohup + full fd redirect so ssh returns. Liveness is tracked by the
  # command line (see header), so no pidfile is needed.
  run_loop "cd '$LOOP_DIR' && nohup env $LOOP_ENV ./videocrawl crawl-loop $LOOP_ARGS >> '$LOOP_LOG' 2>&1 < /dev/null &"
  sleep 2
  if loop_alive; then
    log "crawl-loop started (pid $(loop_pid)); log: $LOOP_LOG"
  else
    log "WARNING: crawl-loop failed to start (is $LOOP_DIR/videocrawl built? is $LOOP_HOST reachable?)"
  fi
}

stop_loop() {
  local pid
  pid="$(loop_pid)"
  if [ -n "$pid" ]; then
    log "stopping crawl-loop (pid $pid): SIGTERM, waiting up to 60s for current video + DB flush"
    run_loop "pkill -TERM -f 'videocrawl crawl[-]loop' 2>/dev/null"
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
      loop_alive || break
      sleep 5
    done
    run_loop "pkill -KILL -f 'videocrawl crawl[-]loop' 2>/dev/null"
  else
    log "crawl-loop not running"
  fi
}

cmd_start() {
  if [ -f "$WATCH_PID" ] && kill -0 "$(cat "$WATCH_PID")" 2>/dev/null; then
    echo "watchdog already running (pid $(cat "$WATCH_PID")); use 'start-loop.sh stop' first" >&2
    exit 1
  fi
  echo "$$" > "$WATCH_PID"
  log "=== watchdog started (pid $$) ==="
  while true; do
    is_local || ensure_tunnel
    ensure_loop
    sleep "$WATCH_INTERVAL"
  done
}

cmd_stop() {
  # Stop the watchdog FIRST — otherwise it resurrects the loop while we
  # are shutting it down (it polls every WATCH_INTERVAL seconds).
  if [ -f "$WATCH_PID" ] && kill -0 "$(cat "$WATCH_PID")" 2>/dev/null; then
    kill "$(cat "$WATCH_PID")" 2>/dev/null
    log "watchdog stopped (pid $(cat "$WATCH_PID"))"
  fi
  rm -f "$WATCH_PID"
  stop_loop
  if ! is_local; then
    if pkill -f "ssh .*-[R] ${TUNNEL_PORT}:${TUNNEL_LOCAL}" 2>/dev/null; then
      log "reverse tunnel stopped"
    fi
  fi
  log "stopped"
}

cmd_status() {
  if is_local; then echo "loop host: local (single-box, no tunnel)"; else echo "loop host: $LOOP_HOST"; fi
  if loop_alive; then echo "crawl-loop: RUNNING (pid $(loop_pid))"; else echo "crawl-loop: DOWN"; fi
  if is_local; then
    echo "tunnel: n/a"
  elif port_open "$TUNNEL_HOST" "$TUNNEL_PORT"; then
    echo "tunnel: UP ($TUNNEL_PORT:$TUNNEL_LOCAL -> $TUNNEL_HOST)"
  else
    echo "tunnel: DOWN"
  fi
}

case "$CMD" in
  start) cmd_start ;;
  stop)  cmd_stop ;;
  status) cmd_status ;;
  *) echo "usage: $0 [start|stop|status]" >&2; exit 2 ;;
esac
