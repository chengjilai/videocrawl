package netx

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Live tests of the media.ccc.de egress (warp-doh). They need the WARP
// socks on 127.0.0.1:40000; skip when absent.
func liveSocks() bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:40000")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func TestResolveViaWARP(t *testing.T) {
	if !liveSocks() {
		t.Skip("no WARP socks")
	}
	ips, err := dohViaWARP(context.Background(), "cdn.media.ccc.de")
	if err != nil {
		t.Fatalf("dohViaWARP: %v", err)
	}
	if len(ips) == 0 {
		t.Fatal("no answers")
	}
	t.Logf("cdn.media.ccc.de -> %v", ips)
}

// TestCCCPathLive fetches the first 4KB of a 37c3 recording through the
// full warp-doh path including the cdn -> ffmuc redirect.
func TestCCCPathLive(t *testing.T) {
	if !liveSocks() {
		t.Skip("no WARP socks")
	}
	if os.Getenv("VC_NET_LIVE") == "" {
		t.Skip("set VC_NET_LIVE=1 to run live network tests")
	}
	c := Client("warp-doh", "", 0)
	req, _ := http.NewRequest("GET",
		"https://cdn.media.ccc.de/congress/2023/h264-hd/37c3-57956-deu-Dicke_Bretter_Die_Congress_Edition_hd.mp4", nil)
	req.Header.Set("Range", "bytes=0-4095")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	t.Logf("status=%d bytes=%d final=%s", resp.StatusCode, len(b), resp.Request.URL)
	if resp.StatusCode != 206 {
		t.Fatalf("want 206, got %d", resp.StatusCode)
	}
}
