#!/usr/bin/env bash
# run-loop.sh — starts the videocrawl crawl-loop on lab (this machine;
# the tracked copy in the aturing repo is documentation only). Runs on
# lab ONLY: the bilibili web session lives on aturing, so the upload
# scripts (music-upload-loop.sh, start-upload-loop.sh, sync-upload.sh)
# are aturing-only by design and do not exist here.
#
# Env:
#   VIDEOCRAWL_WARP_SOCKS=off          no WARP socks on lab — skip the probe
#   VIDEOCRAWL_NO_PROXY_CHECK=1        skip the smart-proxy health probe
#   VIDEOCRAWL_PROXY=http://127.0.0.1:18888
#                                      aturing's smart-proxy via the manual
#                                      reverse tunnel kept up by the
#                                      lab-proxy-tunnel systemd unit on aturing
#   VIDEOCRAWL_OUT / VIDEOCRAWL_DB / VIDEOCRAWL_COOKIES_DIR   (see README)
#
# `exec` replaces the shell, so pgrep sees './videocrawl crawl-loop', not
# 'run-loop.sh'.
#
# NO watchdog: if the crawl-loop dies nothing restarts it (the old
# watchdog script was deleted 2026-08-15). Restart manually, e.g.:
#   nohup ~/videocrawl/run-loop.sh >> ~/crawl-loop.log 2>&1 &
#
# --limit 8 --workers 6 --max-time 3000 is the live tune (2026-08-15).
cd "$HOME/videocrawl" || exit 1
export VIDEOCRAWL_WARP_SOCKS=off VIDEOCRAWL_NO_PROXY_CHECK=1
export VIDEOCRAWL_PROXY=http://127.0.0.1:18888
# transcript gate: 0.15 → 0.2 (2026-08-15: the 0.15 floor let 2012-era
# generic talks through; 0.2 keeps the precision layer meaningful)
export VIDEOCRAWL_TRANSCRIPT_THRESHOLD=0.2
# VIDEOCRAWL_EMBED_URL: the lab embedding server (Qwen3-Embedding-0.6B);
# the crawler falls back to TF-IDF scoring if it is down.
export VIDEOCRAWL_EMBED_URL=http://127.0.0.1:8700/embed
export VIDEOCRAWL_OUT="$HOME/Videos/Crawl"
export VIDEOCRAWL_DB="$HOME/videocrawl/videocrawl.db"
export VIDEOCRAWL_COOKIES_DIR="$HOME/videocrawl/cookies"
# --auto-seed 5 --auto-seed-every 24: one corpus-driven discovery pass per
# day (24 hourly rounds), top-5 hits seeded into the 'discover' source —
# scored rows jump the download queue and upload via the 'disc' allowlist.
exec ./videocrawl crawl-loop --every 3600 --limit 8 --workers 6 --max-time 3000 \
  --auto-seed 5 --auto-seed-every 24
