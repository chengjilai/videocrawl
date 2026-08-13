// Package sites: per-site crawl configuration (the same single-registry idea
// as config/sites.json). Defaults are baked in; a JSON file can override.
package sites

import (
	"encoding/json"
	"os"
	"strings"
)

// Site configures one source family: how to reach it (proxy), and the
// yt-dlp arguments for enumeration and download.
type Site struct {
	// Proxy: "" = direct, "auto" = use VIDEOCRAWL_PROXY env if set.
	Proxy string `json:"proxy"`
	// EnumArgs: extra yt-dlp args for enumeration (flat playlist mode).
	EnumArgs []string `json:"enumArgs"`
	// DLArgs: extra yt-dlp args for downloads.
	DLArgs []string `json:"dlArgs"`
	// MaxHeight: resolution cap for downloads (0 = no cap).
	MaxHeight int `json:"maxHeight"`
	// Cookies: path to a Netscape cookies file, or "" for none.
	Cookies string `json:"cookies"`
	// Dial: transport for REST sources + native downloads: "" (direct),
	// "proxy" (smart-proxy), "warp-doh" (DoH + WARP socks; media.ccc.de).
	Dial string `json:"dial"`
}

func Defaults() map[string]Site {
	return map[string]Site{
		"youtube": {
			Proxy:     "auto",
			MaxHeight: 720,
			// default client: tv client DRMs formats without cookies. If a
			// channel 403s, override via sites.json:
			//   EnumArgs/DLArgs: ["--extractor-args", "youtube:player_client=tv"]
			// approximate_date: channel-tab/UU flat entries otherwise lack
			// upload_date, breaking the oldest-first queue order.
			EnumArgs: []string{"--extractor-args", "youtubetab:approximate_date=true"},
			DLArgs:   []string{},
		},
		"bilibili": {
			Proxy:     "", // direct: api.bilibili.com reachable from CN
			MaxHeight: 720,
		},
		"peertube": {
			Proxy:     "", // most instances reachable from CN
			MaxHeight: 720,
		},
		"ccc": {
			Proxy:     "",        // smart-proxy cannot route it (policy gap)
			Dial:      "warp-doh", // DoH resolve + WARP socks, verified path
			MaxHeight: 720,
		},
		"archive": {
			Proxy:     "auto",
			MaxHeight: 720,
		},
		"rss": {
			Proxy:     "auto",
			MaxHeight: 720,
		},
	}
}

// Load merges JSON overrides (e.g. ~/.config/videocrawl/sites.json) onto
// defaults. VIDEOCRAWL_COOKIES_DIR supplies per-site cookie files.
func Load(path string) map[string]Site {
	sites := Defaults()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var ov map[string]Site
			if json.Unmarshal(b, &ov) == nil {
				for k, v := range ov {
					s := sites[k]
					if v.Proxy != "" {
						s.Proxy = v.Proxy
					}
					if v.EnumArgs != nil {
						s.EnumArgs = v.EnumArgs
					}
					if v.DLArgs != nil {
						s.DLArgs = v.DLArgs
					}
					if v.MaxHeight != 0 {
						s.MaxHeight = v.MaxHeight
					}
					if v.Dial != "" {
						s.Dial = v.Dial
					}
					if v.Cookies != "" {
						s.Cookies = v.Cookies
					}
					sites[k] = s
				}
			}
		}
	}
	// cookie dir fallback: <dir>/<site>.txt
	if dir := os.Getenv("VIDEOCRAWL_COOKIES_DIR"); dir != "" {
		for k, s := range sites {
			if s.Cookies == "" {
				p := dir + "/" + k + ".txt"
				if _, err := os.Stat(p); err == nil {
					s.Cookies = p
					sites[k] = s
				}
			}
		}
	}
	return sites
}

// ProxyURL resolves "auto"/"" against the env override (VIDEOCRAWL_PROXY,
// default http://127.0.0.1:8888 — the smart-proxy on this machine; on lab,
// set it to the reverse-tunnel port).
func ProxyURL(s Site) string {
	if s.Proxy == "" {
		return ""
	}
	if s.Proxy == "auto" {
		p := os.Getenv("VIDEOCRAWL_PROXY")
		if p == "" {
			return "http://127.0.0.1:8888"
		}
		return p
	}
	return s.Proxy
}

// Blocked reports whether a host needs the proxy (GFW set, mirrors the
// smart-proxy's warp list and techcrawl-go's blockedDomains).
func Blocked(host string) bool {
	h := strings.ToLower(host)
	for _, suffix := range []string{
		"youtube.com", "youtu.be", "googlevideo.com", "ytimg.com",
		"github.com", "githubusercontent.com",
		"archive.org", "media.ccc.de", "wikipedia.org",
	} {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return true
		}
	}
	return false
}
