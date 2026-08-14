package dl

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"videocrawl/internal/netx"
)

// Live verification of the parallel range-fetch stripes against a real
// media.ccc.de file through warp-doh. Skips unless VC_NET_LIVE=1 and the
// WARP socks are up. Small file by default (datenspuren 2023 "Opening",
// 13MiB sd) — set VC_TEST_URL to target something else.
func TestFetchStripedLive(t *testing.T) {
	if os.Getenv("VC_NET_LIVE") == "" {
		t.Skip("set VC_NET_LIVE=1 for live tests")
	}
	url := os.Getenv("VC_TEST_URL")
	if url == "" {
		url = "https://cdn.media.ccc.de/events/datenspuren/2023/h264-sd/ds2023-277-deu-Opening_sd.mp4"
	}
	client := netx.Client("warp-doh", "", 0)
	total, ok, err := probeRange(client, url)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !ok {
		t.Skipf("server lacks range support")
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "test.part")
	dst := filepath.Join(dir, "test.mp4")
	start := time.Now()
	if err := fetchStriped(client, url, tmp, dst); err != nil {
		t.Fatalf("fetchStriped: %v", err)
	}
	el := time.Since(start)
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != total {
		t.Fatalf("size: got %d want %d", fi.Size(), total)
	}
	rate := float64(fi.Size()) / el.Seconds()
	t.Logf("striped fetch: %d bytes in %s = %.0f KB/s (%.1f MiB/s), sha256=%x",
		fi.Size(), el, rate/1024, rate/1024/1024, sha256File(t, dst))

	// correctness: 64KB windows at 0 / 50% / near-EOF must byte-match
	// independent Range GETs through the same transport
	const win = 64 << 10
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, off := range []int64{0, total / 2, total - win} {
		if off < 0 {
			off = 0
		}
		got := make([]byte, win)
		n, _ := f.ReadAt(got, off)
		want := rangeGet(client, url, off, off+int64(n))
		if !bytes.Equal(got[:n], want) {
			t.Fatalf("window at offset %d differs from server", off)
		}
	}
	t.Log("byte windows at 0/50%/EOF match independent Range GETs")
}

// TestSingleStreamCompareLive: same file via the legacy single-stream
// resume fetch, to demonstrate the stripe win. Skips unless VC_NET_LIVE=1.
func TestSingleStreamCompareLive(t *testing.T) {
	if os.Getenv("VC_NET_LIVE") == "" {
		t.Skip("set VC_NET_LIVE=1 for live tests")
	}
	url := os.Getenv("VC_TEST_URL")
	if url == "" {
		url = "https://cdn.media.ccc.de/events/datenspuren/2023/h264-sd/ds2023-277-deu-Opening_sd.mp4"
	}
	client := netx.Client("warp-doh", "", 0)
	total, _, err := probeRange(client, url)
	if err != nil || total <= 0 {
		t.Skipf("probe failed: %v", err)
	}
	dir := t.TempDir()
	tmp := filepath.Join(dir, "test.part")
	dst := filepath.Join(dir, "test.mp4")
	start := time.Now()
	if err := FetchResume(client, url, tmp, dst); err != nil {
		t.Fatalf("fetchResume: %v", err)
	}
	el := time.Since(start)
	fi, _ := os.Stat(dst)
	rate := float64(fi.Size()) / el.Seconds()
	t.Logf("single-stream fetch: %d bytes in %s = %.0f KB/s (%.1f MiB/s)",
		fi.Size(), el, rate/1024, rate/1024/1024)
}

func sha256File(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return h.Sum(nil)
}

func rangeGet(client *http.Client, url string, from, to int64) []byte {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "videocrawl/1.0")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to-1))
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}
