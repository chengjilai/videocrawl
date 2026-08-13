// Package app: CLI commands.
package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"videocrawl/internal/dl"
	"videocrawl/internal/enum"
	"videocrawl/internal/model"
	"videocrawl/internal/politeness"
	"videocrawl/internal/sites"
	"videocrawl/internal/store"
)

type App struct {
	DBPath  string
	OutDir  string
	SitesJS string
}

func Default() *App {
	home, _ := os.UserHomeDir()
	db := envOr("VIDEOCRAWL_DB", home+"/videocrawl.db")
	out := envOr("VIDEOCRAWL_OUT", home+"/Videos/Crawl")
	return &App{DBPath: db, OutDir: out, SitesJS: envOr("VIDEOCRAWL_SITES_JSON", "")}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (a *App) open() (*store.Store, error) { return store.Open(a.DBPath) }

func (a *App) sites() map[string]sites.Site { return sites.Load(a.SitesJS) }

// ---- add ----

func (a *App) Add(kind, raw, name, query string) error {
	url, q, err := normalize(kind, raw, query)
	if err != nil {
		return err
	}
	if name == "" {
		name = guessName(kind, raw)
	}
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	id, err := st.AddSource(kind, url, q, name)
	if err != nil {
		return err
	}
	fmt.Printf("added source #%d: %s %s (%s)\n", id, kind, url, name)
	return nil
}

// normalize maps a user-provided seed to a canonical URL + query.
func normalize(kind, raw, query string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	switch kind {
	case model.KindYoutubeChannel:
		if !strings.HasPrefix(raw, "http") {
			if strings.HasPrefix(raw, "UC") && len(raw) == 24 {
				return "https://www.youtube.com/channel/" + raw, "", nil
			}
			if strings.HasPrefix(raw, "@") {
				return "https://www.youtube.com/" + raw, "", nil
			}
		}
		return raw, "", nil
	case model.KindYoutubePlaylist:
		if !strings.HasPrefix(raw, "http") {
			return "https://www.youtube.com/playlist?list=" + raw, "", nil
		}
		return raw, "", nil
	case model.KindBilibiliSpace:
		mid := raw
		if i := strings.LastIndex(raw, "space.bilibili.com/"); i >= 0 {
			rest := raw[i+len("space.bilibili.com/"):]
			mid = strings.SplitN(rest, "/", 2)[0]
		}
		mid = strings.Trim(mid, "/ ")
		if mid == "" || !allDigits(mid) {
			return "", "", fmt.Errorf("bilibili-space needs a numeric mid or space URL, got %q", raw)
		}
		return "https://space.bilibili.com/" + mid + "/video", "", nil
	case model.KindPeertubeChannel:
		if !strings.HasPrefix(raw, "http") {
			return "", "", fmt.Errorf("peertube-channel needs a full URL: https://<instance>/video-channels/<handle>")
		}
		return raw, "", nil
	case model.KindPeertubeSearch:
		if !strings.HasPrefix(raw, "http") {
			return "", "", fmt.Errorf("peertube-search needs an instance URL; pass --query for the search term")
		}
		if query == "" {
			return "", "", fmt.Errorf("peertube-search needs --query")
		}
		return raw, query, nil
	case model.KindCCCConf:
		return "https://media.ccc.de/public/conferences/" + strings.Trim(raw, "/ "), "", nil
	case model.KindCCCSearch:
		if query == "" {
			return "", "", fmt.Errorf("ccc-search needs --query")
		}
		return "https://media.ccc.de/public/events/search", query, nil
	case model.KindArchiveQuery:
		if query == "" {
			return "", "", fmt.Errorf("archive-query needs --query (lucene), e.g. 'mediatype:movies AND collection:tech_talks'")
		}
		return "https://archive.org/advancedsearch.php", query, nil
	case model.KindRSS:
		if !strings.HasPrefix(raw, "http") {
			return "", "", fmt.Errorf("rss needs a feed URL")
		}
		return raw, "", nil
	}
	return "", "", fmt.Errorf("unknown kind %q", kind)
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func guessName(kind, raw string) string {
	raw = strings.Trim(raw, "/ ")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	if strings.HasPrefix(raw, "UC") && len(raw) == 24 {
		raw = raw[2:]
	}
	if strings.HasPrefix(raw, "PL") {
		raw = "playlist"
	}
	raw = strings.ReplaceAll(raw, "@", "")
	return kind + ":" + raw
}

// ---- rm ----

func (a *App) Rm(id int64) error {
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeleteSource(id); err != nil {
		return err
	}
	fmt.Printf("removed source #%d\n", id)
	return nil
}

// ---- enumerate ----

func (a *App) Enumerate(concurrency, limit int, onlySource int64) error {
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	cfgs := a.sites()
	srcs, err := st.ListSources(true)
	if err != nil {
		return err
	}
	lim := politeness.New(1500 * time.Millisecond)
	type job struct {
		src model.Source
	}
	var jobs []job
	for _, s := range srcs {
		if onlySource > 0 && s.ID != onlySource {
			continue
		}
		jobs = append(jobs, job{s})
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	sem := make(chan struct{}, concurrency)
	errs := make(chan error, len(jobs))
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := a.enumOne(st, cfgs, lim, j.src, limit); err != nil {
				fmt.Fprintf(os.Stderr, "source #%d %s: %v\n", j.src.ID, j.src.URL, err)
				errs <- err
			}
		}(j)
	}
	wg.Wait()
	close(errs)
	n := 0
	for range errs {
		n++
	}
	if n > 0 {
		return fmt.Errorf("%d source(s) failed", n)
	}
	return nil
}

func (a *App) enumOne(st *store.Store, cfgs map[string]sites.Site, lim *politeness.Limiter, src model.Source, limit int) error {
	fn := enum.ForKind(src.Kind)
	if fn == nil {
		return fmt.Errorf("no enumerator for %s", src.Kind)
	}
	cfg := cfgs[src.Site]
	host := hostOf(src.URL)
	if host != "" {
		lim.Wait(host)
	}
	count, complete, err := fn(src.URL, src.Query, cfg, limit, func(e enum.Entry) error {
		if err := st.UpsertVideo(model.Video{
			SourceID:  src.ID,
			VideoID:   e.VideoID,
			URL:       e.URL,
			Title:     e.Title,
			Duration:  e.Duration,
			Published: e.Published,
			Channel:   e.Channel,
		}); err != nil {
			return err
		}
		if len(e.Files) > 0 {
			return st.UpsertFiles(src.ID, e.VideoID, e.Files)
		}
		return nil
	})
	if err != nil {
		if host != "" {
			lim.NoteError(host)
		}
		return err
	}
	if host != "" {
		lim.NoteSuccess(host)
	}
	if err := st.SetSourceEnum(src.ID, int64(count), complete); err != nil {
		return err
	}
	mark := "complete"
	if !complete {
		mark = "partial"
	}
	fmt.Printf("source #%d %-22s %-9s %6d entries (%s)\n", src.ID, src.Kind, src.URL, count, mark)
	return nil
}

func hostOf(rawurl string) string {
	rawurl = strings.TrimPrefix(rawurl, "https://")
	rawurl = strings.TrimPrefix(rawurl, "http://")
	return strings.SplitN(rawurl, "/", 2)[0]
}

// ---- download ----

func (a *App) Download(limit, workers int, policy dl.Policy) error {
	if err := dl.EnsureDir(a.OutDir); err != nil {
		return err
	}
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	pool := dl.NewPool(st, a.sites(), a.OutDir, policy, workers)
	if err := pool.Run(limit); err != nil {
		return err
	}
	d, f, s := pool.Counts()
	fmt.Printf("download pass done: %d ok, %d failed, %d skipped\n", d, f, s)
	return nil
}

// ---- crawl-loop ----

func (a *App) CrawlLoop(every int, limit, workers int, rounds int, policy dl.Policy) error {
	if err := dl.EnsureDir(a.OutDir); err != nil {
		return err
	}
	round := 0
	for {
		round++
		fmt.Printf("=== round %d %s ===\n", round, time.Now().Format("15:04:05"))
		if err := a.Enumerate(2, 0, 0); err != nil {
			fmt.Fprintf(os.Stderr, "enumerate: %v\n", err)
		}
		if err := a.Download(limit, workers, policy); err != nil {
			fmt.Fprintf(os.Stderr, "download: %v\n", err)
		}
		if rounds > 0 && round >= rounds {
			return nil
		}
		time.Sleep(time.Duration(every) * time.Second)
	}
}

// ---- status / list ----

func (a *App) Status() error {
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	srcs, err := st.ListSources(false)
	if err != nil {
		return err
	}
	counts, err := st.CountByStatus()
	if err != nil {
		return err
	}
	fmt.Printf("%d sources; videos: ", len(srcs))
	keys := []string{}
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d ", k, counts[k])
	}
	fmt.Println()
	for _, s := range srcs {
		mark := ""
		if !s.EnumComplete {
			mark = " (partial)"
		}
		fmt.Printf("  #%d %-22s %-45s %6d entries%s\n", s.ID, s.Kind, s.Name, s.EnumCount, mark)
	}
	return nil
}

func (a *App) List(status string, jsonOut bool, limit int) error {
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	rows, err := st.VideoRows(status, limit)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		for _, v := range rows {
			enc.Encode(v)
		}
		return nil
	}
	for _, v := range rows {
		fmt.Printf("%-12s %-30s %8ds %-20s %s\n",
			v.Status, trunc(v.Title, 30), v.Duration, trunc(v.Channel, 20), v.URL)
	}
	return nil
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Flag helpers for subcommands.
type FlagSet struct{ *flag.FlagSet }

func (f *FlagSet) Int(name string, def int, usage string) *int {
	return f.FlagSet.Int(name, def, usage)
}
func (f *FlagSet) Str(name, def, usage string) *string {
	return f.FlagSet.String(name, def, usage)
}
func (f *FlagSet) Bool(name string, def bool, usage string) *bool {
	return f.FlagSet.Bool(name, def, usage)
}
func (f *FlagSet) Int64(name string, def int64, usage string) *int64 {
	return f.FlagSet.Int64(name, def, usage)
}

var _ = flag.NewFlagSet
