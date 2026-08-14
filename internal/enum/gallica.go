// Gallica (BnF) support for the 'gallica' source kind: each source is one
// ark (a PDF score at https://gallica.bnf.fr/ark:/12148/<ark>). Enumeration
// maps the ark to a single entry (title via SRU when reachable); downloads
// fetch the PDF through the altcha proof-of-work + session-cookie flow
// (ported from musget/pkg/gallica): solve SHA-256(salt+counter)==challenge
// once per session, keep the verified cookie, then fetch the PDF.
//
// Reconciliation with musget/pkg/gallica (kept as a port, not imported):
// musget's package is a strict subset of this file — wiring it in would
// regress batch downloads:
//   - musget Download buffers the whole PDF in memory via
//     io.ReadAll(io.LimitReader(body, 512<<20)): scores >512 MiB are
//     silently truncated yet pass the %PDF header check and would be
//     recorded as downloaded. This port streams to dest (getTo) and
//     verifies the file on disk.
//   - musget's client has a 300 s total Timeout; this port uses
//     netx.Client(dial, proxy, 0) — no total timeout (big PDFs over the
//     smart proxy can exceed 300 s).
//   - musget returns on the first non-200/429 response (e.g. Gallica's
//     403 "Access Interdit") without attempting the altcha solve; this
//     port treats any failed or HTML response from an unverified session
//     as the challenge trigger.
//   - musget has no exported ark extractor (GallicaArk, used below) and
//     no netx dial-mode transport.
//
// To wire musget later: first upstream the streamed + no-total-timeout +
// error-path-altcha behavior into musget/pkg/gallica and export an Ark
// helper, then replace this file with a thin wrapper. The mechanics are
// verified: go.mod 'require musget v0.0.0' + 'replace musget => ../musget',
// 'go mod vendor' vendors only musget/pkg/gallica (stdlib-only; no utls
// deps, go.sum unchanged), and -mod=vendor builds/tests pass.
package enum

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"videocrawl/internal/netx"
	"videocrawl/internal/sites"
)

const gallicaBase = "https://gallica.bnf.fr"

// Gallica holds one session (cookie jar) shared across requests; the altcha
// verified cookie is kept here so batch downloads solve the PoW once.
type Gallica struct {
	HTTP     *http.Client
	MaxTries int
	verified bool
}

// NewGallica builds a session client for a site config: transport per the
// site's dial mode (proxy for the smart-proxy default) plus a cookie jar
// for the altcha verification cookie.
func NewGallica(cfg sites.Site) *Gallica {
	dial := cfg.Dial
	if dial == "" && sites.ProxyURL(cfg) != "" {
		dial = "proxy"
	}
	jar, _ := cookiejar.New(nil)
	c := netx.Client(dial, sites.ProxyURL(cfg), 0) // no total timeout: big PDFs
	c.Jar = jar
	return &Gallica{HTTP: c, MaxTries: 8}
}

// Hit is one SRU search result.
type Hit struct {
	Ark   string
	Title string
	Type  string
	Pages string
}

var (
	reGallicaRecord = regexp.MustCompile(`(?s)<srw:record>(.*?)</srw:record>`)
	reGallicaField  = regexp.MustCompile(`(?s)<(?:dc|dcx):([a-zA-Z]+)[^>]*>(.*?)</(?:dc|dcx):[a-zA-Z]+>`)
	reGallicaArk    = regexp.MustCompile(`ark:/12148/([a-z0-9]+)`)
)

// GallicaArk extracts the ark id from a URL ("https://gallica.bnf.fr/
// ark:/12148/btv1b52503827w"), a partial ("ark:/12148/...") or a bare id.
func GallicaArk(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := reGallicaArk.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	if raw != "" {
		ok := true
		for _, r := range raw {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				ok = false
				break
			}
		}
		if ok {
			return raw
		}
	}
	return ""
}

// Search queries the SRU endpoint (CQL over dc fields).
func (c *Gallica) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	q.Set("operation", "searchRetrieve")
	q.Set("version", "1.2")
	q.Set("query", query+` and not dc.type any "sound" and not dc.type any "video"`)
	q.Set("maximumRecords", fmt.Sprint(limit))
	body, err := c.getBytes(ctx, gallicaBase+"/SRU?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, rec := range reGallicaRecord.FindAllString(string(body), -1) {
		fields := map[string]string{}
		for _, m := range reGallicaField.FindAllStringSubmatch(rec, -1) {
			fields[m[1]] = strings.TrimSpace(stripGallicaTags(m[2]))
		}
		ark := ""
		if m := reGallicaArk.FindStringSubmatch(fields["identifier"]); m != nil {
			ark = m[1]
		} else if m := reGallicaArk.FindStringSubmatch(rec); m != nil {
			ark = m[1]
		}
		if ark == "" {
			continue
		}
		hits = append(hits, Hit{Ark: ark, Title: fields["title"], Type: fields["type"], Pages: fields["format"]})
	}
	return hits, nil
}

func stripGallicaTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Download fetches the full PDF for an ark into dest (streamed, not
// buffered), solving altcha on the first non-PDF response and retrying with
// the verified session cookie. Gallica gates the PDF behind its altcha
// anti-bot challenge: an unverified session gets 403 "Access Interdit", a
// 429, or a 200 HTML challenge page instead of the PDF — any of those
// triggers the PoW solve (once per session, cookie reused).
//
// Current BnF backend: the altcha/verify POST itself 302-redirects to the
// PDF (set-cookie altcha_pass, single-use) — the client follows it and the
// verify response body IS the PDF, while a subsequent GET of the PDF URL
// 403s. solveAltcha therefore captures the PDF from the verify response
// when present; the old flow (verify returns an empty ok, PDF fetched on
// the next GET) is kept as fallback.
func (c *Gallica) Download(ctx context.Context, ark, dest string) error {
	u := fmt.Sprintf("%s/ark:/12148/%s.pdf", gallicaBase, ark)
	// Probe once: a verified session streams the PDF directly; an
	// unverified one is gated (302 altcha redirect, 403, 429, or an HTML
	// challenge page). Retrying the probe is useless — the gate persists
	// until the PoW is solved — and BnF's rate limit can hold for
	// minutes, so a single attempt keeps the pre-altcha stall ~1s instead
	// of the full retry ladder.
	if err := c.getToN(ctx, u, dest, 1); err == nil && FileIsPDF(dest) {
		return nil
	}
	got := false
	if !c.verified {
		var err error
		if got, err = c.solveAltcha(ctx, u, dest); err != nil {
			return fmt.Errorf("altcha: %w", err)
		}
		c.verified = true
	}
	if !got {
		if err := c.getToN(ctx, u, dest, c.MaxTries); err != nil {
			os.Remove(dest + ".part")
			return err
		}
	}
	if !FileIsPDF(dest) || fileSize0(dest) == 0 {
		os.Remove(dest + ".part")
		return fmt.Errorf("still not a valid PDF after altcha")
	}
	return nil
}

func fileSize0(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// FileIsPDF reports whether p starts with the %PDF magic (and is nonempty).
func FileIsPDF(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 4)
	n, _ := io.ReadFull(f, b)
	return n == 4 && string(b) == "%PDF"
}

// getBytes fetches a small response body (SRU XML, altcha challenge JSON).
func (c *Gallica) getBytes(ctx context.Context, u string, headers map[string]string) ([]byte, error) {
	return c.getBytesN(ctx, u, headers, c.MaxTries)
}

func (c *Gallica) getBytesN(ctx context.Context, u string, headers map[string]string, tries int) ([]byte, error) {
	var lastErr error
	for try := 0; try < tries; try++ {
		if err := gallicaWait(ctx, try); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			lastErr = fmt.Errorf("%d %s (Gallica rate limit — retrying)", resp.StatusCode, resp.Status)
			continue // backoff
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// getToN streams a (possibly large) response body to dest, with the same
// retry/backoff/politeness policy as getBytes. dest is truncated on each
// attempt so a failed attempt never leaves a half-old file behind.
func (c *Gallica) getToN(ctx context.Context, u, dest string, tries int) error {
	var lastErr error
	for try := 0; try < tries; try++ {
		if err := gallicaWait(ctx, try); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			lastErr = fmt.Errorf("%d %s (Gallica rate limit — retrying)", resp.StatusCode, resp.Status)
			continue // backoff
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return fmt.Errorf("gallica HTTP %d", resp.StatusCode)
		}
		// Write to dest+".part"; rename only after a COMPLETE copy, so a
		// mid-stream death can never leave a partial PDF at the final
		// path (which processGallica would otherwise record as done).
		part := dest + ".part"
		f, ferr := os.Create(part)
		if ferr != nil {
			resp.Body.Close()
			return ferr
		}
		_, cerr := io.Copy(f, resp.Body)
		resp.Body.Close()
		ferr = f.Close()
		if cerr != nil {
			os.Remove(part)
			lastErr = cerr
			continue
		}
		if ferr != nil {
			os.Remove(part)
			lastErr = ferr
			continue
		}
		if !FileIsPDF(part) {
			os.Remove(part)
			lastErr = fmt.Errorf("gallica: response is not a PDF")
			continue
		}
		if err := os.Rename(part, dest); err != nil {
			os.Remove(part)
			return err
		}
		return nil
	}
	return lastErr
}

// gallicaWait: polite min-interval throttle before the first attempt (the
// API rate-limits aggressively) and exponential backoff after failures.
func gallicaWait(ctx context.Context, try int) error {
	delay := 1200 * time.Millisecond
	if try > 0 {
		delay = 3 * time.Second
		for i := 0; i < try; i++ {
			delay *= 2
		}
		// cap at 30s: an uncapped ladder burns ~9min per 8-try call on a
		// persistent rate limit, and the probes it is attached to are
		// single-shot anyway; the cap bounds a post-verify transfer stall
		// while staying polite to BnF.
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// solveAltcha performs the full PoW flow and keeps the session cookie. If
// pdfDest is non-empty and BnF answers the verify POST with the PDF itself
// (current backend: 302 to the PDF with a single-use altcha_pass cookie,
// which the client follows), the PDF is streamed to pdfDest and gotPDF=true
// is returned so Download can skip the (now 403ing) refetch.
func (c *Gallica) solveAltcha(ctx context.Context, referer, pdfDest string) (bool, error) {
	// seed a session (single attempt: a gate response still sets the
	// session cookie; retrying a rate-limited seed just keeps BnF's
	// limiter hot)
	_, _ = c.getBytesN(ctx, referer, map[string]string{"Referer": gallicaBase + "/"}, 1)
	// fetch challenge
	body, err := c.getBytes(ctx, gallicaBase+"/services/engine/search/altcha/challenge", map[string]string{"Referer": referer})
	if err != nil {
		return false, err
	}
	var ch struct {
		Algorithm string `json:"algorithm"`
		Challenge string `json:"challenge"`
		Maxnumber int    `json:"maxnumber"`
		Salt      string `json:"salt"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(body, &ch); err != nil {
		return false, fmt.Errorf("challenge json: %w", err)
	}
	if ch.Algorithm != "SHA-256" {
		return false, fmt.Errorf("unsupported altcha algorithm %q", ch.Algorithm)
	}
	maxN := ch.Maxnumber
	if maxN <= 0 {
		maxN = 100000
	}
	sol := -1
	start := time.Now()
	for n := 0; n <= maxN; n++ {
		sum := sha256.Sum256([]byte(ch.Salt + fmt.Sprint(n)))
		if hex.EncodeToString(sum[:]) == ch.Challenge {
			sol = n
			break
		}
		if n%50000 == 0 && time.Since(start) > 20*time.Second {
			return false, fmt.Errorf("altcha solve timed out at %d", n)
		}
	}
	if sol < 0 {
		return false, fmt.Errorf("altcha no solution in [0,%d]", maxN)
	}
	payload, _ := json.Marshal(map[string]any{
		"algorithm": ch.Algorithm,
		"challenge": ch.Challenge,
		"number":    sol,
		"salt":      ch.Salt,
		"signature": ch.Signature,
	})
	b64 := base64.StdEncoding.EncodeToString(payload)
	form := url.Values{"altchaPayload": {b64}}.Encode()
	var lastErr error
	for try := 0; try < c.MaxTries; try++ {
		if err := gallicaWait(ctx, try); err != nil {
			return false, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			gallicaBase+"/services/engine/search/altcha/verify", strings.NewReader(form))
		if err != nil {
			return false, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", referer)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusFound {
			got, cerr := capturePDF(resp, pdfDest)
			if cerr != nil {
				lastErr = cerr
				continue
			}
			return got, nil
		}
		// 429, 403 and 5xx are transient rate-limit/WAF errors: retry with
		// backoff (BnF's WAF escalates to 403 before hard-dropping an IP;
		// the block clears on its own). Other statuses are permanent.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("altcha verify HTTP %d", resp.StatusCode)
			continue
		}
		resp.Body.Close()
		return false, fmt.Errorf("altcha verify HTTP %d", resp.StatusCode)
	}
	return false, lastErr
}

// capturePDF inspects a 200/302 altcha-verify response: when its body is a
// PDF (current BnF backend redirects the verify POST to the PDF itself), it
// is streamed to dest+.".part" and atomically renamed; got=true is
// returned. An empty/non-PDF body (old backend: "verified, go fetch") is
// consumed and returns got=false so the caller falls back to the PDF GET.
func capturePDF(resp *http.Response, dest string) (bool, error) {
	defer resp.Body.Close()
	if dest == "" {
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	br := bufio.NewReader(resp.Body)
	magic, err := br.Peek(4)
	if err != nil || string(magic) != "%PDF" {
		io.Copy(io.Discard, br)
		return false, nil
	}
	part := dest + ".part"
	f, ferr := os.Create(part)
	if ferr != nil {
		return false, ferr
	}
	_, cerr := io.Copy(f, br)
	ferr = f.Close()
	if cerr != nil || ferr != nil {
		os.Remove(part)
		if cerr != nil {
			return false, cerr
		}
		return false, ferr
	}
	if !FileIsPDF(part) || fileSize0(part) == 0 {
		os.Remove(part)
		return false, fmt.Errorf("gallica: captured verify body is not a valid PDF")
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return false, err
	}
	return true, nil
}

// enumGallica: a gallica source is one ark; enumeration emits its single
// entry, with the title looked up via SRU (best-effort: offline SRU still
// yields a row keyed on the ark).
func enumGallica(srcURL, _ string, cfg sites.Site, limit int, onEntry func(Entry) error) (int, bool, error) {
	ark := GallicaArk(srcURL)
	if ark == "" {
		return 0, false, fmt.Errorf("gallica: no ark in %q", srcURL)
	}
	title := ark
	cl := NewGallica(cfg)
	if hits, err := cl.Search(context.Background(), `dc.identifier any "ark:/12148/`+ark+`"`, 5); err == nil && len(hits) > 0 && hits[0].Title != "" {
		title = hits[0].Title
	}
	entry := Entry{
		VideoID: ark,
		URL:     gallicaBase + "/ark:/12148/" + ark,
		Title:   title,
		Channel: "gallica.bnf.fr",
	}
	if err := onEntry(entry); err != nil {
		return 0, false, err
	}
	return 1, true, nil
}
