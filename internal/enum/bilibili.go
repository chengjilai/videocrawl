// Native bilibili favorites (收藏夹) enumerator.
//
// Decision (research lesson 1): the yt-dlp path already covers spaces, but
// users' favorites lists — x/v3/fav/resource/list — are a major source that
// yt-dlp only reaches via the space page. A native Go enumerator is cheap:
// the API is plain JSON, paginated to the END (no time bias), and the wbi
// signing algorithm + mixin table are already proven in
// ~/src/bilibili/upload_web.py (same table as yt-dlp's BilibiliFavoritesListIE).
// yt-dlp would cost a subprocess + full-metadata pass per favlist; native is
// ~1 request per 20 videos with full metadata (title/duration/uploader/date).
//
// Signing: fetch the wbi img/sub keys from /x/web-interface/nav (cached 24h),
// fall back to the published static pair when the API is unreachable. Requests
// carry a buvid3 device cookie (yt-dlp's format: "<uuid>infoc"); if the site
// config points at a Netscape cookies file (the same bilibili.txt the yt-dlp
// enumerator uses), its session cookies are sent too, which unlocks private
// favlists.
package enum

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"videocrawl/internal/sites"
)

const (
	biliFavPageSize = 20 // x/v3/fav/resource/list caps ps at 20
	// static wbi key pair: shipped in every bilibili page load; used as the
	// fallback when /x/web-interface/nav is unreachable.
	biliWbiImgStatic = "7cd084941338484aae1ad9425b84077c"
	biliWbiSubStatic = "4932caff0ff746eab6f01bf08b70ac45"
	biliUA           = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
)

// biliWbiMixinTab: getMixinKey() from bilibili's vendor js — the same index
// table as yt-dlp and ~/src/bilibili/upload_web.py.
var biliWbiMixinTab = [...]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

type biliWbiCache struct {
	mu sync.Mutex
	mk string
	at time.Time
}

var biliWbi = &biliWbiCache{}

// biliMixinKey returns the 32-char wbi mixin key, cached for 24h (the keys
// rotate roughly daily; a stale key 412s, which surfaces as a clear error).
func biliMixinKey(client *http.Client) string {
	biliWbi.mu.Lock()
	defer biliWbi.mu.Unlock()
	if biliWbi.mk != "" && time.Since(biliWbi.at) < 24*time.Hour {
		return biliWbi.mk
	}
	img, sub := biliWbiImgStatic, biliWbiSubStatic
	if resp, err := client.Get("https://api.bilibili.com/x/web-interface/nav"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var nav struct {
			Data struct {
				WbiImg struct {
					ImgURL string `json:"img_url"`
					SubURL string `json:"sub_url"`
				} `json:"wbi_img"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &nav) == nil {
			if k := wbiKeyOf(nav.Data.WbiImg.ImgURL); k != "" {
				img = k
			}
			if k := wbiKeyOf(nav.Data.WbiImg.SubURL); k != "" {
				sub = k
			}
		}
	}
	all := img + sub
	var b strings.Builder
	for _, i := range biliWbiMixinTab {
		b.WriteByte(all[i])
	}
	biliWbi.mk, biliWbi.at = b.String()[:32], time.Now()
	return biliWbi.mk
}

// wbiKeyOf strips "/bfs/wbi/<key>.png" down to the 32-hex key.
func wbiKeyOf(u string) string {
	i := strings.LastIndex(u, "/")
	if i < 0 {
		return ""
	}
	rest := u[i+1:]
	if j := strings.IndexByte(rest, '.'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// biliWbiInvalidate drops the cached mixin key so the next request re-fetches
// it (stale keys 412 after the ~daily rotation).
func biliWbiInvalidate() {
	biliWbi.mu.Lock()
	defer biliWbi.mu.Unlock()
	biliWbi.mk, biliWbi.at = "", time.Time{}
}

// biliSignWbi appends wts + w_rid to params (sorted query, md5 + mixin key).
func biliSignWbi(params url.Values, mk string) url.Values {
	params.Set("wts", fmt.Sprint(time.Now().Unix()))
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		qs = append(qs, k+"="+url.QueryEscape(params.Get(k)))
	}
	sum := md5.Sum([]byte(strings.Join(qs, "&") + mk))
	params.Set("w_rid", hex.EncodeToString(sum[:]))
	return params
}

// biliBuvid3: device cookie, yt-dlp's format "<uuid>infoc".
func biliBuvid3() string {
	return uuidv4() + "infoc"
}

// biliCookieHeader reads a Netscape cookies.txt (the bilibili.txt the yt-dlp
// enumerator uses, exported by ~/nixos/scripts/export-cookies.py) and returns
// the bilibili-domain cookies as a Cookie header value ("" if none).
func biliCookieHeader(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pairs []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		domain, name, value := f[0], f[5], f[6]
		if domain == "bilibili.com" || strings.HasSuffix(domain, ".bilibili.com") {
			pairs = append(pairs, name+"="+value)
		}
	}
	return strings.Join(pairs, "; ")
}

// biliAPI GET with browser headers (risk control is UA/referer-sensitive).
func biliAPI(client *http.Client, u, cookie string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", biliUA)
	req.Header.Set("Referer", "https://space.bilibili.com/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// BiliFavID extracts the favorites media_id (fid) from a user seed:
// bare digits, space.bilibili.com/<mid>/favlist?fid=<fid>, or
// bilibili.com/medialist/detail/ml<fid>. "" when none found.
func BiliFavID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if allDigits(raw) {
		return raw
	}
	if i := strings.Index(raw, "medialist/detail/ml"); i >= 0 {
		rest := raw[i+len("medialist/detail/ml"):]
		rest = strings.SplitN(rest, "/", 2)[0]
		rest = strings.TrimFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
		if allDigits(rest) {
			return rest
		}
	}
	if u, err := url.Parse(raw); err == nil {
		if v := u.Query().Get("fid"); allDigits(v) {
			return v
		}
	}
	return ""
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// ---- enumeration ----

type biliFavList struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Medias []struct {
			ID       int    `json:"id"` // avid, fallback when bvid missing
			BVID     string `json:"bvid"`
			Title    string `json:"title"`
			Duration int64  `json:"duration"` // seconds for type=2 videos
			Upper    struct {
				Name string `json:"name"`
			} `json:"upper"`
			Pubtime int64 `json:"pubtime"`
			Ctime   int64 `json:"ctime"`
			FavTime int64 `json:"fav_time"`
		} `json:"medias"`
		HasMore bool `json:"has_more"`
	} `json:"data"`
}

func enumBiliFav(srcURL, _ string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	fid := BiliFavID(srcURL)
	if fid == "" {
		return 0, false, fmt.Errorf("bilibili-fav: no media_id in %s", srcURL)
	}
	client := httpClient(cfg)
	mk := biliMixinKey(client)
	cookie := "buvid3=" + biliBuvid3()
	if s := biliCookieHeader(cfg.Cookies); s != "" {
		cookie = s + "; " + cookie
	}
	n := 0
	retried := false // one stale-wbi-key retry per enumeration
	for pn := 1; ; pn++ {
		params := biliSignWbi(url.Values{
			"media_id": {fid},
			"pn":       {fmt.Sprint(pn)},
			"ps":       {fmt.Sprint(biliFavPageSize)},
			"platform": {"web"},
			"jsonp":    {"jsonp"},
		}, mk)
		u := "https://api.bilibili.com/x/v3/fav/resource/list?" + params.Encode()
		b, status, err := biliAPI(client, u, cookie)
		if err != nil {
			return n, false, err
		}
		if status != 200 {
			return n, false, fmt.Errorf("bilibili fav %d", status)
		}
		var l biliFavList
		if err := json.Unmarshal(b, &l); err != nil {
			return n, false, err
		}
		if l.Code != 0 {
			switch l.Code {
			case -403:
				return n, false, fmt.Errorf("bilibili fav: private favorites list (need bilibili session cookies in sites.json/bilibili.txt)")
			case -412:
				// wbi keys rotate ~daily; a stale cached key 412s — refresh once
				// and retry the same page.
				if !retried {
					biliWbiInvalidate()
					mk = biliMixinKey(client)
					retried = true
					pn--
					continue
				}
				return n, false, fmt.Errorf("bilibili fav: risk control (-412) after key refresh — try again later")
			default:
				return n, false, fmt.Errorf("bilibili fav: %s", l.Message)
			}
		}
		if len(l.Data.Medias) == 0 { // empty page: exhausted (has_more=false)
			return n, true, nil
		}
		for _, m := range l.Data.Medias {
			if limit > 0 && n >= limit {
				return n, true, nil
			}
			vid := m.BVID
			if vid == "" {
				vid = fmt.Sprintf("av%d", m.ID)
			}
			entry := Entry{
				VideoID:  vid,
				URL:      "https://www.bilibili.com/video/" + vid,
				Title:    m.Title,
				Duration: m.Duration,
				Channel:  m.Upper.Name,
			}
			if ts := biliPubTime(m.Pubtime, m.Ctime, m.FavTime); ts > 0 {
				entry.Published = time.Unix(ts, 0).UTC().Format("2006-01-02")
			}
			if err := onEntry(entry); err != nil {
				return n, false, err
			}
			n++
		}
		if !l.Data.HasMore {
			return n, true, nil
		}
		time.Sleep(enumPageSleep) // politeness: ~1 req/s to api.bilibili.com
	}
}

// biliPubTime: published date with graceful fallbacks (pubtime, then the
// list's ctime, then when the owner favorited it).
func biliPubTime(pubtime, ctime, favTime int64) int64 {
	if pubtime > 0 {
		return pubtime
	}
	if ctime > 0 {
		return ctime
	}
	return favTime
}
