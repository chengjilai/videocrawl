// The 'discover' command: corpus-driven discovery of new talks. Queries are
// generated from the uploaded-talk corpus (querygen.go), run through the
// requested backends (yt search / HN Algolia / media.ccc.de search), gated
// (dedup vs the videos table + corpus duplicates, topic filter, semantic
// title gate, latin script, shorts/live, speaker boost), ranked, and
// printed as a table. With --seed N the top-N ranked candidates are queued
// into a 'discover' source (lazily created, idempotent) so the crawl-loop
// downloads them in relevance order; `upload --upload-allowlist disc`
// republishes them. Without --seed the handoff is
// 'videocrawl add youtube-playlist <url>' for a single talk.
package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"videocrawl/internal/dl"
	"videocrawl/internal/enum"
	"videocrawl/internal/model"
	"videocrawl/internal/politeness"
	"videocrawl/internal/score"
	"videocrawl/internal/sites"
	"videocrawl/internal/store"
	"videocrawl/internal/yt"
)

// DiscoverOpts: 'discover' command flags.
type DiscoverOpts struct {
	Limit        int      // max ranked results
	Queries      []string // extra --query strings (appended to generated)
	Sources      []string // yt, hn, ccc
	Threshold    float64  // semantic gate
	Topics       string   // topical filter (kw,-exclude), '' = off
	HNMinPoints  int      // HN story points floor
	PerQuery     int      // candidate cap per query (ytsearch depth, finalists)
	Transcripts  int      // transcript-check the top N survivors (0 = off)
	Seed         int      // --seed N: queue the top-N ranked candidates into the discover source (0 = off)
	IncludeKnown bool     // show already-known/off-profile hits instead of dropping
	JSON         bool     // JSONL output
}

// stringList: repeatable --query flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Discover parses the 'discover' flags and runs the pipeline.
func Discover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	limit := fs.Int("limit", 20, "max ranked results")
	var queries stringList
	fs.Var(&queries, "query", "extra search query (repeatable)")
	sources := fs.String("sources", "yt,hn,ccc", "backends: yt,hn,ccc")
	threshold := fs.Float64("threshold", discoverSemanticDefault(), "semantic title gate (score of title+channel vs the corpus)")
	topics := fs.String("topics", "", "comma-separated topic filter (kw1,-kw2; '' = off)")
	hnMin := fs.Int("hn-min-points", 50, "HN story points floor")
	perQuery := fs.Int("per-query", 10, "candidate cap per query")
	transcripts := fs.Int("transcripts", 0, "fetch auto-subs + transcript-score the top N survivors (0 = off)")
	seed := fs.Int("seed", 0, "queue the top-N ranked candidates into the discover source for the crawl-loop (0 = off)")
	includeKnown := fs.Bool("include-known", false, "show already-known/off-profile hits instead of dropping")
	jsonOut := fs.Bool("json", false, "JSONL output")
	fs.Parse(args)

	var srcs []string
	for _, s := range strings.Split(*sources, ",") {
		if s = strings.TrimSpace(s); s != "" {
			srcs = append(srcs, s)
		}
	}
	if len(srcs) == 0 {
		srcs = []string{"yt", "hn", "ccc"}
	}
	a := Default()
	return a.discoverRun(ctx, DiscoverOpts{
		Limit:        *limit,
		Queries:      queries,
		Sources:      srcs,
		Threshold:    *threshold,
		Topics:       *topics,
		HNMinPoints:  *hnMin,
		PerQuery:     *perQuery,
		Transcripts:  *transcripts,
		Seed:         *seed,
		IncludeKnown: *includeKnown,
		JSON:         *jsonOut,
	})
}

// discoverSemanticDefault: --threshold default honors VIDEOCRAWL_SEMANTIC_THRESHOLD
// (the enumerate/download gate's override).
func discoverSemanticDefault() float64 {
	t := 0.16 // discovery default is stricter than enumeration (search returns
	// arbitrary videos; 0.12 admitted politics/news channels) — speaker
	// boost rescues low-title-score corpus-speaker talks.
	if v := os.Getenv("VIDEOCRAWL_SEMANTIC_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			t = f
		}
	}
	return t
}

// transcriptThreshold: VIDEOCRAWL_TRANSCRIPT_THRESHOLD, default 0.15 (the
// download gate's transcript score floor).
func transcriptThreshold() float64 {
	t := 0.15
	if v := os.Getenv("VIDEOCRAWL_TRANSCRIPT_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			t = f
		}
	}
	return t
}

// candidate: one discovered talk passing the gates.
type candidate struct {
	Source   string // yt | hn | ccc
	URL      string
	VideoID  string
	Title    string
	Channel  string
	Duration int64
	Score    float64
	Known    bool
	Speaker  string // speaker-boost name ("" = none)
}

// gates: the shared discovery gates.
type gates struct {
	corpusTitles []string // uploaded-talk titles (jaccard dedup)
	scorer       *score.SemanticScorer
	filter       func(enum.Entry) bool // topicFilter(topics)
	threshold    float64
	includeKnown bool
	knownURLs    map[string]bool
	knownIDs     map[string]bool
	speakers     []string // corpus speaker names (boost)
}

func newGates(corpusFull, corpusTitles []string, knownURLs, knownIDs map[string]bool, o DiscoverOpts) *gates {
	return &gates{
		corpusTitles: corpusTitles,
		scorer:       score.NewSemanticScorer(corpusFull),
		filter:       topicFilter(o.Topics),
		threshold:    o.Threshold,
		includeKnown: o.IncludeKnown,
		knownURLs:    knownURLs,
		knownIDs:     knownIDs,
		speakers:     SpeakerNames(corpusTitles),
	}
}

// known reports whether the candidate is already in the videos table (URL
// or video id — youtube ids extracted from any URL form).
func (g *gates) known(c *candidate) bool {
	if c.VideoID != "" && g.knownIDs[c.VideoID] {
		return true
	}
	if g.knownURLs[strings.TrimSuffix(c.URL, "/")] {
		return true
	}
	if id := ytVideoID(c.URL); id != "" && g.knownIDs[id] {
		return true
	}
	return false
}

// gate applies the shared gates. Candidates already known (videos table or
// a >=0.7 title-Jaccard duplicate of an uploaded talk) drop unless
// --include-known (then they pass, tagged Known). The SPEAKER BOOST applies
// here too: a title/channel containing a corpus speaker name scores
// max(score, threshold) and passes the semantic gate. Returns false to drop.
func (g *gates) gate(c *candidate) bool {
	if g.known(c) || corpusJaccard(c.Title, g.corpusTitles) >= 0.7 {
		if !g.includeKnown {
			return false
		}
		c.Known = true
	}
	if g.filter != nil && !g.filter(enum.Entry{Title: c.Title, Channel: c.Channel}) {
		return false
	}
	c.Score = g.scorer.Score(c.Title + " " + c.Channel)
	if name := matchSpeaker(c.Title+" "+c.Channel, g.speakers); name != "" {
		c.Speaker = name
		c.Score = math.Max(c.Score, g.threshold)
	}
	if g.threshold > 0 && c.Score < g.threshold {
		return false
	}
	if !mostlyLatin(c.Title + " " + c.Channel) {
		return false
	}
	return true
}

// finalize re-scores a candidate after enrichment (yt GetMeta / ccc
// abstract) and applies the SPEAKER BOOST: when the title or channel
// contains a corpus speaker name, score = max(score, threshold) so the talk
// passes regardless. Returns false when the enriched score drops below the
// threshold (unless boosted).
func (g *gates) finalize(c *candidate, scoreText string) bool {
	c.Score = g.scorer.Score(scoreText)
	if name := matchSpeaker(c.Title+" "+c.Channel, g.speakers); name != "" {
		c.Speaker = name
		c.Score = math.Max(c.Score, g.threshold)
	}
	return c.Score >= g.threshold || c.Speaker != ""
}

// corpusJaccard: max token-set Jaccard of title against any corpus title
// (corpus tokenizer semantics). >=0.7 means "the same talk, already
// uploaded" — the dedup complement to the videos table.
func corpusJaccard(title string, corpusTitles []string) float64 {
	a := tokenSet(title)
	if len(a) == 0 {
		return 0
	}
	best := 0.0
	for _, ct := range corpusTitles {
		b := tokenSet(ct)
		if len(b) == 0 {
			continue
		}
		inter := 0
		for t := range a {
			if b[t] {
				inter++
			}
		}
		j := float64(inter) / float64(len(a)+len(b)-inter)
		if j > best {
			best = j
		}
	}
	return best
}

// tokenSet: the corpus tokenizer applied to one text (lowercase tokens,
// len>=2, score STOPWORDS dropped), as a set.
func tokenSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, tok := range qTokRe.FindAllString(strings.ToLower(s), -1) {
		if len(tok) < 2 || score.STOPWORDS[tok] {
			continue
		}
		set[tok] = true
	}
	return set
}

// ytIDRe: youtube video id from watch/shorts/live/youtu.be URL forms.
var ytIDRe = regexp.MustCompile(`[?&]v=([A-Za-z0-9_-]{11})|youtu\.be/([A-Za-z0-9_-]{11})`)

func ytVideoID(raw string) string {
	m := ytIDRe.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// cccGUID: the media.ccc.de/v/<guid> last path segment.
func cccGUID(raw string) string {
	if i := strings.LastIndex(raw, "/v/"); i >= 0 {
		g := strings.Trim(raw[i+3:], "/")
		if g != "" && !strings.Contains(g, "/") {
			return g
		}
	}
	return ""
}

// knownVideoSets: the videos table's URLs and ids for dedup.
func knownVideoSets(st *store.Store) (urls, ids map[string]bool, err error) {
	rows, err := st.VideoRows("", 1<<30)
	if err != nil {
		return nil, nil, err
	}
	urls = map[string]bool{}
	ids = map[string]bool{}
	for _, v := range rows {
		urls[strings.TrimSuffix(v.URL, "/")] = true
		if v.VideoID != "" {
			ids[v.VideoID] = true
		}
		if id := ytVideoID(v.URL); id != "" {
			ids[id] = true
		}
	}
	return urls, ids, nil
}

// loadCorpusTitles: the uploaded-talk titles (config/semantic-corpus.json,
// VIDEOCRAWL_CORPUS override) — the query/discovery source. The generic
// exemplar base is intentionally NOT included: queries derive from the
// actual uploads.
func (a *App) loadCorpusTitles() []string {
	path := os.Getenv("VIDEOCRAWL_CORPUS")
	if path == "" {
		path = "config/semantic-corpus.json"
	}
	var titles []string
	if data, err := os.ReadFile(path); err == nil {
		var d struct {
			Corpus []string `json:"corpus"`
		}
		if json.Unmarshal(data, &d) == nil {
			titles = d.Corpus
		}
	}
	return titles
}

// hnLine: a seed-suggestion or lead (HN non-candidate hits).
type hnLine struct {
	url   string
	title string
}

// discoverSeedURL: sentinel URL for the single discover source (AddSource
// dedups by (url,query) so seeding is idempotent).
const discoverSeedURL = "discover://topic-search"

// seedResults: queue the top-N ranked candidates into the single discover
// source (lazily created; idempotent). Each row carries the candidate's
// semantic score so the download/upload queues pull it in relevance order.
// INSERT OR IGNORE: first row wins — existing rows keep their status
// (failed/done rows are not resurrected). Returns the source id and the
// number of NEW rows inserted.
func (a *App) seedResults(st *store.Store, results []candidate, n int) (int64, int, error) {
	srcID, err := st.AddSource(model.KindDiscover, discoverSeedURL, "", "discover", "")
	if err != nil {
		return 0, 0, err
	}
	queued := 0
	for _, c := range results[:n] {
		res, err := st.UpsertVideo(model.Video{
			SourceID: srcID,
			VideoID:  c.VideoID,
			URL:      c.URL,
			Title:    c.Title,
			Duration: c.Duration,
			Channel:  c.Channel,
			Score:    c.Score,
		})
		if err != nil {
			return srcID, queued, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return srcID, queued, err
		}
		queued += int(affected)
	}
	return srcID, queued, nil
}

// discoverRun: the full pipeline — queries, backends, gates, ranking,
// transcript stage, output.
func (a *App) discoverRun(ctx context.Context, o DiscoverOpts) error {
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.PerQuery <= 0 {
		o.PerQuery = 10
	}
	if o.HNMinPoints <= 0 {
		o.HNMinPoints = 50
	}
	corpusTitles := a.loadCorpusTitles()
	queries := GenerateQueries(corpusTitles, o.Queries...)
	if len(corpusTitles) == 0 && len(o.Queries) == 0 {
		return fmt.Errorf("discover: no corpus at config/semantic-corpus.json and no --query given")
	}
	if len(corpusTitles) == 0 {
		fmt.Fprintln(os.Stderr, "discover: warning: corpus missing — using --query only")
	}
	st, err := a.open()
	if err != nil {
		return err
	}
	defer st.Close()
	knownURLs, knownIDs, err := knownVideoSets(st)
	if err != nil {
		return err
	}
	g := newGates(a.loadCorpus(), corpusTitles, knownURLs, knownIDs, o)
	lim := politeness.New(1500 * time.Millisecond)

	var results []candidate
	seen := map[string]bool{}
	emit := func(c candidate) {
		if seen[c.URL] {
			return
		}
		seen[c.URL] = true
		results = append(results, c)
	}
	var seeds, leads []hnLine
	seedLine := func(url, title string) { seeds = append(seeds, hnLine{url, title}) }
	leadLine := func(url, title string) { leads = append(leads, hnLine{url, title}) }

	for _, src := range o.Sources {
		for _, q := range queries {
			if dl.Expired(ctx, time.Time{}) {
				break
			}
			var err error
			switch src {
			case "yt":
				err = a.discoverYT(ctx, g, lim, q, o.PerQuery, emit)
			case "hn":
				err = a.discoverHN(ctx, g, lim, q, o, emit, seedLine, leadLine)
			case "ccc":
				err = a.discoverCCC(ctx, g, lim, q, o.PerQuery, emit)
			default:
				return fmt.Errorf("discover: unknown source %q (want yt, hn, ccc)", src)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "discover %s %q: %v\n", src, q, err)
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].Source != results[j].Source {
			return results[i].Source < results[j].Source
		}
		return results[i].Title < results[j].Title
	})
	if len(results) > o.Limit {
		results = results[:o.Limit]
	}

	if o.JSON {
		enc := json.NewEncoder(os.Stdout)
		for _, s := range seeds {
			enc.Encode(map[string]string{"kind": "seed", "url": s.url, "title": s.title})
		}
		for _, l := range leads {
			enc.Encode(map[string]string{"kind": "lead", "url": l.url, "title": l.title})
		}
		for i, c := range results {
			enc.Encode(map[string]any{
				"kind": "result", "rank": i + 1, "score": c.Score,
				"source": c.Source, "channel": c.Channel, "title": c.Title,
				"url": c.URL, "speaker": c.Speaker, "known": c.Known,
			})
		}
	} else {
		for _, s := range seeds {
			kind := "youtube-playlist"
			if !strings.Contains(s.url, "/playlist") {
				kind = "youtube-channel"
			}
			fmt.Printf("seed: add %s %s  # %s\n", kind, s.url, trunc(s.title, 60))
		}
		for _, l := range leads {
			fmt.Printf("lead: %s — %s\n", trunc(l.title, 60), l.url)
		}
		for i, c := range results {
			scoreCell := fmt.Sprintf("%.2f", c.Score)
			if c.Speaker != "" {
				scoreCell += " speaker:" + c.Speaker
			}
			title := c.Title
			if c.Known {
				title += " [known]"
			}
			fmt.Printf("%3d  %-6s  %-3s  %-22s  %s / %s\n",
				i+1, scoreCell, c.Source, trunc(c.Channel, 22), trunc(title, 60), c.URL)
		}
		if len(results) > 0 && o.Seed <= 0 {
			fmt.Println("hint: add a single talk with: videocrawl add youtube-playlist <url>")
		}
	}

	if o.Transcripts > 0 && len(results) > 0 {
		n := o.Transcripts
		if n > len(results) {
			n = len(results)
		}
		if err := a.discoverTranscripts(ctx, g, lim, results[:n], o.JSON); err != nil {
			fmt.Fprintf(os.Stderr, "discover: transcripts: %v\n", err)
		}
	}

	if o.Seed > 0 && len(results) > 0 {
		n := o.Seed
		if n > len(results) {
			n = len(results)
		}
		srcID, queued, err := a.seedResults(st, results, n)
		if err != nil {
			// seed failure must fail the run (exit != 0): a --json consumer
			// would otherwise see no "seeded" record and cannot detect it.
			return fmt.Errorf("seed: %w", err)
		}
		if o.JSON {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": "seeded", "source_id": srcID, "queued": queued, "candidates": n})
		} else {
			fmt.Printf("seeded %d new candidate(s) into source #%d (kind discover); crawl-loop downloads them in relevance order\n", queued, srcID)
			fmt.Println("hint: allow uploads with: videocrawl upload --upload-allowlist disc")
		}
	}
	return nil
}

// ---- backends ----

// discoverYT: 'ytsearch<N>:<q>' via yt-dlp flat enumeration (the youtube
// site config's EnumArgs + proxy; politeness gate per search). Flat search
// entries lack upload_date and shorts/live status, so the survivors
// (top per-query by preliminary score) get one GetMeta each: that yields
// duration/media_type/live_status/channel; shorts and live streams drop.
func (a *App) discoverYT(ctx context.Context, g *gates, lim *politeness.Limiter, q string, perQuery int, emit func(candidate)) error {
	cfg := a.sites()["youtube"]
	proxy := sites.ProxyURL(cfg)
	var raws []yt.FlatEntry
	lim.Wait("youtube.com")
	_, _, err := yt.Enum(cfg.EnumArgs, cfg.Cookies, proxy, fmt.Sprintf("ytsearch%d:%s", perQuery, q), func(e yt.FlatEntry) error {
		raws = append(raws, e)
		return nil
	})
	if err != nil {
		lim.NoteError("youtube.com")
		return fmt.Errorf("ytsearch: %w", err)
	}
	lim.NoteSuccess("youtube.com")

	var passed []candidate
	for _, e := range raws {
		if e.ID == "" || e.Title == "" {
			continue
		}
		c := candidate{
			Source:  "yt",
			VideoID: e.ID,
			URL:     "https://www.youtube.com/watch?v=" + e.ID,
			Title:   e.Title,
			Channel: e.Channel,
		}
		if !g.gate(&c) {
			continue
		}
		passed = append(passed, c)
	}
	sort.Slice(passed, func(i, j int) bool { return passed[i].Score > passed[j].Score })
	if len(passed) > perQuery {
		passed = passed[:perQuery]
	}
	for i := range passed {
		if dl.Expired(ctx, time.Time{}) {
			break
		}
		c := &passed[i]
		lim.Wait("youtube.com")
		meta, err := yt.GetMeta(cfg.EnumArgs, cfg.Cookies, proxy, c.URL)
		if err != nil {
			lim.NoteError("youtube.com")
			continue // can't verify shorts/live — drop
		}
		lim.NoteSuccess("youtube.com")
		if meta.MediaType == "short" {
			continue
		}
		switch meta.LiveStatus {
		case "is_live", "is_upcoming", "post_live":
			continue
		}
		if meta.Title != "" {
			c.Title = meta.Title
		}
		c.Channel = meta.Channel
		c.Duration = yt.DurationSeconds(meta.Duration)
		if !g.finalize(c, c.Title+" "+c.Channel) {
			continue
		}
		emit(*c)
	}
	return nil
}

// discoverHN: Algolia story search (points >= floor); youtube watch links
// and media.ccc.de/v/ talks are candidates through the gates; youtube
// channel/playlist links print as seed-suggestion lines; everything else
// drops (or prints as a lead line with --include-known).
func (a *App) discoverHN(ctx context.Context, g *gates, lim *politeness.Limiter, q string, o DiscoverOpts, emit func(candidate), seedLine, leadLine func(url, title string)) error {
	lim.Wait("hn.algolia.com")
	hits, err := enum.SearchHN(q, o.HNMinPoints, 20)
	if err != nil {
		lim.NoteError("hn.algolia.com")
		return fmt.Errorf("hn: %w", err)
	}
	lim.NoteSuccess("hn.algolia.com")
	n := 0
	for _, h := range hits {
		if dl.Expired(ctx, time.Time{}) {
			break
		}
		kind, canon := enum.HNClassifyURL(h.URL)
		switch kind {
		case enum.HNVideo:
			if n >= o.PerQuery {
				continue
			}
			c := candidate{Source: "hn", URL: canon, Title: h.Title}
			if id := ytVideoID(canon); id != "" {
				c.VideoID = id
			} else if gid := cccGUID(canon); gid != "" {
				c.VideoID = gid
				c.Channel = "media.ccc.de"
			}
			if !g.gate(&c) {
				continue
			}
			if !g.finalize(&c, c.Title+" "+c.Channel) {
				continue
			}
			n++
			emit(c)
		case enum.HNSeed:
			seedLine(h.URL, h.Title)
		default:
			if o.IncludeKnown {
				leadLine(h.URL, h.Title)
			}
		}
	}
	return nil
}

// discoverCCC: media.ccc.de event search; the top finalists get their
// /public/events/<guid> abstract fetched and are re-scored on
// title+description.
func (a *App) discoverCCC(ctx context.Context, g *gates, lim *politeness.Limiter, q string, perQuery int, emit func(candidate)) error {
	cfg := a.sites()["ccc"]
	client := enum.CCCClient(cfg)
	lim.Wait("media.ccc.de")
	events, err := enum.CCCSearch(client, q, 0)
	if err != nil {
		lim.NoteError("media.ccc.de")
		return fmt.Errorf("ccc: %w", err)
	}
	lim.NoteSuccess("media.ccc.de")
	var passed []candidate
	for _, e := range events {
		c := candidate{
			Source:   "ccc",
			VideoID:  e.GUID,
			URL:      "https://media.ccc.de/v/" + e.GUID,
			Title:    e.Title,
			Channel:  strings.Join(e.Persons, ", "),
			Duration: e.Duration,
		}
		if !g.gate(&c) {
			continue
		}
		passed = append(passed, c)
		if len(passed) >= perQuery {
			break
		}
	}
	sort.Slice(passed, func(i, j int) bool { return passed[i].Score > passed[j].Score })
	for i := range passed {
		if dl.Expired(ctx, time.Time{}) {
			break
		}
		c := &passed[i]
		lim.Wait("media.ccc.de")
		ev, err := enum.CCCEventByGUID(client, c.VideoID)
		if err != nil {
			lim.NoteError("media.ccc.de")
			continue
		}
		lim.NoteSuccess("media.ccc.de")
		if !g.finalize(c, c.Title+" "+ev.Description) {
			continue
		}
		emit(*c)
	}
	return nil
}

// ---- transcripts ----

// discoverTranscripts: for the top-N survivors, fetch auto-subs with
// yt-dlp into a temp dir (--skip-download --write-auto-subs --sub-langs
// en,en-orig --sub-format srt/best — the exact pair, NOT the en.* regex
// which fans out into ~11 translated tracks and 429s; see yt.SubtitleCmd),
// retrying with backoff (the subs API 429s through the shared proxy IP),
// extract the plain text with the dl transcript pattern, and score it
// against the corpus
// (VIDEOCRAWL_TRANSCRIPT_THRESHOLD, default 0.15). Report only — nothing
// is written to the DB.
func (a *App) discoverTranscripts(ctx context.Context, g *gates, lim *politeness.Limiter, results []candidate, jsonOut bool) error {
	thr := transcriptThreshold()
	tmp, err := os.MkdirTemp("", "videocrawl-discover-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cfg := a.sites()["youtube"]
	proxy := sites.ProxyURL(cfg)
	scorer := score.NewSemanticScorer(a.loadCorpus())
	enc := json.NewEncoder(os.Stdout)
	for i, c := range results {
		if dl.Expired(ctx, time.Time{}) {
			break
		}
		lim.Wait("youtube.com")
		args := []string{
			"--skip-download", "--write-auto-subs", "--sub-langs", "en,en-orig",
			"--sub-format", "srt/best", "--no-playlist", "--no-warnings",
			"--socket-timeout", "60",
			"-o", "discover-subs/%(id)s.%(ext)s",
			"-P", "home:" + tmp,
		}
		if proxy != "" {
			args = append(args, "--proxy", proxy)
		}
		cmd := exec.Command(yt.Bin(), args...)
		cmd.Args = append(cmd.Args, "--", c.URL)
		_ = yt.RunWithRetry(ctx, cmd, 30*time.Second, 60*time.Second) // failures just mean no subs
		text := dl.TranscriptText(tmp, c.VideoID)
		sc := 0.0
		if text != "" {
			sc = scorer.Score(text)
		}
		status := "none"
		if text != "" {
			if sc >= thr {
				status = "pass"
			} else {
				status = "low"
			}
		}
		if jsonOut {
			enc.Encode(map[string]any{
				"kind": "transcript", "rank": i + 1, "url": c.URL,
				"title": c.Title, "score": sc, "status": status, "threshold": thr,
			})
		} else {
			fmt.Printf("transcript #%d: %s score=%.2f (threshold %.2f) — %s / %s\n",
				i+1, status, sc, thr, trunc(c.Title, 50), c.URL)
		}
	}
	return nil
}
