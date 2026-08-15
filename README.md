# videocrawl

A polite, time-unbiased video crawler. Enumerates **full source histories**
(not just recent uploads), keeps the frontier in SQLite, downloads
oldest-first with per-site yt-dlp recipes, and resumes crash-safe.

## Sources

| kind | enumeration | route |
|---|---|---|
| `youtube-channel` / `youtube-playlist` | yt-dlp `--flat-playlist -j` (UU playlist: ~1 req / 100 videos, full history) | smart-proxy (WARP) |
| `bilibili-space` | yt-dlp (wbi-signed, needs `bilibili.txt` cookies) | direct |
| `bilibili-fav` | native REST `x/v3/fav/resource/list`, wbi-signed, buvid3 + optional session cookies | direct |
| `peertube-channel` / `peertube-search` | native REST, `count=100` pagination to the end | direct |
| `ccc-conf` / `ccc-search` | native REST (media.ccc.de events) | warp-doh egress |
| `archive-query` | native scraping API (`services/search/v1/scrape`, cursor-based, 1000/req), advancedsearch fallback | smart-proxy |
| `archive-audio` | same enumerator as `archive-query`, but normalize pins `mediatype:audio`; downloads are native: item metadata → best format tier (mp3 > flac > ogg), Range-resume fetch of every track (extras in `media_files`) | smart-proxy |
| `gallica` | one BnF ark per source (single entry; title via SRU). Downloads solve the altcha PoW once per session (cookie), then fetch the PDF | smart-proxy |
| `rss` | RSS/Atom video enclosures | direct |

No time bias: channel tabs are newest-first feeds, but the crawler walks every
continuation page to the end (yt-dlp loop detection) and processes the queue
**oldest published first** (TubeSync lesson: a slow queue must not starve old
videos).

## Build

```sh
go build -mod=vendor -o videocrawl ./cmd/videocrawl   # static, no CGO
```

## Run

```sh
videocrawl add youtube-channel https://www.youtube.com/@GNOME/videos
videocrawl add bilibili-space 306049207
videocrawl add bilibili-fav 1103260112        # numeric fid, or a favlist URL
videocrawl add peertube-channel https://tilvids.com/video-channels/fosstodon
videocrawl add ccc-conf 37c3
videocrawl add archive-query --query 'mediatype:movies AND collection:opensource_movies'
videocrawl add archive-audio https://archive.org/advancedsearch.php --query 'collection:great78'  # mediatype:audio auto-appended
videocrawl add archive-audio https://archive.org/details/SomeAlbum
videocrawl add gallica https://gallica.bnf.fr/ark:/12148/btv1b52503827w
videocrawl add rss https://shipit.show/feed

videocrawl enumerate --concurrency 3     # discovery (cheap, polite)
videocrawl download  --workers 6 --min-dur 60 --max-dur 7200
videocrawl crawl-loop --every 3600 --limit 20 --workers 6 --max-time 3600
videocrawl status
videocrawl list --status done --json      # feed the bilibili upload pipeline

# channel-unbiased find→upload: corpus-driven search (ytsearch + HN Algolia
# + ccc events) ranks talks by semantic relevance; --seed queues the top-N
# into the static 'discover' source. Scored rows flow to the download and
# upload queues BEFORE unscored ones (relevance order), and the transcript
# gate at download time is the precision backstop.
videocrawl discover --limit 10 --seed 10     # --sources yt,hn,ccc; --threshold 0.16
videocrawl upload --upload-allowlist cc,pt,disc,22,23,24   # 'disc' = discover source
```

`download` / `crawl-loop` default to **6 workers** (lab-tuned; was 3). On
lab (28 cores, 1.2TB) downloads are egress-bound, not CPU-bound: each
worker is one file, the WARP tunnel is the bottleneck, and more files (and
per-file stripes on ccc) saturate it. Tune with `--workers` — the tunnel
sweet spot observed is ~6–12; `--limit` caps videos per pass/round.

env: `VIDEOCRAWL_DB` (~/videocrawl.db), `VIDEOCRAWL_OUT` (~/Videos/Crawl),
`VIDEOCRAWL_PROXY` (auto sites; default http://127.0.0.1:8888),
`VIDEOCRAWL_COOKIES_DIR` (per-site `<site>.txt` Netscape cookies, e.g.
bilibili.txt from ~/nixos/scripts/export-cookies.py),
`VIDEOCRAWL_SITES_JSON` (JSON overrides), `VIDEOCRAWL_YTDLP`,
`VIDEOCRAWL_MIN_FREE_GB` (disk-headroom gate, default 2),
`VIDEOCRAWL_WARP_SOCKS` (WARP socks addr probed by the health check,
default 127.0.0.1:40000; `off` disables the probe),
`VIDEOCRAWL_NO_PROXY_CHECK` (set to 1 to skip the smart-proxy health probe),
`VIDEOCRAWL_STRIPES` (native ccc stripes per file, default 4, cap 6),
`VIDEOCRAWL_RATE_CEIL_MB` (per-file ccc rate ceiling, default 4 MiB/s).

## Audio mode (yt-dlp sites)

Set `audioFormat` (one of `mp3` | `flac` | `m4a`) on a yt-dlp site in
`VIDEOCRAWL_SITES_JSON` to turn that site's downloads into audio extraction
(`-x --audio-format <fmt> --audio-quality 0`, best audio stream):

```json
{ "youtube": { "audioFormat": "mp3" } }
```

`archive-audio` and `gallica` download natively instead (no yt-dlp):
`archive-audio` picks the item's best audio tier from
`archive.org/metadata/{id}` and Range-resumes every track into
`<out>/archive-audio/` (the primary track is recorded on the video row, the
rest in the `media_files` table); `gallica` streams the ark's PDF into
`<out>/gallica/`, solving the altcha proof-of-work once per session and
reusing the verified cookie.

## Unattended operation

The crawl-loop is hardened for long-running, no-intervention operation:

- **Disk headroom.** Before every download pass the free space on the
  `VIDEOCRAWL_OUT` filesystem is checked (`statfs`); below
  `VIDEOCRAWL_MIN_FREE_GB` (default 2) the pass is skipped with a clear
  message and retried next round. Enumeration still runs (DB-only).
- **Per-round time budget.** `crawl-loop --max-time SECONDS` bounds each
  round (enumeration + download). The budget is checked *cooperatively*
  between videos/sources — the video already in flight always finishes
  (crash-safe resume; see below), so a round can overrun by at most one
  video. yt-dlp also gets `--socket-timeout 60` so a dead socket can't
  stall a round forever.
- **Egress health check.** Each round starts by probing the WARP socks
  (127.0.0.1:40000) and the configured smart-proxy (`VIDEOCRAWL_PROXY`; on
  lab the reverse-tunnel port 18888). If either is down the round is
  skipped with a clear message (a dead tunnel would otherwise burn the
  whole round on doomed requests).
- **Crash recovery (verified by code reading).** SQLite runs WAL +
  `synchronous=NORMAL` with a single connection; a kill mid-write cannot
  corrupt the DB (the in-flight transaction rolls back, WAL replays on next
  open). `Store.Close` additionally runs `wal_checkpoint(TRUNCATE)` so a
  graceful stop leaves nothing in `-wal`. Downloads are `.part`-resumed:
  yt-dlp writes to `outDir/.tmp/*.part` and only atomically renames to the
  final name on success (`-w --continue --no-overwrites`); a killed
  download leaves the row `new` and the next pass resumes the `.part`.
  Native (ccc) fetches use parallel Range stripes into a sparse-preallocated
  `.part` (`WriteAt`, in-memory per-stripe progress — a crash restarts the
  file) and `os.Rename` atomically; single-stream `Range: bytes=N-` resume
  stays for subtitles and no-range servers. `findOutput` ignores `*.part`,
  so partial files are never hashed or marked done. A kill between file completion and the
  DB `MarkDownloaded` is also safe: the row is still `new`, and yt-dlp
  (`-w`) / the native `os.Stat` pre-check see the finished file and just
  hash + mark it.
- **Signals.** SIGINT/SIGTERM trigger graceful shutdown: the loop stops
  scheduling new work, the current video finishes, the DB is flushed
  (WAL checkpoint on Close) and the process exits 0. `start-loop.sh stop`
  uses this path.

### start-loop.sh (watchdog)

`./start-loop.sh [start|stop|status]` keeps the rig alive unattended on the
lab: it (re)establishes the reverse tunnel
(`ssh -fN -R 18888:127.0.0.1:8888 lab`, with `ExitOnForwardFailure` and
`ServerAlive` so a dead tunnel is detected and replaced) and starts
`videocrawl crawl-loop` with `nohup` on the crawl host, then watches both
and restarts whichever died. Loop liveness is tracked by command line
(`pgrep -f 'videocrawl crawl[-]loop'`), so SIGTERM reliably reaches the Go
process through the nohup/env wrapper chain. Config via env: `LOOP_HOST`
(lab or `local`), `TUNNEL_HOST`, `TUNNEL_PORT`, `TUNNEL_LOCAL`, `LOOP_DIR`,
`LOOP_ENV`, `LOOP_ARGS`, `LOG_DIR`, `WATCH_INTERVAL` — see the header of
the script.

```sh
# on the machine with the smart-proxy (aturing):
./start-loop.sh start      # tunnel + crawl-loop, then watch both forever
./start-loop.sh status
./start-loop.sh stop       # watchdog first, then graceful SIGTERM to the loop
```

## Egress

- **smart-proxy** (`http://127.0.0.1:8888`): youtube/github/archive.org via
  WARP (policy-routed), everything else direct. On lab, reach it through the
  reverse tunnel `ssh -R 18888:127.0.0.1:8888 lab` → `VIDEOCRAWL_PROXY=http://127.0.0.1:18888`.
- **warp-doh** (media.ccc.de): CN resolvers and doh.pub return poisoned
  answers for ccc hosts (an archive.org IP for cdn.media.ccc.de), and WARP
  intercepts 1.1.1.1 specially. Verified working combo: resolve via
  `dns.google` DoH through the WARP socks (8.8.8.8:443), then connect via
  WARP socks to the resolved IP; try all A records, evict-and-re-resolve on
  total dial failure. The cdn 302-redirects to `ffmuc.media.ccc.de`; the
  same transport follows it.

  **Native downloads use parallel range-fetch stripes.** A single WARP
  stream tops out at ~130KB/s (verified), so each file is split into N
  disjoint byte ranges fetched concurrently (aria2-style; each stripe on its
  own WARP connection + socks hop) and assembled by `WriteAt` into a
  sparse-preallocated `.part`. Per-stripe retry resumes from stripe-local
  progress on connection drops (the verified mid-stream EOF). Politeness:
  the ccc mirror is volunteer-run, so stripes are capped at **6** (default
  **4**, `VIDEOCRAWL_STRIPES`) and a token bucket shared by the stripes caps
  each file at **4 MiB/s** (`VIDEOCRAWL_RATE_CEIL_MB`). Measured live on a
  13 MiB file: **503 KB/s striped vs 116 KB/s single-stream** (4.3×).

## Download recipe (yt-dlp sites)

720p H.264 mp4 archive-grade: `-f bv*[height<=720]+ba/b`,
`-S vcodec:h264,res:720,fps,hdr:12,acodec:m4a`, `--merge-output-format mp4`,
`--remux-video mp4`. No `--check-formats` (probes 403 on YouTube). Output
template `%(channel)s/%(id)s_%(title).100B.%(ext)s` (id kills collisions),
`.part` temp on the same filesystem, `-w --continue` resume, en subs.
`--concurrent-fragments 8`. Default client (tv client DRMs formats without
cookies — the VLC path needs it, the crawler doesn't). Throughput check:
`--concurrent-fragments 8` is verified good for fragmented DASH (per-video
parallelism on yt-dlp sites is otherwise capped by the worker count), and
we deliberately set **no `--limit-rate`** — yt-dlp sites are CDN-backed; the
polite per-file ceiling lives on the native ccc path (see Egress).

## Lab deployment

```sh
scp videocrawl start-loop.sh lab:~/videocrawl/
ssh lab 'python3 -m pip install --user -q yt-dlp'
# reverse tunnel (from aturing; lab reaches the smart-proxy at :18888):
ssh -fN -R 18888:127.0.0.1:8888 lab
# or, unattended: the watchdog manages tunnel + loop and restarts on death
./start-loop.sh start
# manual equivalent (defaults: 6 workers, 4 ccc stripes, 4MiB/s/file ceiling):
ssh lab 'VIDEOCRAWL_PROXY=http://127.0.0.1:18888 VIDEOCRAWL_OUT=~/Videos/Crawl \
  nohup ~/videocrawl/videocrawl crawl-loop --every 3600 --limit 20 --workers 6 --max-time 3600 > ~/videocrawl-loop.log 2>&1 &'
```
