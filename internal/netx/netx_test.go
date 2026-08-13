package netx

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestCCCPathLive(t *testing.T) {
	ip, err := resolveDoh(context.Background(), "cdn.media.ccc.de")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	t.Logf("cdn.media.ccc.de -> %s", ip)
	c := Client("warp-doh", "")
	req, _ := http.NewRequest("GET",
		"https://cdn.media.ccc.de/congress/2023/h264-hd/37c3-57956-deu-Dicke_Bretter_Die_Congress_Edition_hd.mp4", nil)
	req.Header.Set("Range", "bytes=0-4095")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	t.Logf("status=%d bytes=%d final=%s took=%s", resp.StatusCode, len(b), resp.Request.URL, time.Since(start))
	if resp.StatusCode != 206 {
		t.Fatalf("want 206, got %d", resp.StatusCode)
	}
}
