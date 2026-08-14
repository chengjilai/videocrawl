// Package enum: source discovery. YouTube/bilibili spaces go through yt-dlp
// flat enumeration (verified: ~1 request per 30-100 videos, full history, not
// time-biased); bilibili favorites (收藏夹) use a native wbi-signed REST
// enumerator (bilibili.go). PeerTube/CCC/archive.org/RSS use native REST
// pagination — simple JSON, no client complexity. Every source is paginated
// to the END (oldest videos included), satisfying the no-time-bias
// requirement. Native pagination loops self-throttle at ~1 req/s
// (enumPageSleep): the per-host limiter in app only gates whole enumerations,
// so a single loop must not hammer the API.
package enum

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"videocrawl/internal/model"
	"videocrawl/internal/netx"
	"videocrawl/internal/sites"
	"videocrawl/internal/yt"
)

// enumPageSleep: minimum spacing between page requests of native REST
// enumerators (the app-level limiter only gates whole enumerations).
const enumPageSleep = time.Second

// Entry: one discovered video, canonical across all sources.
type Entry struct {
	VideoID   string
	URL       string
	Title     string
	Duration  int64
	Published string // ISO date
	Channel   string
	Files     []model.File // candidate media files (native-download sources: ccc)
}

// EnumFunc discovers entries for one source.
type EnumFunc func(srcURL, query string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error)

// ForKind picks the enumerator for a source kind.
func ForKind(kind string) EnumFunc {
	switch kind {
	case "youtube-channel", "youtube-playlist", "bilibili-space":
		return enumYTDLP
	case "bilibili-fav":
		return enumBiliFav
	case "peertube-channel", "peertube-search":
		return enumPeerTube
	case "ccc-conf", "ccc-search":
		return enumCCC
	case "archive-query", "archive-audio":
		// archive-audio enumerates like archive-query; normalize guarantees
		// the query carries mediatype:audio.
		return enumArchive
	case "gallica":
		return enumGallica
	case "rss":
		return enumRSS
	}
	return nil
}

// ---- yt-dlp based ----

var errLimit = errors.New("enum limit reached")

func enumYTDLP(srcURL, _ string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	n := 0
	_, ok, err := yt.Enum(cfg.EnumArgs, cfg.Cookies, sites.ProxyURL(cfg), srcURL, func(e yt.FlatEntry) error {
		if limit > 0 && n >= limit {
			return errLimit // kills the yt-dlp process; truncation is not complete
		}
		// flat entries from channel tabs may omit duration (shorts)
		entry := Entry{
			VideoID:  e.ID,
			URL:      canonicalURL(e, srcURL),
			Title:    e.Title,
			Duration: yt.DurationSeconds(e.Duration),
			Channel:  e.Channel,
		}
		if entry.Published == "" {
			entry.Published = yt.ParseUploadDate(e.UploadDate)
		}
		if entry.URL == "" {
			entry.URL = srcURL
		}
		if err := onEntry(entry); err != nil {
			return err
		}
		n++
		return nil
	})
	if errors.Is(err, errLimit) {
		return n, false, nil // truncated: not complete
	}
	return n, ok, err
}

func canonicalURL(e yt.FlatEntry, srcURL string) string {
	if strings.Contains(srcURL, "space.bilibili.com") {
		return fmt.Sprintf("https://www.bilibili.com/video/%s", e.ID)
	}
	if strings.HasPrefix(e.URL, "http") {
		return e.URL
	}
	return ""
}

// ---- PeerTube ----

type ptList struct {
	Total int64 `json:"total"`
	Data  []struct {
		UUID      string `json:"uuid"`
		Name      string `json:"name"`
		Published string `json:"publishedAt"`
		Duration  int64  `json:"duration"`
		Channel   struct {
			DisplayName string `json:"displayName"`
		} `json:"channel"`
	} `json:"data"`
}

func enumPeerTube(srcURL, query string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	base, err := peerTubeBase(srcURL)
	if err != nil {
		return 0, false, err
	}
	api := base + "/api/v1/videos?count=100&sort=publishedAt"
	if query != "" {
		api = base + "/api/v1/search/videos?count=100&search=" + url.QueryEscape(query)
	}
	client := httpClient(cfg)
	n := 0
	start := 0
	for {
		u := fmt.Sprintf("%s&start=%d", api, start)
		resp, err := client.Get(u)
		if err != nil {
			return n, false, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return n, false, fmt.Errorf("peertube %d: %s", resp.StatusCode, strings.TrimSpace(string(b))[:min(160, len(b))])
		}
		var l ptList
		if err := json.Unmarshal(b, &l); err != nil {
			return n, false, err
		}
		if len(l.Data) == 0 {
			return n, true, nil
		}
		for _, v := range l.Data {
			if limit > 0 && n >= limit {
				return n, true, nil
			}
			entry := Entry{
				VideoID:  v.UUID,
				URL:      fmt.Sprintf("%s/w/%s", base, v.UUID),
				Title:    v.Name,
				Duration: v.Duration,
				Channel:  v.Channel.DisplayName,
			}
			if t, err := time.Parse(time.RFC3339, v.Published); err == nil {
				entry.Published = t.Format("2006-01-02")
			}
			if err := onEntry(entry); err != nil {
				return n, false, err
			}
			n++
		}
		start += len(l.Data)
		if int64(start) >= l.Total {
			return n, true, nil
		}
		time.Sleep(enumPageSleep)
	}
}

func peerTubeBase(srcURL string) (string, error) {
	u, err := url.Parse(srcURL)
	if err != nil {
		return "", err
	}
	return "https://" + u.Host, nil
}

// ---- media.ccc.de ----

type cccEvent struct {
	GUID       string   `json:"guid"`
	Title      string   `json:"title"`
	Date       string   `json:"date"`
	Duration   int64    `json:"duration"`
	Persons    []string `json:"persons"`
	Recordings []struct {
		Filename string `json:"filename"`
		URL      string `json:"recording_url"`
		MimeType string `json:"mime_type"`
		Language string `json:"language"`
		Size     int64  `json:"size"`
	} `json:"recordings"`
	URL string `json:"url"`
}

type cccPage struct {
	Events []cccEvent `json:"events"`
	// conferences list form
	Conferences []struct {
		Acronym string     `json:"acronym"`
		Title   string     `json:"title"`
		Events  []cccEvent `json:"events"`
	} `json:"conferences"`
}

func enumCCC(srcURL, query string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	client := httpClient(cfg)
	// conference acronym form: srcURL = https://media.ccc.de/public/conferences/<acronym>
	if strings.Contains(srcURL, "/conferences/") && query == "" {
		resp, err := client.Get(srcURL)
		if err != nil {
			return 0, false, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return 0, false, fmt.Errorf("ccc %d", resp.StatusCode)
		}
		var c cccEvent
		// the conference endpoint returns {events:[...]} — reuse cccPage
		var pg cccPage
		if err := json.Unmarshal(b, &pg); err != nil {
			return 0, false, err
		}
		n := 0
		for _, e := range pg.Events {
			if limit > 0 && n >= limit {
				return n, true, nil
			}
			if err := onEntry(cccEntry(e)); err != nil {
				return n, false, err
			}
			n++
		}
		_ = c
		return n, true, nil
	}
	// events list with search: /public/events/search?q=...&page=N
	page := 1
	n := 0
	for {
		u := "https://media.ccc.de/public/events?page=" + fmt.Sprint(page)
		if query != "" {
			u = "https://media.ccc.de/public/events/search?q=" + url.QueryEscape(query) + "&page=" + fmt.Sprint(page)
		}
		resp, err := client.Get(u)
		if err != nil {
			return n, false, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return n, false, fmt.Errorf("ccc %d", resp.StatusCode)
		}
		var pg cccPage
		if err := json.Unmarshal(b, &pg); err != nil {
			return n, false, err
		}
		if len(pg.Events) == 0 {
			return n, true, nil
		}
		for _, e := range pg.Events {
			if limit > 0 && n >= limit {
				return n, true, nil
			}
			if err := onEntry(cccEntry(e)); err != nil {
				return n, false, err
			}
			n++
		}
		if len(pg.Events) < 50 {
			return n, true, nil
		}
		time.Sleep(enumPageSleep)
		page++
	}
}

func cccEntry(e cccEvent) Entry {
	entry := Entry{
		VideoID:  e.GUID,
		Title:    e.Title,
		Duration: e.Duration,
		Channel:  strings.Join(e.Persons, ", "),
	}
	if len(e.Date) >= 10 {
		if t, err := time.Parse("2006-01-02", e.Date[:10]); err == nil {
			entry.Published = t.Format("2006-01-02")
		}
	}
	if e.URL != "" {
		entry.URL = e.URL
	} else {
		entry.URL = "https://media.ccc.de/v/" + e.GUID
	}
	// pick the best video + subtitle recordings
	pref := []string{"h264-hd", "mp4", "h264-sd", "webm-hd", "webm-sd"}
	for _, want := range pref {
		for _, r := range e.Recordings {
			if strings.Contains(r.Filename, want) && isVideoMime(r.MimeType) {
				entry.Files = append(entry.Files, model.File{URL: r.URL, Size: r.Size * 1 << 20, Ext: extOf(r.Filename), Kind: "video"})
				goto haveVideo
			}
		}
	}
haveVideo:
	for _, r := range e.Recordings {
		if isSubMime(r.MimeType) && (r.Language == "" || strings.HasPrefix(r.Language, "en")) {
			entry.Files = append(entry.Files, model.File{URL: r.URL, Ext: extOf(r.Filename), Kind: "sub"})
		}
	}
	return entry
}

func isVideoMime(m string) bool {
	return strings.HasPrefix(m, "video/")
}
func isSubMime(m string) bool {
	return strings.Contains(m, "x-subrip") || strings.Contains(m, "vtt") || strings.Contains(m, "subtitle")
}
func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return "bin"
}

// ---- archive.org ----

// archive scraping API: cursor-based, 1000 items/request (advancedsearch is
// page-based and caps rows at 100 — 10x more requests for the same depth,
// and its (page*rows) depth limit breaks on very large result sets).
// Verified from here: services/search/v1/scrape returns {items,cursor,total}
// with count min 100 / max 10000.
type iaScrape struct {
	Items []struct {
		Identifier string          `json:"identifier"`
		Title      json.RawMessage `json:"title"`
		Date       string          `json:"date"`
		Creator    json.RawMessage `json:"creator"`
	} `json:"items"`
	Cursor string `json:"cursor"`
}

func enumArchive(srcURL, query string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	client := httpClient(cfg)
	// scrape first; any failure (query rejected, endpoint down) falls back to
	// the known-good advancedsearch loop. Re-enumeration is idempotent (PK
	// dedup), so a mid-run fallback only costs extra requests, never dups.
	n, ok, err := enumArchiveScrape(client, query, limit, onEntry)
	if err == nil {
		return n, ok, nil
	}
	return enumArchiveLegacy(client, query, limit, onEntry)
}

func enumArchiveScrape(client *http.Client, query string, limit int, onEntry func(Entry) error) (int, bool, error) {
	cursor := ""
	n := 0
	for {
		u := "https://archive.org/services/search/v1/scrape?q=" + url.QueryEscape(query) +
			"&fields=identifier,title,date,creator&count=1000"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		resp, err := client.Get(u)
		if err != nil {
			return n, false, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			time.Sleep(30 * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			return n, false, fmt.Errorf("archive.org scrape %d", resp.StatusCode)
		}
		var s iaScrape
		if err := json.Unmarshal(b, &s); err != nil {
			return n, false, err
		}
		for _, d := range s.Items {
			if limit > 0 && n >= limit {
				return n, true, nil
			}
			entry := Entry{
				VideoID:   d.Identifier,
				URL:       "https://archive.org/details/" + d.Identifier,
				Title:     rawString(d.Title),
				Channel:   rawString(d.Creator),
				Published: date10(d.Date),
			}
			if err := onEntry(entry); err != nil {
				return n, false, err
			}
			n++
		}
		if len(s.Items) == 0 || s.Cursor == "" {
			return n, true, nil // exhausted: last page or empty result
		}
		cursor = s.Cursor
		time.Sleep(enumPageSleep)
	}
}

// iaSearch: legacy advancedsearch response (fallback path).
type iaSearch struct {
	Response struct {
		NumFound int64 `json:"numFound"`
		Start    int   `json:"start"`
		Docs     []struct {
			Identifier string          `json:"identifier"`
			Title      json.RawMessage `json:"title"`
			Date       string          `json:"date"`
			Creator    json.RawMessage `json:"creator"`
		} `json:"docs"`
	} `json:"response"`
}

func enumArchiveLegacy(client *http.Client, query string, limit int, onEntry func(Entry) error) (int, bool, error) {
	page := 0
	n := 0
	var total int64 = -1
	for {
		u := "https://archive.org/advancedsearch.php?q=" + url.QueryEscape(query) +
			"&fl[]=identifier&fl[]=title&fl[]=date&fl[]=creator&rows=100&page=" + fmt.Sprint(page+1) + "&output=json"
		resp, err := client.Get(u)
		if err != nil {
			return n, false, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 {
			time.Sleep(30 * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			return n, false, fmt.Errorf("archive.org %d", resp.StatusCode)
		}
		var s iaSearch
		if err := json.Unmarshal(b, &s); err != nil {
			return n, false, err
		}
		total = s.Response.NumFound
		if len(s.Response.Docs) == 0 {
			return n, true, nil
		}
		for _, d := range s.Response.Docs {
			if limit > 0 && n >= limit {
				return n, true, nil
			}
			entry := Entry{
				VideoID:   d.Identifier,
				URL:       "https://archive.org/details/" + d.Identifier,
				Title:     rawString(d.Title),
				Channel:   rawString(d.Creator),
				Published: date10(d.Date),
			}
			if err := onEntry(entry); err != nil {
				return n, false, err
			}
			n++
		}
		if int64(s.Response.Start+len(s.Response.Docs)) >= total {
			return n, true, nil
		}
		time.Sleep(enumPageSleep)
		page++
	}
}

func rawString(r json.RawMessage) string {
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	var arr []string
	if err := json.Unmarshal(r, &arr); err == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

func date10(s string) string {
	if len(s) < 10 {
		return ""
	}
	return s[:10]
}

// ---- RSS ----

func enumRSS(srcURL, _ string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	client := httpClient(cfg)
	resp, err := client.Get(srcURL)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, false, fmt.Errorf("rss %d", resp.StatusCode)
	}
	// minimal RSS/Atom parse: items with media:content / enclosure / link
	type item struct {
		Title     string `xml:"title"`
		Link      string `xml:"link"`
		PubDate   string `xml:"pubDate"`
		Enclosure struct {
			URL  string `xml:"url,attr"`
			Type string `xml:"type,attr"`
		} `xml:"enclosure"`
	}
	type feed struct {
		Items []item `xml:"channel>item"`
		Entry []struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			ID    string `xml:"id"`
			Date  string `xml:"updated"`
		} `xml:"entry"`
	}
	var f feed
	if err := xmlUnmarshal(resp.Body, &f); err != nil {
		return 0, false, err
	}
	n := 0
	for _, it := range f.Items {
		if limit > 0 && n >= limit {
			return n, true, nil
		}
		videoURL := it.Enclosure.URL
		if !strings.Contains(it.Enclosure.Type, "video") {
			videoURL = ""
		}
		if videoURL == "" {
			videoURL = it.Link
		}
		if videoURL == "" {
			continue
		}
		e := Entry{
			VideoID: videoURL,
			URL:     videoURL,
			Title:   it.Title,
		}
		if t, err := parseRSSDate(it.PubDate); err == nil {
			e.Published = t.Format("2006-01-02")
		}
		if err := onEntry(e); err != nil {
			return n, false, err
		}
		n++
	}
	for _, it := range f.Entry {
		if limit > 0 && n >= limit {
			return n, true, nil
		}
		if it.Link == "" && it.ID == "" {
			continue
		}
		u := it.Link
		if u == "" {
			u = it.ID
		}
		e := Entry{
			VideoID: u,
			URL:     u,
			Title:   it.Title,
		}
		if t, err := time.Parse(time.RFC3339, it.Date); err == nil {
			e.Published = t.Format("2006-01-02")
		}
		if err := onEntry(e); err != nil {
			return n, false, err
		}
		n++
	}
	return n, true, nil
}

func httpClient(cfg sites.Site) *http.Client {
	dial := cfg.Dial
	if dial == "" {
		// direct unless the site config actually resolves a proxy URL
		// (netx.Client("proxy", "") would dial a bogus empty proxy)
		dial = ""
		if sites.ProxyURL(cfg) != "" {
			dial = "proxy"
		}
	}
	// A configured proxy (smart-proxy / lab tunnel) takes precedence over
	// the warp-doh dial: media.ccc.de is warp-routed by the smart-proxy
	// (egress policy, f90e10d), and warp-doh dials the LOCAL WARP socks
	// (127.0.0.1:40000) which only exists on aturing.
	if p := sites.ProxyURL(cfg); p != "" {
		dial = "proxy"
	}
	return netx.Client(dial, sites.ProxyURL(cfg), 90*time.Second)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
