// Package dl: download workers. Each worker takes one video at a time,
// fetches metadata (policy applied: duration bounds, live skip), runs
// yt-dlp with the per-site recipe, then hashes the finished file and
// records it. Oldest-first order comes from the store query.
package dl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"videocrawl/internal/netx"

	"videocrawl/internal/model"
	"videocrawl/internal/sites"
	"videocrawl/internal/store"
	"videocrawl/internal/yt"
)

// Policy: selection rules applied to metadata before downloading.
type Policy struct {
	MinDuration int64 // seconds; 0 = no minimum
	MaxDuration int64 // seconds; 0 = no maximum
	SkipShorts  bool
	SkipLive    bool
}

// Pool runs W workers.
type Pool struct {
	store *store.Store
	sites map[string]sites.Site
	outDir string
	policy Policy
	workers int
	mu   sync.Mutex
	done int
	fail int
	skip int
}

func NewPool(st *store.Store, sites map[string]sites.Site, outDir string, policy Policy, workers int) *Pool {
	return &Pool{store: st, sites: sites, outDir: outDir, policy: policy, workers: workers}
}

func (p *Pool) Counts() (done, fail, skip int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done, p.fail, p.skip
}

// Run processes videos until the queue is empty or limit is reached.
func (p *Pool) Run(limit int) error {
	if limit <= 0 {
		limit = 1 << 30
	}
	// table scan batches: we can't stream directly with modernc sqlite
	// (single conn), so pull small batches and track done via status.
	totalDone := 0
	for totalDone < limit {
		batch, err := p.store.NextForDownload(p.workers * 2)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, p.workers)
		for _, v := range batch {
			if totalDone >= limit {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(v model.Video) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := p.process(v); err != nil {
					if errors.Is(err, errSkipped) {
						p.mu.Lock()
						p.skip++
						p.mu.Unlock()
					} else {
						p.mu.Lock()
						p.fail++
						p.mu.Unlock()
						fmt.Fprintf(os.Stderr, "FAIL %s (%s): %v\n", v.VideoID, v.URL, err)
					}
				} else {
					p.mu.Lock()
					p.done++
					p.mu.Unlock()
				}
			}(v)
		}
		wg.Wait()
		totalDone += len(batch)
		if len(batch) < p.workers*2 {
			return nil
		}
	}
	return nil
}

var errSkipped = errors.New("skipped")

func (p *Pool) process(v model.Video) error {
	src, err := p.store.GetSource(v.SourceID)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	cfg := p.sites[src.Site]
	proxy := sites.ProxyURL(cfg)

	// native-download sites (ccc: static files via DoH+WARP, no extractor)
	if cfg.Dial == "warp-doh" {
		return p.processNative(v, src, cfg)
	}

	// 1. metadata + policy
	meta, err := yt.GetMeta(cfg.EnumArgs, cfg.Cookies, proxy, v.URL)
	if err == nil {
		p.store.UpdateMeta(v.SourceID, v.VideoID, meta.Title, yt.DurationSeconds(meta.Duration),
			yt.ParseUploadDate(meta.UploadDate), meta.Channel)
	}
	if err != nil {
		if strings.Contains(err.Error(), "is not a valid URL") || strings.Contains(err.Error(), "Unsupported URL") {
			p.store.MarkSkipped(v.SourceID, v.VideoID, "unsupported url: "+err.Error())
			return errSkipped
		}
		p.store.MarkFailed(v.SourceID, v.VideoID, err.Error())
		return err
	}
	dur := yt.DurationSeconds(meta.Duration)
	if p.policy.MinDuration > 0 && dur > 0 && dur < p.policy.MinDuration {
		p.store.MarkSkipped(v.SourceID, v.VideoID, fmt.Sprintf("too short: %ds", dur))
		return errSkipped
	}
	if p.policy.MaxDuration > 0 && dur > p.policy.MaxDuration {
		p.store.MarkSkipped(v.SourceID, v.VideoID, fmt.Sprintf("too long: %ds", dur))
		return errSkipped
	}
	if p.policy.SkipLive && meta.LiveStatus != "" && meta.LiveStatus != "not_live" {
		p.store.MarkSkipped(v.SourceID, v.VideoID, "live: "+meta.LiveStatus)
		return errSkipped
	}

	// 2. download
	cmd := yt.DownloadCmd(cfg.Cookies, proxy, p.outDir, cfg.MaxHeight, cfg.DLArgs, v.URL)
	if err := yt.RunDownload(cmd, nil); err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, err.Error())
		return err
	}

	// 3. locate the finished file: the newest .mp4/.mkv/.webm/.flv under
	// <outDir>/<channel>/ matching the id prefix.
	path, size, err := findOutput(p.outDir, v.VideoID)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "output locate: "+err.Error())
		return fmt.Errorf("output locate: %w", err)
	}
	hash, err := fileSHA256(path)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "hash: "+err.Error())
		return fmt.Errorf("hash: %w", err)
	}
	title := meta.Title
	if title == "" {
		title = v.Title
	}
	upd := model.Video{
		SourceID:  v.SourceID,
		VideoID:   v.VideoID,
		Title:     title,
		Duration:  dur,
		Published: yt.ParseUploadDate(meta.UploadDate),
		Channel:   meta.Channel,
		SizeBytes: size,
		Path:      path,
		SHA256:    hash,
	}
	return p.store.MarkDownloaded(upd)
}

// findOutput finds the file whose name starts with the video id (yt-dlp
// output template is %(channel)s/%(id)s_%(title).100B.%(ext)s, so the id
// prefix is unique).
func findOutput(outDir, videoID string) (string, int64, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", 0, err
	}
	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(outDir, dir.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			if f.IsDir() {
				continue
			}
			base := f.Name()
			if strings.HasPrefix(base, videoID+"_") && !strings.HasSuffix(base, ".part") {
				info, err := f.Info()
				if err != nil {
					continue
				}
				return filepath.Join(outDir, dir.Name(), base), info.Size(), nil
			}
		}
	}
	return "", 0, fmt.Errorf("no output file for %s", videoID)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// processNative: static-file download (media.ccc.de): pick the best video
// recording + en subtitles recorded at enumeration time, fetch with Range
// resume through the warp-doh transport, .part -> atomic rename, sha256.
func (p *Pool) processNative(v model.Video, src model.Source, cfg sites.Site) error {
	files, err := p.store.GetFiles(v.SourceID, v.VideoID)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	if len(files) == 0 && src.Site == "ccc" {
		// list endpoint omits recordings: fetch the event detail
		files, err = cccEventFiles(v.URL, cfg.MaxHeight)
		if err != nil {
			p.store.MarkFailed(v.SourceID, v.VideoID, "ccc detail: "+err.Error())
			return fmt.Errorf("ccc detail: %w", err)
		}
	}
	var videoFile, subFile *model.File
	for i := range files {
		f := &files[i]
		if f.Kind == "video" && videoFile == nil {
			videoFile = f
		}
		if f.Kind == "sub" && subFile == nil {
			subFile = f
		}
	}
	if p.policy.MinDuration > 0 && v.Duration > 0 && v.Duration < p.policy.MinDuration {
		p.store.MarkSkipped(v.SourceID, v.VideoID, fmt.Sprintf("too short: %ds", v.Duration))
		return errSkipped
	}
	if p.policy.MaxDuration > 0 && v.Duration > p.policy.MaxDuration {
		p.store.MarkSkipped(v.SourceID, v.VideoID, fmt.Sprintf("too long: %ds", v.Duration))
		return errSkipped
	}
	if videoFile == nil {
		p.store.MarkSkipped(v.SourceID, v.VideoID, "no video recording")
		return errSkipped
	}
	client := netx.Client("warp-doh", "", 0) // no total timeout: large files
	channel := sanitize(v.Channel)
	if channel == "" {
		channel = "unknown"
	}
	dir := filepath.Join(p.outDir, channel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ext := videoFile.Ext
	if ext != "mp4" && ext != "webm" && ext != "mkv" {
		ext = "mp4"
	}
	base := filepath.Join(dir, v.VideoID+"_"+sanitize(v.Title)+"."+ext)
	if _, err := os.Stat(base); err == nil {
		// already downloaded (resume of a prior pass)
		info, _ := os.Stat(base)
		hash, herr := fileSHA256(base)
		if herr != nil {
			return herr
		}
		upd := model.Video{SourceID: v.SourceID, VideoID: v.VideoID, Title: v.Title,
			Duration: v.Duration, Published: v.Published, Channel: v.Channel,
			SizeBytes: info.Size(), Path: base, SHA256: hash}
		return p.store.MarkDownloaded(upd)
	}
	if err := fetchResume(client, videoFile.URL, base+".part", base); err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "fetch: "+err.Error())
		return fmt.Errorf("fetch %s: %w", videoFile.URL, err)
	}
	hash, err := fileSHA256(base)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "hash: "+err.Error())
		return err
	}
	upd := model.Video{SourceID: v.SourceID, VideoID: v.VideoID, Title: v.Title,
		Duration: v.Duration, Published: v.Published, Channel: v.Channel,
		SizeBytes: fileSize(base), Path: base, SHA256: hash}
	if err := p.store.MarkDownloaded(upd); err != nil {
		return err
	}
	if subFile != nil {
		subPath := filepath.Join(dir, v.VideoID+"_"+sanitize(v.Title)+"."+subFile.Ext)
		if _, err := os.Stat(subPath); err != nil {
			fetchResume(client, subFile.URL, subPath+".part", subPath)
		}
	}
	return nil
}

// fetchResume downloads url to tmp with Range resume, retrying on
// connection drops (WARP egress is flaky on long transfers — verified:
// mid-stream EOF). Each attempt continues from tmp's current size; on
// success the tmp file is renamed to dst (atomic).
func fetchResume(client *http.Client, url, tmp, dst string) error {
	const maxAttempts = 8
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := fetchOnce(client, url, tmp)
		if err == nil {
			return os.Rename(tmp, dst)
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return lastErr
}

func fetchOnce(client *http.Client, url, tmp string) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	resume, _ := f.Stat()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "videocrawl/1.0")
	if resume.Size() > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resume.Size()))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	restart := resp.StatusCode == 200 && resume.Size() > 0
	if restart {
		f.Truncate(0)
		f.Seek(0, 0)
	}
	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	f.Sync()
	return nil
}

// cccEventFiles fetches one event's detail and picks video+sub recordings.
// maxHeight<=480 prefers the sd variants (smaller files; the hd are 4-8x).
func cccEventFiles(eventURL string, maxHeight int) ([]model.File, error) {
	client := netx.Client("warp-doh", "", 0) // no total timeout: large files
	resp, err := client.Get(eventURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var e struct {
		Recordings []struct {
			Filename string `json:"filename"`
			URL      string `json:"recording_url"`
			MimeType string `json:"mime_type"`
			Language string `json:"language"`
			Size     int64  `json:"size"`
		} `json:"recordings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return nil, err
	}
	pref := []string{"h264-hd", "mp4", "h264-sd", "webm-hd", "webm-sd"}
	if maxHeight > 0 && maxHeight <= 480 {
		pref = []string{"h264-sd", "webm-sd", "h264-hd", "mp4", "webm-hd"}
	}
	var files []model.File
	for _, want := range pref {
		for _, r := range e.Recordings {
			if strings.Contains(r.Filename, want) && strings.HasPrefix(r.MimeType, "video/") {
				files = append(files, model.File{URL: r.URL, Size: r.Size << 20, Ext: extOf(r.Filename), Kind: "video"})
				goto haveVideo
			}
		}
	}
haveVideo:
	for _, r := range e.Recordings {
		if (strings.Contains(r.MimeType, "x-subrip") || strings.Contains(r.MimeType, "vtt")) &&
			(r.Language == "" || strings.HasPrefix(r.Language, "en")) {
			files = append(files, model.File{URL: r.URL, Ext: extOf(r.Filename), Kind: "sub"})
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no recordings")
	}
	return files, nil
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return "bin"
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', ' ', 0x27:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}

// EnsureDir creates the output tree.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}
