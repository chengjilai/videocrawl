// Package netx: transport builders for the REST sources.
//
// Dial modes:
//   - ""        plain HTTPS (direct).
//   - "proxy"   through the smart-proxy (VIDEOCRAWL_PROXY; the verified
//               WARP-routed path for youtube/archive.org etc.).
//   - "warp-doh" DoH-resolve the host (Tencent doh.pub — CN-reachable, real
//               IPs where system DNS is poisoned) then connect via the WARP
//               SOCKS5 proxy on 127.0.0.1:40000. Needed for media.ccc.de:
//               its DNS is poisoned locally and the smart-proxy's policy does
//               not cover it (verified: WARP can reach ccc's real IP).
package netx

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	warpSocksAddr = "127.0.0.1:40000"
	// DoH resolution goes through the WARP tunnel to Google (8.8.8.8):
	// CN-resolvers return poisoned answers for media.ccc.de hosts
	// (doh.pub gave an archive.org IP for cdn.media.ccc.de), and WARP egress
	// reaches the real IPs. NOT 1.1.1.1: the WARP tunnel intercepts 1.1.1.1
	// and its DNS returned a different, unreachable answer (96.44.x) —
	// verified: dns.google via 8.8.8.8 gives the reachable 212.201.68.132.
	cloudflareDoH  = "8.8.8.8:443"
	cloudflareSNI  = "dns.google"
	dohURLFallback = "https://doh.pub/dns-query"
)

// Client builds an http.Client for a site with the given dial mode and
// proxy URL (used when mode is "proxy").
// Client builds an http.Client. timeout=0 disables the total request
// timeout (needed for large file downloads; dial/TLS keep their own
// timeouts via the transport).
func Client(dial, proxyURL string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     60 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
	}
	switch dial {
	case "warp-doh":
		tr.DialContext = warpDohDial
	case "proxy":
		if p, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(p)
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ---- warp-doh dial ----

var dnsCache sync.Map // host -> []string ips

func resolveDoh(ctx context.Context, host string) ([]string, error) {
	if v, ok := dnsCache.Load(host); ok {
		ips := v.([]string)
		if len(ips) > 0 {
			return ips, nil
		}
	}
	// Cloudflare's answer for CDN hosts varies per query/node (cdn.media.ccc.de
	// returned an unreachable IP in one query, the reachable one in another).
	// Union several queries and cache the set.
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		ips, err := dohViaWARP(ctx, host)
		if err != nil {
			ips, err = dohPlain(ctx, host)
		}
		if err == nil {
			for _, ip := range ips {
				seen[ip] = true
			}
			if len(seen) >= 2 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	var out []string
	for ip := range seen {
		out = append(out, ip)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("doh: no answers for %s", host)
	}
	dnsCache.Store(host, out)
	return out, nil
}

// dohViaWARP queries Cloudflare DoH through the WARP socks to 1.1.1.1
// (fixed IP, no recursion into resolveDoh).
func dohViaWARP(ctx context.Context, host string) ([]string, error) {
	// Dial the WARP socks to the fixed DoH IP; the transport performs the
	// TLS handshake with the matching SNI (dns.google). Each dial opens a
	// fresh socks connection — stateless, safe for transport reuse.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return warpSocksConnect(ctx, cloudflareDoH)
		},
		TLSClientConfig: &tls.Config{ServerName: cloudflareSNI},
	}
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://"+cloudflareSNI+"/resolve?name="+url.QueryEscape(host)+"&type=A", nil)
	req.Header.Set("Accept", "application/dns-json")
	resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("warp-doh %d", resp.StatusCode)
	}
	return parseDohAnswers(resp.Body)
}

// dohPlain queries a CN-reachable DoH directly (fallback).
func dohPlain(ctx context.Context, host string) ([]string, error) {
	u := fmt.Sprintf("%s?name=%s&type=A", dohURLFallback, url.QueryEscape(host))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	req.Header.Set("User-Agent", "videocrawl/1.0")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("doh %d", resp.StatusCode)
	}
	return parseDohAnswers(resp.Body)
}

func parseDohAnswers(r io.Reader) ([]string, error) {
	var d struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, err
	}
	var ips []string
	for _, a := range d.Answer {
		if a.Type == 1 && strings.Contains(a.Data, ".") {
			ips = append(ips, a.Data)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no A records")
	}
	return ips, nil
}

// warpDohDial resolves via DoH and tries each A record until one connects
// (CDN hosts return varying answers; some IPs are unreachable from WARP
// egress while others work — verified for cdn.media.ccc.de).
func warpDohDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// A cached set can be entirely poisoned (CN DoH fallback answers when the
	// WARP DoH path was briefly down — verified for ffmuc.media.ccc.de: got
	// 199.16.158.104 instead of 46.226.127.231). On total dial failure, evict
	// and re-resolve so a recovered WARP path gets another chance.
	for attempt := 0; attempt < 3; attempt++ {
		ips, err := resolveDoh(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := warpSocksConnect(ctx, net.JoinHostPort(ip, port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		dnsCache.Delete(host)
		if lastErr == nil {
			lastErr = fmt.Errorf("no A records for %s", host)
		}
		if attempt == 2 {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("dial %s: exhausted retries", host)
}

func warpSocksConnect(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", warpSocksAddr)
	if err != nil {
		return nil, err
	}
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portStr)
	ip := net.ParseIP(host)
	if ip == nil {
		conn.Close()
		return nil, fmt.Errorf("warp socks needs an IP, got %q", host)
	}
	ipb := ip.To4()
	if err := socksHandshake(conn, 1, ipb, port); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func socksHandshake(conn net.Conn, atyp byte, addr []byte, port int) error {
	buf := []byte{0x05, 0x01, 0x00}
	if _, err := conn.Write(buf); err != nil {
		return err
	}
	rb := make([]byte, 2)
	if _, err := io.ReadFull(conn, rb); err != nil {
		return err
	}
	if rb[1] != 0x00 {
		return fmt.Errorf("socks auth: %x", rb[1])
	}
	req := append([]byte{0x05, 0x01, 0x00, atyp}, addr...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}
	// reply: VER REP RSV ATYP ... (variable length)
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return err
	}
	if hdr[1] != 0x00 {
		return fmt.Errorf("socks connect: status %d", hdr[1])
	}
	var rest []byte
	switch hdr[3] {
	case 1:
		rest = make([]byte, 4+2)
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return err
		}
		rest = make([]byte, int(l[0])+2)
	case 4:
		rest = make([]byte, 16+2)
	}
	if len(rest) > 0 {
		if _, err := io.ReadFull(conn, rest); err != nil {
			return err
		}
	}
	return nil
}
