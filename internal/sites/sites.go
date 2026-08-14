// Package sites: per-site crawl configuration (the same single-registry idea
// as config/sites.json). Defaults are baked in; a JSON file can override.
package sites

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
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
	// AudioFormat: "" = video downloads; otherwise yt-dlp extracts audio
	// (-x --audio-format <fmt> --audio-quality 0). One of mp3|flac|m4a.
	AudioFormat string `json:"audioFormat"`
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
			// Proxy "auto" since 2026-08-14 (f90e10d): media.ccc.de is in the
			// egress warp policy, so the smart-proxy routes it — machine-
			// agnostic (aturing direct, lab via the :18888 tunnel). The
			// warp-doh dial below stays as the no-proxy fallback path.
			Proxy:     "auto",
			Dial:      "warp-doh", // fallback when no proxy is configured
			MaxHeight: 720,
		},
		"archive": {
			Proxy:     "auto",
			MaxHeight: 720,
		},
		"gallica": {
			Proxy:     "auto", // BnF via the smart-proxy (direct policy ok)
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
					if v.AudioFormat != "" {
						s.AudioFormat = v.AudioFormat
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

// --- Egress policy ---------------------------------------------------------
//
// Blocked consults the fleet's single egress policy at /etc/egress-policy.json
// (the same file smart-proxy.py reads; format validated by
// ~/nixos/scripts/check-egress-policy.py):
//
//	{ "default": "warp|direct|doh", "domains": { "<domain>": "warp|direct|doh" } }
//
// Most specific suffix wins (longest match, same as smart-proxy). A host
// whose resolved action is warp (TCP via WARP SOCKS) or doh (DoT resolve)
// needs the proxy; direct does not. The parsed policy and its mtime are
// cached, so hot paths pay one stat per call. When the file is unreadable or
// malformed, Blocked falls back to the baked GFW list below.

const policyPathDefault = "/etc/egress-policy.json"

// bakedBlocked: hosts that need the proxy when the egress policy file is
// unavailable (mirrors the smart-proxy's warp list).
var bakedBlocked = []string{
	"youtube.com", "youtu.be", "googlevideo.com", "ytimg.com",
	"github.com", "githubusercontent.com",
	"archive.org", "media.ccc.de", "wikipedia.org",
}

type egressPolicy struct {
	Default string            `json:"default"`
	Domains map[string]string `json:"domains"`
}

var (
	policyPath        = policyPathDefault // overridable for tests
	policyMu          sync.Mutex
	policyCached      egressPolicy
	policyCachedMtime time.Time
	policyCachedOK    bool
)

// loadPolicy returns the parsed /etc/egress-policy.json (cached by mtime).
// ok=false means the file is unreadable/malformed and callers must use the
// baked list; a stale cache is dropped so the fallback is always current.
func loadPolicy() (egressPolicy, bool) {
	policyMu.Lock()
	defer policyMu.Unlock()
	if fi, err := os.Stat(policyPath); err == nil {
		if policyCachedOK && fi.ModTime().Equal(policyCachedMtime) {
			return policyCached, true
		}
		if b, err := os.ReadFile(policyPath); err == nil {
			var pol egressPolicy
			if json.Unmarshal(b, &pol) == nil {
				if pol.Default == "" {
					pol.Default = "direct"
				}
				if pol.Domains == nil {
					pol.Domains = map[string]string{}
				}
				policyCached, policyCachedMtime, policyCachedOK = pol, fi.ModTime(), true
				return pol, true
			}
		}
	}
	policyCachedOK = false
	return egressPolicy{}, false
}

// resetPolicyCache drops the cached policy (tests).
func resetPolicyCache() {
	policyMu.Lock()
	policyCachedOK = false
	policyMu.Unlock()
}

// policyAction resolves host to its egress action; most specific suffix wins.
func policyAction(host string, pol egressPolicy) string {
	if a, ok := pol.Domains[host]; ok {
		return a
	}
	best := ""
	for d := range pol.Domains {
		if len(d) > len(best) && strings.HasSuffix(host, "."+d) {
			best = d
		}
	}
	if best != "" {
		return pol.Domains[best]
	}
	return pol.Default
}

// PolicyActions returns the effective domain→action map from
// /etc/egress-policy.json, for debugging/CLI. When the policy file is
// unreadable, the baked fallback list is reported with action "warp"
// (consistent with Blocked's fallback semantics).
func PolicyActions() map[string]string {
	if pol, ok := loadPolicy(); ok {
		m := make(map[string]string, len(pol.Domains))
		for d, a := range pol.Domains {
			m[d] = a
		}
		return m
	}
	m := make(map[string]string, len(bakedBlocked))
	for _, d := range bakedBlocked {
		m[d] = "warp"
	}
	return m
}

// Blocked reports whether a host needs the proxy: its egress action (per
// /etc/egress-policy.json, most specific suffix wins) is warp or doh. Falls
// back to the baked GFW list when the policy file is unreadable.
func Blocked(host string) bool {
	h := strings.ToLower(host)
	if pol, ok := loadPolicy(); ok {
		a := policyAction(h, pol)
		return a == "warp" || a == "doh"
	}
	for _, suffix := range bakedBlocked {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return true
		}
	}
	return false
}
