#!/usr/bin/env bash
# sync-upload.sh — one lab→aturing sync + bilibili upload round.
#
# Round trip (run on aturing, where the bilibili web session lives):
#   1. PULL lab's crawl output + DB into ~/videocrawl-lab/:
#        rsync -a --ignore-existing lab:~/Videos/Crawl/  ~/videocrawl-lab/files/
#        rsync -a lab:~/videocrawl/videocrawl.db*        ~/videocrawl-lab/
#      (--ignore-existing: never overwrite files we already have; the db*
#      glob picks up the -wal/-shm sidecars and must land in the DIRECTORY —
#      with sidecars present the glob expands to >1 file, and rsync refuses
#      a plain-file destination for multiple sources.)
#   2. UPLOAD at most one done video to bilibili:
#        videocrawl upload --upload-allowlist cc --limit 1 \
#          --path-prefix-rewrite /home/sjtu/Videos/Crawl:$FILES
#      (the DB + out dir are passed via VIDEOCRAWL_DB / VIDEOCRAWL_OUT —
#      the upload subcommand has no --db/--out flags; it reads status=done,
#      uploads via ~/src/bilibili/upload_web.py with the web session at
#      ~/.config/bili-web-session.json, and marks the row 'uploaded' only
#      on a parsed "SUBMIT OK" bvid.)
#   3. PUSH the 'uploaded' marks back to lab:
#        sqlite3 "$DB" 'PRAGMA wal_checkpoint(TRUNCATE);'   (if sqlite3 exists)
#        rsync -a "$DB"* lab:~/videocrawl/
#
# SQLite WAL notes (why the checkpoint step exists):
#   * the db runs in WAL mode (PRAGMA journal_mode=WAL); live writes land
#     in videocrawl.db-wal, so rsync-ing only the main db would silently
#     drop the newest rows, and copying db+wal together is non-atomic.
#   * PULL is therefore racy while the lab crawl-loop is writing: a torn
#     copy opens as "database disk image is malformed" (observed in
#     testing 2026-08-14, see ~/upload-loop.log — and a torn copy once got
#     pushed back to lab). The integrity gate below aborts the round
#     BEFORE any upload or push; the loop retries next round.
#   * PUSH: `wal_checkpoint(TRUNCATE)` merges the local WAL into the main
#     db file, and the now-stale -wal/-shm sidecars are deleted, so the
#     pushed copy is self-contained. If sqlite3 is missing the sidecars
#     are pushed as-is (racy; documented limitation — install sqlite3).
#
# Config via env (defaults):
#   UPLOAD_LIMIT  1       max videos per upload pass (--limit)
#   DRY_RUN       0|1     1 = pass --dry-run to the upload command
#   VIDEOCRAWL_BIN $HOME/videocrawl/videocrawl
# All output goes to stdout with timestamps; start-upload-loop.sh appends
# it to ~/upload-loop.log.
set -euo pipefail

LAB=lab
DB="$HOME/videocrawl-lab/videocrawl.db"      # local db (pull destination)
DB_DIR="$(dirname "$DB")"
FILES="$HOME/videocrawl-lab/files"
REMOTE_DB="~/videocrawl/videocrawl.db"       # on lab (glob: + -wal/-shm)
REMOTE_FILES="~/Videos/Crawl"
REWRITE="/home/sjtu/Videos/Crawl:$FILES"
UPLOAD_LIMIT="${UPLOAD_LIMIT:-1}"
DRY_RUN="${DRY_RUN:-0}"
VIDEOCRAWL_BIN="${VIDEOCRAWL_BIN:-$HOME/videocrawl/videocrawl}"

log() { echo "[$(date '+%F %T')] $*"; }

[ -x "$VIDEOCRAWL_BIN" ] || {
  echo "error: $VIDEOCRAWL_BIN not executable — build it:" >&2
  echo "  cd $HOME/videocrawl && go build -mod=vendor -o videocrawl ./cmd/videocrawl" >&2
  exit 1
}
command -v sqlite3 >/dev/null 2>&1 \
  || echo "warning: sqlite3 not found; WAL sidecars will be pushed as-is (documented limitation)" >&2

mkdir -p "$FILES" "$DB_DIR"

# 1. pull files (never overwrite what we already have)
log "pull: $LAB:$REMOTE_FILES/ -> $FILES/"
rsync -a --ignore-existing "$LAB:$REMOTE_FILES/" "$FILES/"

# 2. pull DB (+ -wal/-shm sidecars; the glob expands on the remote shell)
log "pull: $LAB:$REMOTE_DB* -> $DB_DIR/"
rsync -a "$LAB:$REMOTE_DB"* "$DB_DIR/"

# 2b. integrity gate — abort BEFORE any upload/push if the copy is torn
if ! sqlite3 "$DB" 'PRAGMA integrity_check;' 2>/dev/null | grep -q '^ok'; then
  log "ERROR: pulled DB failed integrity_check; aborting round (no upload, no push)"
  rm -f "$DB"-wal "$DB"-shm
  exit 1
fi

# 3. upload pass (one video; paced by start-upload-loop.sh)
# Licensing gate: 'cc' = media.ccc.de (CC BY by documentation); numeric ids =
# vouched sources. 5 = tilvids (FOSS PeerTube instance, CC by platform
# convention; the API records no per-video license). YouTube stays excluded.
# Override: ALLOWLIST=cc,5,6 ./sync-upload.sh
ALLOWLIST="${ALLOWLIST:-cc,5}"
args=(--upload-allowlist "$ALLOWLIST" --limit "$UPLOAD_LIMIT" --path-prefix-rewrite "$REWRITE")
if [ "$DRY_RUN" = 1 ]; then
  args+=(--dry-run)
fi
log "upload: VIDEOCRAWL_DB=$DB VIDEOCRAWL_OUT=$FILES"
upload_rc=0
VIDEOCRAWL_DB="$DB" VIDEOCRAWL_OUT="$FILES" "$VIDEOCRAWL_BIN" upload "${args[@]}" \
  || upload_rc=$?

# 4. WAL checkpoint, then push the DB back (uploaded marks propagate to lab)
if command -v sqlite3 >/dev/null 2>&1; then
  if sqlite3 "$DB" 'PRAGMA wal_checkpoint(TRUNCATE);' >/dev/null 2>&1; then
    rm -f "$DB"-wal "$DB"-shm   # stale sidecars after TRUNCATE
  else
    log "warning: wal_checkpoint failed; pushing -wal/-shm as-is"
  fi
else
  log "note: sqlite3 not found; pushing db + -wal/-shm as-is (non-atomic; documented limitation)"
fi
log "push: $DB_DIR/ -> $LAB:~/videocrawl/"
rsync -a "$DB"* "$LAB:~/videocrawl/"

exit "$upload_rc"
