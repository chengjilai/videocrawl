// HN discovery backend for the 'discover' command: Algolia's official HN
// search API (hn.algolia.com — reachable directly, proxy fallback for
// proxy-only hosts). The discover command classifies hits into single-talk
// videos, channel/playlist seeds, and everything else.
package enum

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"videocrawl/internal/netx"
	"videocrawl/internal/sites"
)

// HNHit: one Algolia story hit (the fields discover uses).
type HNHit struct {
	ObjectID string `json:"objectID"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Points   int    `json:"points"`
}

// HNKind: how a hit's URL classifies for discovery.
type HNKind int

const (
	HNDrop  HNKind = iota // not a video we can enumerate (or a lead with --include-known)
	HNVideo               // a single talk video (youtube watch / media.ccc.de/v/)
	HNSeed                // a youtube channel/playlist worth seeding
)

// HNClassifyURL classifies a story URL: youtube watch links (watch?v=,
// youtu.be, shorts, live, embed, /v/) and media.ccc.de/v/ talks are
// videos; youtube channel/playlist links are seeds; everything else drops.
// Video URLs are canonicalized so dedup across queries works.
func HNClassifyURL(raw string) (HNKind, string) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return HNDrop, ""
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(u.Path, "/")
	switch host {
	case "www.youtube.com", "youtube.com", "m.youtube.com", "music.youtube.com":
		switch {
		case strings.HasPrefix(path, "/watch"):
			if id := u.Query().Get("v"); id != "" {
				return HNVideo, "https://www.youtube.com/watch?v=" + id
			}
		case strings.HasPrefix(path, "/shorts/"), strings.HasPrefix(path, "/live/"),
			strings.HasPrefix(path, "/embed/"), strings.HasPrefix(path, "/v/"):
			if id := lastSeg(path); id != "" {
				return HNVideo, "https://www.youtube.com/watch?v=" + id
			}
		case strings.HasPrefix(path, "/playlist"):
			return HNSeed, raw
		case strings.HasPrefix(path, "/channel/"), strings.HasPrefix(path, "/c/"),
			strings.HasPrefix(path, "/user/"), strings.HasPrefix(path, "/@"):
			return HNSeed, raw
		}
	case "youtu.be":
		if id := lastSeg(path); id != "" {
			return HNVideo, "https://www.youtube.com/watch?v=" + id
		}
	case "media.ccc.de":
		if strings.HasPrefix(path, "/v/") {
			if g := lastSeg(path); g != "" {
				return HNVideo, "https://media.ccc.de/v/" + g
			}
		}
	}
	return HNDrop, ""
}

func lastSeg(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return strings.Trim(path[i+1:], "/")
	}
	return strings.Trim(path, "/")
}

// SearchHN queries Algolia for stories about query with at least minPoints
// points. Direct transport first; on failure, retried through the site
// proxy (the smart-proxy / tunnel).
func SearchHN(query string, minPoints, hitsPerPage int) ([]HNHit, error) {
	u := "https://hn.algolia.com/api/v1/search?query=" + url.QueryEscape(query) +
		"&tags=story&numericFilters=" + url.QueryEscape(fmt.Sprintf("points>%d", minPoints)) +
		fmt.Sprintf("&hitsPerPage=%d", hitsPerPage) + "&attributesToHighlight=none"
	client := netx.Client("", "", 60*time.Second) // direct first
	b, err := hnGet(client, u)
	if err != nil {
		// proxy fallback (youtube site config: "auto" proxy resolution)
		client = netx.Client("proxy", sites.ProxyURL(sites.Defaults()["youtube"]), 60*time.Second)
		b, err = hnGet(client, u)
		if err != nil {
			return nil, err
		}
	}
	return parseHNSearch(b)
}

func hnGet(client *http.Client, u string) ([]byte, error) {
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("hn.algolia.com %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func parseHNSearch(b []byte) ([]HNHit, error) {
	var d struct {
		Hits []HNHit `json:"hits"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return d.Hits, nil
}
