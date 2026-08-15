// videocrawl: a polite, time-unbiased video crawler. Enumerates full source
// histories (YouTube channels/playlists + bilibili spaces via yt-dlp flat
// playlist; PeerTube / media.ccc.de / archive.org / RSS via REST), stores the
// frontier in SQLite, downloads oldest-first with per-site yt-dlp recipes.
//
// Commands:
//
//	add <kind> <url|id> [--name N] [--query Q]
//	rm <id>
//	enumerate [--concurrency N] [--limit N] [--source ID]
//	download [--limit N] [--workers W] [--min-dur S] [--max-dur S]
//	         [--skip-shorts] [--skip-live]
//	crawl-loop [--every SEC] [--limit N] [--workers W] [--rounds N]
//	upload [--limit N] [--dry-run] --upload-allowlist 'cc'|IDs
//	       [--path-prefix-rewrite OLD:NEW]
//	status
//	list [--status X] [--json] [--limit N]
//
//	post-loop [--every SEC] [--limit N] [--dry-run] [--check-bili]
//	          [--no-upload|--upload-only] [--seed ID[:T],...] [--proxy URL] [--relay cors.sh|eu.org]
//	post-seed ID[:Title] ...      queue music candidates (PD-gated repost)
//	post-status                   show the music post queue
//	search <query> [--seed]       archive.org PD music search (find logic)
//	discover [--limit N] [--query Q]... [--sources yt,hn,ccc]
//	         [--threshold F] [--topics kw1,-kw2] [--hn-min-points N]
//	         [--per-query N] [--transcripts N] [--include-known] [--seed N] [--json]
//	                             corpus-driven discovery of new talks; --seed N
//	                             queues the top-N hits into the discover source
//	                             (crawl-loop downloads in relevance order;
//	                             'upload --upload-allowlist disc' republishes)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"videocrawl/internal/app"
	"videocrawl/internal/dl"
)

func main() {
	// SIGINT/SIGTERM → graceful shutdown: the crawl-loop finishes the
	// current video, flushes the DB (WAL checkpoint on Close) and exits 0.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := app.Default()
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "add":
		fs := flag.NewFlagSet("add", flag.ExitOnError)
		name := fs.String("name", "", "source name")
		query := fs.String("query", "", "extra query (search kinds)")
		topics := fs.String("topics", "", "comma-separated title/channel topic filter (only matching entries are queued)")
		fs.Parse(reorderArgs(args, map[string]bool{}))
		if fs.NArg() < 2 {
			err = usageErr("add <kind> <url-or-id> [--name N] [--query Q] [--topics kw1,kw2]")
			break
		}
		err = a.Add(fs.Arg(0), fs.Arg(1), *name, *query, *topics)
	case "set-topics":
		fs := flag.NewFlagSet("set-topics", flag.ExitOnError)
		fs.Parse(reorderArgs(args, map[string]bool{}))
		if fs.NArg() < 2 {
			err = usageErr("set-topics <source-id> <kw1,kw2,...>")
			break
		}
		var sid int64
		fmt.Sscan(fs.Arg(0), &sid)
		err = a.SetTopics(sid, fs.Arg(1))
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ExitOnError)
		fs.Parse(reorderArgs(args, map[string]bool{}))
		if fs.NArg() < 1 {
			err = usageErr("rm <source-id>")
			break
		}
		var id int64
		fmt.Sscan(fs.Arg(0), &id)
		err = a.Rm(id)
	case "enumerate":
		fs := flag.NewFlagSet("enumerate", flag.ExitOnError)
		concurrency := fs.Int("concurrency", 2, "parallel sources")
		limit := fs.Int("limit", 0, "max entries per source (0=all)")
		source := fs.Int64("source", 0, "only this source id")
		fs.Parse(reorderArgs(args, map[string]bool{}))
		err = a.Enumerate(ctx, *concurrency, *limit, *source, time.Time{})
	case "download":
		fs := flag.NewFlagSet("download", flag.ExitOnError)
		limit := fs.Int("limit", 0, "max videos this pass (0=all queued)")
		workers := fs.Int("workers", 6, "parallel downloads (lab: egress-bound, default 6)")
		minDur := fs.Int64("min-dur", 60, "skip videos shorter than N seconds")
		maxDur := fs.Int64("max-dur", 7200, "skip videos longer than N seconds")
		skipShorts := fs.Bool("skip-shorts", true, "skip YouTube Shorts")
		skipLive := fs.Bool("skip-live", true, "skip live streams")
		fs.Parse(reorderArgs(args, map[string]bool{"skip-shorts": true, "skip-live": true}))
		policy := dl.Policy{MinDuration: *minDur, MaxDuration: *maxDur, SkipShorts: *skipShorts, SkipLive: *skipLive}
		err = a.Download(ctx, *limit, *workers, policy, time.Time{})
	case "crawl-loop":
		fs := flag.NewFlagSet("crawl-loop", flag.ExitOnError)
		every := fs.Int("every", 900, "seconds between rounds")
		limit := fs.Int("limit", 20, "max videos per round")
		workers := fs.Int("workers", 6, "parallel downloads (lab: egress-bound, default 6)")
		rounds := fs.Int("rounds", 0, "0 = run forever")
		maxTime := fs.Int("max-time", 0, "per-round time budget in seconds (0=unlimited)")
		autoSeed := fs.Int("auto-seed", 0, "seed the top-N corpus-discovery hits into the discover source each --auto-seed-every rounds (0 = off)")
		autoSeedEvery := fs.Int("auto-seed-every", 24, "rounds between auto-seed passes (loop cadence * rounds ≈ daily at --every 3600)")
		minDur := fs.Int64("min-dur", 60, "min video seconds")
		maxDur := fs.Int64("max-dur", 7200, "max video seconds")
		fs.Parse(reorderArgs(args, map[string]bool{}))
		policy := dl.Policy{MinDuration: *minDur, MaxDuration: *maxDur, SkipShorts: true, SkipLive: true}
		err = a.CrawlLoop(ctx, *every, *limit, *workers, *rounds, time.Duration(*maxTime)*time.Second, policy, *autoSeed, *autoSeedEvery)
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ExitOnError)
		limit := fs.Int("limit", 0, "max videos this pass (0=all done)")
		dryRun := fs.Bool("dry-run", false, "print the would-upload list only")
		allowlist := fs.String("upload-allowlist", "", "'cc' (CC BY, site=ccc), 'pt' (peertube), 'disc' (discover) or comma list of source ids; REQUIRED (licensing gate)")
		rewrite := fs.String("path-prefix-rewrite", "", "OLD:NEW — rewrite videos.path prefix (lab->aturing sync)")
		fs.Parse(reorderArgs(args, map[string]bool{"dry-run": true}))
		err = a.Upload(*limit, *dryRun, *allowlist, *rewrite, a.OutDir)
	case "post-loop":
		err = app.PostLoop(reorderArgs(args, map[string]bool{"dry-run": true, "check-bili": true, "no-upload": true, "upload-only": true, "q": true}))
	case "post-seed":
		err = app.PostSeed(args)
	case "post-status":
		err = app.PostStatus(reorderArgs(args, map[string]bool{}))
	case "search":
		err = app.MusicSearch(reorderArgs(args, map[string]bool{"seed": true}))
	case "discover":
		err = app.Discover(ctx, reorderArgs(args, map[string]bool{"include-known": true, "json": true}))
	case "status":
		err = a.Status()
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		status := fs.String("status", "", "filter: new|done|failed|skipped|uploaded")
		jsonOut := fs.Bool("json", false, "JSONL output")
		limit := fs.Int("limit", 50, "rows")
		fs.Parse(reorderArgs(args, map[string]bool{"json": true}))
		err = a.List(*status, *jsonOut, *limit)
	default:
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`videocrawl — polite time-unbiased video crawler

  add <kind> <url-or-id> [--name N] [--query Q] [--topics kw1,kw2]
      --topics: only queue entries whose title/channel matches a keyword
      (case-insensitive, OR) — the techcrawl-style topical gate for broad
      seeds (e.g. a whole PeerTube instance)
      kinds: youtube-channel youtube-playlist bilibili-space bilibili-fav
             peertube-channel peertube-search ccc-conf ccc-search
             archive-query archive-audio rss gallica
  rm <id>                     remove a source (and its queued videos)
  set-topics <id> kw1,kw2      set a source's topic filter ('' clears)
  enumerate [--concurrency N] [--limit N] [--source ID]
  download  [--limit N] [--workers W] [--min-dur S] [--max-dur S]
            [--skip-shorts] [--skip-live]
  crawl-loop [--every SEC] [--limit N] [--workers W] [--rounds N] [--max-time SEC]
  upload [--limit N] [--dry-run] --upload-allowlist 'cc'|'pt'|'disc'|IDs [--path-prefix-rewrite OLD:NEW]
      republish done videos to bilibili; --upload-allowlist is mandatory:
      'cc' = media.ccc.de (CC BY) sources, 'pt' = peertube, 'disc' = discover
      sources, or a comma list of source ids
  post-loop [--every SEC] [--limit N] [--dry-run] [--check-bili] [--no-upload|--upload-only]
      [--seed ID[:T],...] [--video-dir DIR]
      PD-gated auto download+repost loop for music (archive.org → bilibili;
      law gate: recording year >50y; dedup: ~/.videocrawl/repost-state.jsonl;
      egress via the archive site config — smart-proxy, same as the crawler)
  post-seed ID[:Title] ...     queue music candidates
  post-status                  show the music post queue
  search <query> [--seed]      archive.org PD music search (the music find)
  discover [--limit N] [--query Q]... [--sources yt,hn,ccc]
      [--threshold F] [--topics kw1,-kw2] [--hn-min-points N]
      [--per-query N] [--transcripts N] [--include-known] [--seed N] [--json]
      corpus-driven discovery of new talks: queries are
      generated from the uploaded-talk corpus (config/semantic-corpus.json)
      and run through youtube search / HN / media.ccc.de; results are gated
      (dedup, topics, semantic score, shorts/live, speaker boost) and ranked.
      --seed N queues the top-N hits into the discover source (crawl-loop
      downloads them in relevance order; 'upload --upload-allowlist disc'
      republishes). Without --seed, add a hit with
      'videocrawl add youtube-playlist <url>' (works for a single talk)
  status                      sources + queue counts
  list [--status X] [--json] [--limit N]

env: VIDEOCRAWL_DB (~/videocrawl.db) VIDEOCRAWL_OUT (~/Videos/Crawl)
     VIDEOCRAWL_PROXY (auto sites; default http://127.0.0.1:8888)
     VIDEOCRAWL_COOKIES_DIR (per-site <site>.txt Netscape cookie files)
     VIDEOCRAWL_YTDLP (yt-dlp binary) VIDEOCRAWL_SITES_JSON (overrides)
     VIDEOCRAWL_UPLOAD_SCRIPT (~/src/bilibili/upload_web.py)

examples:
  videocrawl add youtube-channel https://www.youtube.com/@GNOME/videos
  videocrawl add bilibili-space 306049207
  videocrawl add bilibili-fav 1103260112          # or a favlist URL
  videocrawl add peertube-channel https://tilvids.com/video-channels/fosstodon
  videocrawl add ccc-conf 37c3
  videocrawl add archive-query --query 'mediatype:movies AND licenseurl:[* TO *]'
  videocrawl add archive-audio https://archive.org/advancedsearch.php --query 'collection:great78'  # mediatype:audio auto-appended
  videocrawl add archive-audio https://archive.org/details/SomeItem
  videocrawl add gallica https://gallica.bnf.fr/ark:/12148/btv1b52503827w
  videocrawl add rss https://shipit.show/feed
  videocrawl crawl-loop --every 3600 --limit 10 --workers 6 --max-time 3600
  videocrawl upload --upload-allowlist cc,3,7 --path-prefix-rewrite /home/sjtu/Videos/Crawl:/home/chengjilai/Videos/Crawl
  videocrawl search 'collection:78rpm AND mediatype:audio' --seed
  videocrawl discover --limit 10 --sources yt,ccc
  videocrawl discover --query 'taming the future shepherd' --sources hn
  videocrawl post-loop --limit 1 --check-bili`)
}

func usageErr(msg string) error { return fmt.Errorf("usage: %s", msg) }

// reorderArgs moves --flag value pairs before positionals so Go's flag
// package (which stops at the first non-flag arg) parses both orders:
// "add kind url --name X" and "add --name X kind url".
func reorderArgs(args []string, boolFlags map[string]bool) []string {
	var flags, pos []string
	afterDash := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// everything after '--' is positional — including args that
			// start with '-' (e.g. set-topics 22 -- -interview,-panel)
			afterDash = true
			flags = append(flags, a)
			continue
		}
		if !afterDash && strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !boolFlags[strings.TrimLeft(a, "-")] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}
