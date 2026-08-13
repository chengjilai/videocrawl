// Health checks for unattended operation: egress transports (WARP socks +
// smart-proxy) and disk headroom on the output filesystem. The crawl-loop
// runs these at the start of every round so a dead tunnel or full disk
// doesn't burn a round on doomed work.
package app

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// warpSocksDefault matches internal/netx's warpSocksAddr (the WARP
	// SOCKS5 endpoint used for media.ccc.de DoH+dial).
	warpSocksDefault = "127.0.0.1:40000"
	// proxyDefault matches sites.ProxyURL's fallback (the smart-proxy).
	proxyDefault     = "http://127.0.0.1:8888"
	defaultMinFreeGB = 2
)

// checkTransports probes the egress paths the crawler depends on:
//
//   - WARP socks (127.0.0.1:40000): media.ccc.de enumeration + native
//     downloads (the warp-doh dial in internal/netx).
//   - the smart-proxy (VIDEOCRAWL_PROXY, default http://127.0.0.1:8888; on
//     lab the reverse-tunnel port 18888): youtube/archive.org/RSS routes.
//
// Both are plain TCP probes — the common unattended failure modes (ssh
// tunnel died, WARP daemon restarted) show up as a refused/unreachable
// connect. Escape hatches for proxy-less deployments:
//   - VIDEOCRAWL_WARP_SOCKS=off  disables the WARP socks probe
//   - VIDEOCRAWL_NO_PROXY_CHECK=1 disables the smart-proxy probe
func checkTransports() error {
	var down []string
	if socks := envOr("VIDEOCRAWL_WARP_SOCKS", warpSocksDefault); socks != "" && socks != "off" {
		if !tcpUp(socks) {
			down = append(down, "WARP socks "+socks)
		}
	}
	if os.Getenv("VIDEOCRAWL_NO_PROXY_CHECK") == "" {
		proxy := os.Getenv("VIDEOCRAWL_PROXY")
		if proxy == "" {
			proxy = proxyDefault
		}
		if p := proxyHostPort(proxy); p != "" && !tcpUp(p) {
			down = append(down, "smart-proxy "+proxy)
		}
	}
	if len(down) > 0 {
		return fmt.Errorf("egress down: %s", strings.Join(down, ", "))
	}
	return nil
}

// proxyHostPort strips the scheme (http://) off a proxy URL for dialing.
func proxyHostPort(raw string) string {
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// tcpUp reports whether addr accepts a TCP connection within 3s.
func tcpUp(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// checkDiskFree verifies the filesystem holding outDir has at least
// VIDEOCRAWL_MIN_FREE_GB (default 2) GB free. Called before every download
// pass; the loop pauses the pass (logs, retries next round) while low.
func checkDiskFree(outDir string) error {
	free, err := diskFreeBytes(outDir)
	if err != nil {
		return fmt.Errorf("statfs %s: %w", outDir, err)
	}
	min := minFreeBytes()
	if free < min {
		return fmt.Errorf("only %.1f GB free on %s (need >= %d GB)",
			float64(free)/(1<<30), outDir, min>>30)
	}
	return nil
}

func minFreeBytes() int64 {
	gb := envOr("VIDEOCRAWL_MIN_FREE_GB", "2")
	f, err := strconv.ParseFloat(gb, 64)
	if err != nil || f <= 0 {
		return int64(defaultMinFreeGB) << 30
	}
	return int64(f * (1 << 30))
}
