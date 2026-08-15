// Package app: CLI commands.
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"videocrawl/internal/yt"

	"videocrawl/internal/dl"
	"videocrawl/internal/enum"
	"videocrawl/internal/model"
	"videocrawl/internal/politeness"
	"videocrawl/internal/score"
	"videocrawl/internal/sites"
	"videocrawl/internal/store"
)

type App struct {
	DBPath       string
	OutDir       string
	SitesJS      string
	UploadScript string // bilibili uploader CLI (upload_web.py)
}

func Default() *App {
	home := mustHome()
	db := envOr("VIDEOCRAWL_DB", home+"/videocrawl.db")
	out := envOr("VIDEOCRAWL_OUT", home+"/Videos/Crawl")
	return &App{
		DBPath:       db,
		OutDir:       out,
		SitesJS:      envOr("VIDEOCRAWL_SITES_JSON", ""),
		UploadScript: envOr("VIDEOCRAWL_UPLOAD_SCRIPT", filepath.Join(home, "src", "bilibili", "upload_web.py")),
	}
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

func (a *App) Add(kind, raw, name, query, topics string) error {
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
	id, err := st.AddSource(kind, url, q, name, topics)
	if err != nil {
		return err
	}
	fmt.Printf("added source #%d: %s %s (%s)\n", id, kind, url, name)
	return nil
}

// readCorpusFile loads the desired-talk reference titles from
// config/semantic-corpus.json (VIDEOCRAWL_CORPUS override). A missing file
// or malformed JSON is an error; callers pick the fallback (loadCorpus
// merges the generic exemplar base, loadCorpusTitles returns an empty
// corpus).
func readCorpusFile() ([]string, error) {
	path := os.Getenv("VIDEOCRAWL_CORPUS")
	if path == "" {
		path = "config/semantic-corpus.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d struct {
		Corpus []string `json:"corpus"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return d.Corpus, nil
}

// loadCorpus reads the desired-talk reference (config/semantic-corpus.json)
// and merges the generic exemplar base. When the file is missing, the
// generic exemplar base alone is used (the semantic gate always runs).
func (a *App) loadCorpus() []string {
	corpus := score.GenericExemplars()
	titles, err := readCorpusFile()
	if err == nil {
		corpus = append(corpus, titles...)
	}
	return corpus
}

// SetTopics updates a source's topic filter (” clears it). The next
// enumeration applies it to new entries; existing queued rows are not
// retroactively filtered (rm + re-add to rebuild a queue).
func (a *App) SetTopics(id int64, topics string) error {
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.SetSourceTopics(id, topics); err != nil {
		return err
	}
	fmt.Printf("source #%d topics = %q\n", id, topics)
	return nil
}

// mostlyLatin reports whether ≥ half the letters are ASCII (drops Khmer,
// Arabic, CJK title floods — the desired corpus is English tech talks).
func mostlyLatin(s string) bool {
	letters, latin := 0, 0
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			letters++
			latin++
		} else if r > 127 {
			letters++
		}
	}
	if letters == 0 {
		return false
	}
	return latin*2 >= letters
}

// topicFilter compiles a source's topics into a matcher. A topic matches
// when it appears case-insensitively in the entry title OR channel. Any
// topic matching keeps the entry (OR semantics); a topic prefixed with '-'
// EXCLUDES the entry when it matches (e.g. "-how to,-switch" drops
// beginner tutorials). Empty topics = keep all — the techcrawl-go-style
// curation is: pick good seeds AND gate by topic here.
func topicFilter(topics string) func(e enum.Entry) bool {
	if strings.TrimSpace(topics) == "" {
		return nil
	}
	var kws, neg []string
	for _, k := range strings.Split(topics, ",") {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if strings.HasPrefix(k, "-") {
			if k = strings.TrimPrefix(k, "-"); k != "" {
				neg = append(neg, k)
			}
			continue
		}
		kws = append(kws, k)
	}
	if len(kws) == 0 && len(neg) == 0 {
		return nil
	}
	return func(e enum.Entry) bool {
		t := strings.ToLower(e.Title + " " + e.Channel)
		for _, k := range neg {
			if strings.Contains(t, k) {
				return false
			}
		}
		if len(kws) == 0 {
			return true
		}
		for _, k := range kws {
			if strings.Contains(t, k) {
				return true
			}
		}
		return false
	}
}

// semanticPass applies the shared semantic core of the score gate: the
// topic filter (nil = already applied by the caller), the corpus score of
// title+channel, and the latin-script check. Returns the score and whether
// the candidate passes. Used by discovery (gates.score); enumeration runs
// the same filter/latin cheap gates itself and page-batches the scoring.
// The threshold gate is deliberately NOT part of the core: discovery's
// speaker boost must run between the score and the threshold check (a
// boosted candidate passes with max(score, threshold)), so each caller
// keeps its own threshold check.
func semanticPass(scorer score.Scorer, filter func(enum.Entry) bool, title, channel string) (float64, bool) {
	if filter != nil && !filter(enum.Entry{Title: title, Channel: channel}) {
		return 0, false
	}
	text := title + " " + channel
	sc := scorer.Score(text)
	if !mostlyLatin(text) {
		return sc, false
	}
	return sc, true
}

// normalize maps a user-provided seed to a canonical URL + query.
func normalize(kind, raw, query string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	switch kind {
	case model.KindYoutubeChannel:
		if strings.HasPrefix(raw, "UC") && len(raw) == 24 {
			return "https://www.youtube.com/playlist?list=UU" + raw[2:], "", nil
		}
		if !strings.HasPrefix(raw, "http") {
			raw = "https://www.youtube.com/" + strings.TrimPrefix(raw, "@")
		}
		if i := strings.Index(raw, "channel/UC"); i >= 0 {
			uc := raw[i+len("channel/"):]
			uc = strings.SplitN(uc, "/", 2)[0]
			if len(uc) == 24 {
				return "https://www.youtube.com/playlist?list=UU" + uc[2:], "", nil
			}
		}
		id, err := resolveChannelID(raw)
		if err != nil {
			return "", "", fmt.Errorf("resolve channel: %w", err)
		}
		if !strings.HasPrefix(id, "UC") || len(id) != 24 {
			return "", "", fmt.Errorf("unexpected channel id %q", id)
		}
		return "https://www.youtube.com/playlist?list=UU" + id[2:], "", nil
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
	case model.KindBilibiliFav:
		fid := enum.BiliFavID(raw)
		if fid == "" {
			return "", "", fmt.Errorf("bilibili-fav needs a numeric media_id (fid) or a favlist URL, got %q", raw)
		}
		return "https://www.bilibili.com/medialist/detail/ml" + fid, "", nil
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
	case model.KindArchiveAudio:
		// seed: a bare lucene query, a --query, or an archive.org/details URL
		// (single item). mediatype:audio is guaranteed in the query so the
		// archive enumerator only ever sees audio items.
		q := strings.TrimSpace(query)
		if q == "" {
			q = raw
		}
		if i := strings.Index(q, "archive.org/details/"); i >= 0 {
			q = "identifier:" + strings.SplitN(q[i+len("archive.org/details/"):], "/", 2)[0]
		} else if strings.HasPrefix(q, "http") {
			return "", "", fmt.Errorf("archive-audio needs a lucene query or an archive.org/details URL, got %q", raw)
		}
		q = strings.TrimSpace(q)
		if q == "" {
			return "", "", fmt.Errorf("archive-audio needs a lucene query (bare or --query)")
		}
		if !strings.Contains(q, "mediatype:audio") {
			q += " AND mediatype:audio"
		}
		return "https://archive.org/advancedsearch.php", q, nil
	case model.KindGallica:
		ark := enum.GallicaArk(raw)
		if ark == "" {
			return "", "", fmt.Errorf("gallica needs an ark URL: https://gallica.bnf.fr/ark:/12148/<ark>")
		}
		return "https://gallica.bnf.fr/ark:/12148/" + ark, "", nil
	case model.KindRSS:
		if !strings.HasPrefix(raw, "http") {
			return "", "", fmt.Errorf("rss needs a feed URL")
		}
		return raw, "", nil
	}
	return "", "", fmt.Errorf("unknown kind %q", kind)
}

// resolveChannelID: one yt-dlp metadata call to turn a handle/URL into a
// channel id. Uses the env proxy (youtube is GFW-blocked).
func resolveChannelID(rawurl string) (string, error) {
	proxy := os.Getenv("VIDEOCRAWL_PROXY")
	if proxy == "" {
		proxy = "http://127.0.0.1:8888"
	}
	args := []string{"-J", "--no-warnings", "--skip-download", "--proxy", proxy, "--", rawurl}
	cmd := exec.Command(yt.Bin(), args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("yt-dlp: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	var m struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		return "", err
	}
	if m.ChannelID == "" {
		return "", fmt.Errorf("no channel_id in response")
	}
	return m.ChannelID, nil
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
	if kind == model.KindBilibiliFav {
		if fid := enum.BiliFavID(raw); fid != "" {
			return "bilibili-fav:" + fid
		}
	}
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

func (a *App) Enumerate(ctx context.Context, concurrency, limit int, onlySource int64, deadline time.Time) error {
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
	// semantic gate: the scorer is built ONCE for the whole pass (corpus
	// read + idf/centroid are the expensive part) and shared by every
	// source's enumOne — per-source construction re-read the corpus file
	// and rebuilt the idf tables for each source.
	scorer := score.New(a.loadCorpus())
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
		if dl.Expired(ctx, deadline) {
			fmt.Fprintf(os.Stderr, "enumerate: stopping early (time budget/signal)\n")
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := a.enumOne(ctx, deadline, st, cfgs, lim, scorer, j.src, limit); err != nil {
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

// errBudget aborts one source's scan when the round time budget or a stop
// signal arrives mid-enumeration (checked at each entry boundary).
var errBudget = errors.New("round time budget exceeded")

func (a *App) enumOne(ctx context.Context, deadline time.Time, st *store.Store, cfgs map[string]sites.Site, lim *politeness.Limiter, scorer score.Scorer, src model.Source, limit int) error {
	if src.Kind == model.KindDiscover {
		return nil // discover sources are seeded statically by 'discover --seed'; nothing to enumerate
	}
	fn := enum.ForKind(src.Kind)
	if fn == nil {
		return fmt.Errorf("no enumerator for %s", src.Kind)
	}
	cfg := cfgs[src.Site]
	host := hostOf(src.URL)
	if host != "" {
		lim.Wait(host)
	}
	filter := topicFilter(src.Topics)
	// semantic gate: score against the desired-talk reference (upload
	// history + generic exemplars); keep entries above the threshold. The
	// scorer is built once in Enumerate and shared by all sources; only
	// the env-driven threshold is resolved here.
	threshold := 0.12
	if v := os.Getenv("VIDEOCRAWL_SEMANTIC_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			threshold = f
		}
	}
	// Page-batched semantic gate: entries pass the cheap gates (topic
	// filter, latin-script check — both pure title/channel predicates)
	// into a page; when the page fills, the page's title+channel texts
	// are scored in ONE batch (embedding scorer) or inline (plain TF-IDF
	// scorer — identical to the old per-entry flow), then each entry
	// above the threshold is upserted. Entries failing the cheap gates
	// never enter a page.
	const pageSize = 64
	var page []enum.Entry
	flush := func() error {
		if len(page) == 0 {
			return nil
		}
		texts := make([]string, len(page))
		for i, e := range page {
			texts[i] = e.Title + " " + e.Channel
		}
		scores := make([]float64, len(page))
		if bs, ok := scorer.(score.BatchScorer); ok {
			scs := bs.BatchScore(texts)
			copy(scores, scs)
		} else {
			for i, t := range texts {
				scores[i] = scorer.Score(t)
			}
		}
		for i, e := range page {
			if scores[i] < threshold {
				continue // semantically unlike the desired corpus — skip
			}
			if _, err := st.UpsertVideo(model.Video{
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
				if err := st.UpsertFiles(src.ID, e.VideoID, e.Files); err != nil {
					return err
				}
			}
		}
		page = page[:0]
		return nil
	}
	count, complete, err := fn(src.URL, src.Query, cfg, limit, func(e enum.Entry) error {
		if dl.Expired(ctx, deadline) {
			// budget/signal reached: keep the page collected so far, then stop
			if err := flush(); err != nil {
				return err
			}
			return errBudget
		}
		// cheap gates first — the topic filter and the latin-script check
		// need no score, so they run before the page's batch score pass.
		if filter != nil && !filter(e) {
			return nil // topic filter — skip
		}
		if !mostlyLatin(e.Title + " " + e.Channel) {
			return nil // non-Latin script — skip
		}
		page = append(page, e)
		if len(page) >= pageSize {
			return flush()
		}
		return nil
	})
	if errors.Is(err, errBudget) {
		// budget/signal reached mid-scan: keep what we have, stop quietly
		_ = st.SetSourceEnum(src.ID, int64(count), false)
		fmt.Printf("source #%d %-22s %-9s %6d entries (stopped: time budget)\n", src.ID, src.Kind, src.URL, count)
		return nil
	}
	if err != nil {
		if host != "" {
			lim.NoteError(host)
		}
		return err
	}
	// normal completion: flush the trailing partial page (the callback only
	// flushes at page-full and on budget expiry).
	if err := flush(); err != nil {
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
	h := strings.SplitN(rawurl, "/", 2)[0]
	// bilibili enumerations hit api/www/space/member hosts from one source;
	// collapse the family so they share a single adaptive limiter slot.
	switch h {
	case "www.bilibili.com", "space.bilibili.com", "api.bilibili.com", "member.bilibili.com":
		return "bilibili.com"
	}
	return h
}

// ---- download ----

func (a *App) Download(ctx context.Context, limit, workers int, policy dl.Policy, deadline time.Time) error {
	if err := dl.EnsureDir(a.OutDir); err != nil {
		return err
	}
	if err := checkDiskFree(a.OutDir); err != nil {
		return err
	}
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	pool := dl.NewPool(st, a.sites(), a.OutDir, policy, workers, dl.WithCorpus(a.loadCorpus()))
	if err := pool.Run(ctx, limit, deadline); err != nil {
		return err
	}
	d, f, s := pool.Counts()
	fmt.Printf("download pass done: %d ok, %d failed, %d skipped\n", d, f, s)
	return nil
}

// ---- crawl-loop ----

func (a *App) CrawlLoop(ctx context.Context, every int, limit, workers, rounds int, maxTime time.Duration, policy dl.Policy) error {
	if err := dl.EnsureDir(a.OutDir); err != nil {
		return err
	}
	round := 0
	for {
		if ctx.Err() != nil {
			fmt.Println("videocrawl: signal received, exiting loop")
			return nil
		}
		round++
		deadline := time.Time{} // 0 = no per-round budget
		if maxTime > 0 {
			deadline = time.Now().Add(maxTime)
		}
		fmt.Printf("=== round %d %s (max-time %s) ===\n", round, time.Now().Format("15:04:05"), maxTime)
		// (3) egress health: a dead tunnel / WARP socks would doom the round.
		if err := checkTransports(); err != nil {
			fmt.Fprintf(os.Stderr, "!! round %d skipped: %v\n", round, err)
		} else {
			if err := a.Enumerate(ctx, 2, 0, 0, deadline); err != nil {
				fmt.Fprintf(os.Stderr, "enumerate: %v\n", err)
			}
			// (1) disk headroom before the download pass.
			if err := checkDiskFree(a.OutDir); err != nil {
				fmt.Fprintf(os.Stderr, "!! download pass skipped: %v (retry next round)\n", err)
			} else if err := a.Download(ctx, limit, workers, policy, deadline); err != nil {
				fmt.Fprintf(os.Stderr, "download: %v\n", err)
			}
		}
		if rounds > 0 && round >= rounds {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Println("videocrawl: signal received, exiting loop")
			return nil
		case <-time.After(time.Duration(every) * time.Second):
		}
	}
}

// ---- upload ----

// uploadGate decides which sources may be republished to bilibili.
type uploadGate struct {
	ids   map[int64]bool
	sites map[string]bool
	kinds map[string]bool
}

func (g *uploadGate) allows(src model.Source) bool {
	return g.ids[src.ID] || g.sites[src.Site] || g.kinds[src.Kind]
}

// parseUploadAllowlist parses --upload-allowlist: the token 'cc' (CC BY
// verified: media.ccc.de, site 'ccc'), 'pt' (peertube), 'disc' (discover
// sources) and/or a comma list of numeric source ids. An empty spec refuses
// everything (licensing discipline: no accidental republishing of videos we
// have no right to mirror).
func parseUploadAllowlist(spec string) (*uploadGate, error) {
	g := &uploadGate{ids: map[int64]bool{}, sites: map[string]bool{}, kinds: map[string]bool{}}
	for _, tok := range strings.Split(spec, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if tok == "cc" {
			g.sites["ccc"] = true
			continue
		}
		if tok == "pt" {
			// peertube sources: trust-at-seed (curated tech channels on
			// tilvids etc.); the topics filter gates content on top.
			g.sites["peertube"] = true
			continue
		}
		if tok == "disc" {
			// discover sources: talks found by 'discover --seed' (ytsearch/HN/ccc
			// search, corpus-driven), gated by the semantic title score at seed
			// time and the transcript score at download time.
			g.kinds[model.KindDiscover] = true
			continue
		}
		id, err := strconv.ParseInt(tok, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("--upload-allowlist: unknown token %q (want 'cc', 'pt', 'disc' or source ids)", tok)
		}
		g.ids[id] = true
	}
	if len(g.ids) == 0 && len(g.sites) == 0 && len(g.kinds) == 0 {
		return nil, fmt.Errorf("upload refused: --upload-allowlist required (licensing discipline); pass 'cc' for CC BY (site=ccc) sources, 'pt'/'disc', or a comma list of source ids")
	}
	return g, nil
}

// Upload publishes done videos to bilibili through the web-path uploader
// (upload_web.py). outDir is the local crawl root (VIDEOCRAWL_OUT); the
// recorded videos.path may live on another machine (lab→aturing sync), so
// --path-prefix-rewrite OLD:NEW maps it to the local tree, and a final
// id-prefix scan of outDir catches stragglers. Failures are logged to
// stderr and the video stays 'done' (retryable next pass); only a parsed
// "SUBMIT OK" bvid moves the row to 'uploaded'.
func (a *App) Upload(limit int, dryRun bool, allowlist, pathPrefixRewrite, outDir string) error {
	gate, err := parseUploadAllowlist(allowlist)
	if err != nil {
		return err
	}
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	if limit <= 0 {
		limit = 1 << 30
	}
	// Fetch with headroom so disallowed rows (youtube) can't starve the
	// round: NextForUpload orders scored rows first (relevance desc), then
	// by earliest-published, and with limit=1 a single skipped row previously
	// blocked everything behind it. The done queue is small (~100 rows), so
	// 50x limit covers it; at most `limit` uploads still happen (the loop
	// breaks at ok == limit).
	fetchN := limit * 50
	if fetchN < 500 {
		fetchN = 500
	}
	rows, err := st.NextForUpload(fetchN)
	if err != nil {
		return err
	}
	if dryRun {
		// apply the SAME licensing gate as the real loop (dry-run must not
		// promise uploads a real run would refuse)
		ok := 0
		skip := 0
		for _, v := range rows {
			src, err := st.GetSource(v.SourceID)
			if err != nil || !gate.allows(src) {
				skip++
				continue
			}
			ok++
			p, _ := rewritePath(v.Path, pathPrefixRewrite)
			fmt.Printf("  %-6s %-12s %-40s %s\n", fmt.Sprintf("#%d", v.SourceID), trunc(v.VideoID, 12), trunc(v.Title, 40), p)
		}
		fmt.Printf("upload: dry-run — would upload %d video(s), %d skipped (allowlist)\n", ok, skip)
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("upload: nothing to upload (no done videos)")
		return nil
	}
	ok, fail, skip := 0, 0, 0
	for _, v := range rows {
		// attempts (ok+fail) count toward limit too: a burst of failures
		// (e.g. bilibili risk-control 403s) must end the round, not hammer
		// the endpoint 64x like the first cc,5 round did.
		if ok+fail >= limit {
			break
		}
		src, err := st.GetSource(v.SourceID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "upload: FAIL %s (%s): source #%d: %v\n", v.VideoID, v.URL, v.SourceID, err)
			fail++
			continue
		}
		if !gate.allows(src) {
			// the gate is keyed on site (cc/pt) or kind (disc) or source id —
			// report which dimension refused so the operator knows the fix.
			fmt.Fprintf(os.Stderr, "upload: SKIP %s (%s): source #%d site=%s kind=%s not in allowlist\n", v.VideoID, v.URL, src.ID, src.Site, src.Kind)
			skip++
			continue
		}
		if err := a.uploadOne(st, v, src, pathPrefixRewrite, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "upload: FAIL %s (%s): %v\n", v.VideoID, v.URL, err)
			fail++
			continue
		}
		ok++
	}
	fmt.Printf("upload pass done: %d ok, %d failed, %d skipped (allowlist)\n", ok, fail, skip)
	return nil
}

// uploadOne shells to upload_web.py for one video and records the bvid.
func (a *App) uploadOne(st *store.Store, v model.Video, src model.Source, pathPrefixRewrite, outDir string) error {
	path, err := findUploadFile(v.Path, v.VideoID, outDir, pathPrefixRewrite)
	if err != nil {
		return err
	}
	if v.URL == "" {
		return fmt.Errorf("no source URL in DB")
	}
	if v.Title == "" {
		return fmt.Errorf("no title in DB")
	}
	title := uploadTitle(v.Title)
	desc := uploadDesc(v, subFileNextTo(path, v.VideoID))
	script := a.UploadScript
	if script == "" {
		script = filepath.Join(mustHome(), "src", "bilibili", "upload_web.py")
	}
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("uploader script %s: %v", script, err)
	}
	fmt.Printf("upload: %s -> bilibili (%s)\n", v.VideoID, path)
	cmd := exec.Command("python3", script, path,
		"--title", title, "--source", v.URL, "--tag", "科技", "--desc", desc)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &out) // progress + SUBMIT OK line
	cmd.Stderr = os.Stderr                       // uploader failures land on stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("uploader: %v", err)
	}
	bvid := parseBVID(out.String())
	if bvid == "" {
		return fmt.Errorf("uploader exited 0 but printed no 'SUBMIT OK — https://www.bilibili.com/video/<bvid>'")
	}
	if err := st.UploadMarked(v.SourceID, v.VideoID, bvid); err != nil {
		return err
	}
	fmt.Printf("upload: %s -> https://www.bilibili.com/video/%s\n", v.VideoID, bvid)
	return nil
}

// uploadDesc builds the repost description; the final line is only added
// when an English-subtitle file was actually downloaded next to the video
// (yt-dlp --write-auto-subs en.* / ccc .srt/.vtt), so we never claim subs
// we are not sure about.
func uploadDesc(v model.Video, hasSubs bool) string {
	d := "转载自: " + v.URL + "\n演讲者: " + v.Channel + "\n版权归原作者与主办方所有, 本视频为转载"
	if hasSubs {
		d += "\n(含英文字幕)"
	}
	return d
}

// uploadTitle truncates to 80 runes (the uploader hard-fails above 80).
func uploadTitle(s string) string {
	r := []rune(s)
	if len(r) <= 80 {
		return s
	}
	return string(r[:79]) + "…"
}

// subFileNextTo reports whether a subtitle file (srt/vtt/ass/sub) with the
// video's id prefix sits next to the video file.
func subFileNextTo(videoPath, videoID string) bool {
	entries, err := os.ReadDir(filepath.Dir(videoPath))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, videoID+"_") {
			continue
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".srt", ".vtt", ".ass", ".sub":
			return true
		}
	}
	return false
}

// findUploadFile resolves the local file for one video: the recorded path
// (after --path-prefix-rewrite), the raw recorded path, then a one-level
// scan of outDir for the id-prefixed file (lab→aturing sync stragglers).
func findUploadFile(path, videoID, outDir, rewrite string) (string, error) {
	candidates := []string{}
	if p, ok := rewritePath(path, rewrite); ok {
		candidates = append(candidates, p)
	}
	if path != "" {
		candidates = append(candidates, path)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	if outDir != "" {
		if p := scanOutDir(outDir, videoID); p != "" {
			return p, nil
		}
	}
	if path != "" {
		return "", fmt.Errorf("file not found: %s", path)
	}
	return "", fmt.Errorf("no local file for %s (path empty)", videoID)
}

// scanOutDir looks one level deep (outDir/<channel>/<file>, the yt-dlp
// output template) for a finished video file with the id prefix.
func scanOutDir(outDir, videoID string) string {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return ""
	}
	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(outDir, dir.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			if f.IsDir() || !strings.HasPrefix(f.Name(), videoID+"_") {
				continue
			}
			switch filepath.Ext(f.Name()) {
			case ".mp4", ".mkv", ".webm", ".flv":
				return filepath.Join(outDir, dir.Name(), f.Name())
			}
		}
	}
	return ""
}

// rewritePath applies the OLD:NEW prefix rewrite: when p starts with OLD,
// returns (NEW + remainder, true); otherwise (p, false).
func rewritePath(p, spec string) (string, bool) {
	if spec == "" {
		return p, false
	}
	old, newp, ok := strings.Cut(spec, ":")
	if !ok || old == "" {
		return p, false
	}
	if !strings.HasPrefix(p, old) {
		return p, false
	}
	return newp + strings.TrimPrefix(p, old), true
}

// parseBVID extracts the bvid from the uploader's "SUBMIT OK —
// https://www.bilibili.com/video/<bvid>" line (em dash tolerant).
var submitOKRe = regexp.MustCompile(`SUBMIT OK.*https://www\.bilibili\.com/video/(BV[0-9A-Za-z]+)`)

func parseBVID(s string) string {
	m := submitOKRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
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
		fmt.Printf("%-12s %-30s %8ds %-20s %10d %s %s\n",
			v.Status, trunc(v.Title, 30), v.Duration, trunc(v.Channel, 20),
			v.SizeBytes, v.SHA256, v.URL)
	}
	return nil
}

// trunc truncates s to n runes TOTAL, ellipsis included (n-1 runes + "…").
// Behavior is pinned by TestTrunc; the post-loop's truncStr (post.go) is the
// n-runes-plus-ellipsis variant.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
