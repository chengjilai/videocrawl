# videocrawl

A polite, time-unbiased video crawler. Enumerates **full source histories**
(not just recent uploads), keeps the frontier in SQLite, downloads
oldest-first with per-site yt-dlp recipes, and resumes crash-safe.

## Sources

| kind | enumeration | route |
|---|---|---|
| `youtube-channel` / `youtube-playlist` | yt-dlp `--flat-playlist -j` (UU playlist: ~1 req / 100 videos, full history) | smart-proxy (WARP) |
| `bilibili-space` | yt-dlp (wbi-signed, needs `bilibili.txt` cookies) | direct |
| `peertube-channel` / `peertube-search` | native REST, `count=100` pagination to the end | direct |
| `ccc-conf` / `ccc-search` | native REST (media.ccc.de events) | warp-doh egress |
| `archive-query` | native advancedsearch pagination | smart-proxy |
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
videocrawl add peertube-channel https://tilvids.com/video-channels/fosstodon
videocrawl add ccc-conf 37c3
videocrawl add archive-query --query 'mediatype:movies AND collection:opensource_movies'
videocrawl add rss https://shipit.show/feed

videocrawl enumerate --concurrency 3     # discovery (cheap, polite)
videocrawl download  --workers 4 --min-dur 60 --max-dur 7200
videocrawl crawl-loop --every 3600 --limit 20 --workers 6
videocrawl status
videocrawl list --status done --json      # feed the bilibili upload pipeline
```

env: `VIDEOCRAWL_DB` (~/videocrawl.db), `VIDEOCRAWL_OUT` (~/Videos/Crawl),
`VIDEOCRAWL_PROXY` (auto sites; default http://127.0.0.1:8888),
`VIDEOCRAWL_COOKIES_DIR` (per-site `<site>.txt` Netscape cookies, e.g.
bilibili.txt from ~/nixos/scripts/export-cookies.py),
`VIDEOCRAWL_SITES_JSON` (JSON overrides), `VIDEOCRAWL_YTDLP`.

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

## Download recipe (yt-dlp sites)

720p H.264 mp4 archive-grade: `-f bv*[height<=720]+ba/b`,
`-S vcodec:h264,res:720,fps,hdr:12,acodec:m4a`, `--merge-output-format mp4`,
`--remux-video mp4`. No `--check-formats` (probes 403 on YouTube). Output
template `%(channel)s/%(id)s_%(title).100B.%(ext)s` (id kills collisions),
`.part` temp on the same filesystem, `-w --continue` resume, en subs.
`--concurrent-fragments 8`. Default client (tv client DRMs formats without
cookies — the VLC path needs it, the crawler doesn't).

## Lab deployment

```sh
scp videocrawl lab:~/videocrawl/
ssh lab 'python3 -m pip install --user -q yt-dlp'
# reverse tunnel (from aturing; lab reaches the smart-proxy at :18888):
ssh -fN -R 18888:127.0.0.1:8888 lab
ssh lab 'VIDEOCRAWL_PROXY=http://127.0.0.1:18888 VIDEOCRAWL_OUT=~/Videos/Crawl \
  nohup ~/videocrawl/videocrawl crawl-loop --every 3600 --limit 20 --workers 8 > ~/videocrawl-loop.log 2>&1 &'
```
