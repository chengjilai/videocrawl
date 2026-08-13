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
//	status
//	list [--status X] [--json] [--limit N]
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
	"videocrawl/internal/model"
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
		fs.Parse(reorderArgs(args, map[string]bool{}))
		if fs.NArg() < 2 {
			err = usageErr("add <kind> <url-or-id> [--name N] [--query Q]")
			break
		}
		err = a.Add(fs.Arg(0), fs.Arg(1), *name, *query)
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
		minDur := fs.Int64("min-dur", 60, "min video seconds")
		maxDur := fs.Int64("max-dur", 7200, "max video seconds")
		fs.Parse(reorderArgs(args, map[string]bool{}))
		policy := dl.Policy{MinDuration: *minDur, MaxDuration: *maxDur, SkipShorts: true, SkipLive: true}
		err = a.CrawlLoop(ctx, *every, *limit, *workers, *rounds, time.Duration(*maxTime)*time.Second, policy)
	case "status":
		err = a.Status()
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		status := fs.String("status", "", "filter: new|done|failed|skipped")
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

  add <kind> <url-or-id> [--name N] [--query Q]
      kinds: youtube-channel youtube-playlist bilibili-space bilibili-fav
             peertube-channel peertube-search ccc-conf ccc-search
             archive-query rss
  rm <id>                     remove a source (and its queued videos)
  enumerate [--concurrency N] [--limit N] [--source ID]
  download  [--limit N] [--workers W] [--min-dur S] [--max-dur S]
            [--skip-shorts] [--skip-live]
  crawl-loop [--every SEC] [--limit N] [--workers W] [--rounds N] [--max-time SEC]
  status                      sources + queue counts
  list [--status X] [--json] [--limit N]

env: VIDEOCRAWL_DB (~/videocrawl.db) VIDEOCRAWL_OUT (~/Videos/Crawl)
     VIDEOCRAWL_PROXY (auto sites; default http://127.0.0.1:8888)
     VIDEOCRAWL_COOKIES_DIR (per-site <site>.txt Netscape cookie files)
     VIDEOCRAWL_YTDLP (yt-dlp binary) VIDEOCRAWL_SITES_JSON (overrides)

examples:
  videocrawl add youtube-channel https://www.youtube.com/@GNOME/videos
  videocrawl add bilibili-space 306049207
  videocrawl add bilibili-fav 1103260112          # or a favlist URL
  videocrawl add peertube-channel https://tilvids.com/video-channels/fosstodon
  videocrawl add ccc-conf 37c3
  videocrawl add archive-query --query 'mediatype:movies AND licenseurl:[* TO *]'
  videocrawl add rss https://shipit.show/feed
  videocrawl crawl-loop --every 3600 --limit 10 --workers 6 --max-time 3600`)
}

func usageErr(msg string) error { return fmt.Errorf("usage: %s", msg) }

// reorderArgs moves --flag value pairs before positionals so Go's flag
// package (which stops at the first non-flag arg) parses both orders:
// "add kind url --name X" and "add --name X kind url".
func reorderArgs(args []string, boolFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
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

var _ = model.StatusNew
var _ = strings.TrimSpace
