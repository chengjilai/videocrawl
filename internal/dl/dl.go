// Package dl: download workers. Each worker takes one video at a time,
// fetches metadata (policy applied: duration bounds, live skip), runs
// yt-dlp with the per-site recipe, then hashes the finished file and
// records it. Oldest-first order comes from the store query.
package dl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"videocrawl/internal/enum"
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
	store   *store.Store
	sites   map[string]sites.Site
	outDir  string
	policy  Policy
	workers int
	mu      sync.Mutex
	done    int
	fail    int
	skip    int
	// shared gallica session (altcha PoW solved once per pool)
	gallicaMu sync.Mutex
	gallica   *enum.Gallica
}

func NewPool(st *store.Store, sites map[string]sites.Site, outDir string, policy Policy, workers int) *Pool {
	return &Pool{store: st, sites: sites, outDir: outDir, policy: policy, workers: workers}
}

func (p *Pool) Counts() (done, fail, skip int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done, p.fail, p.skip
}

// Expired reports whether a cooperative pass should stop: the context was
// canceled (SIGINT/SIGTERM) or the per-round deadline passed. Checked
// between videos/sources, never mid-download — the current item finishes
// (crash-safe .part resume handles the rest).
func Expired(ctx context.Context, deadline time.Time) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return !deadline.IsZero() && time.Now().After(deadline)
}

// Run processes videos until the queue is empty, limit is reached, or the
// pass budget/signal fires (cooperative: in-flight videos finish first).
func (p *Pool) Run(ctx context.Context, limit int, deadline time.Time) error {
	if limit <= 0 {
		limit = 1 << 30
	}
	// table scan batches: we can't stream directly with modernc sqlite
	// (single conn), so pull small batches and track done via status.
	totalDone := 0
	for totalDone < limit {
		if Expired(ctx, deadline) {
			fmt.Fprintf(os.Stderr, "download: pass stopped early (time budget/signal)\n")
			return nil
		}
		batch, err := p.store.NextForDownload(p.workers*2, p.policy.MinDuration, p.policy.MaxDuration)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, p.workers)
		remaining := limit - totalDone
		launched := 0
		for _, v := range batch {
			if launched >= remaining {
				break
			}
			if Expired(ctx, deadline) {
				fmt.Fprintf(os.Stderr, "download: stopping early (time budget/signal); finishing in-flight videos\n")
				break
			}
			launched++
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
		totalDone += launched
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
		p.store.MarkFailed(v.SourceID, v.VideoID, "source: "+err.Error())
		return fmt.Errorf("source: %w", err)
	}
	cfg := p.sites[src.Site]
	proxy := sites.ProxyURL(cfg)

	// native-download sites: ccc (static files via DoH+WARP), archive-audio
	// (audio files via the site's proxy dial), gallica (PDF via altcha flow).
	if cfg.Dial == "warp-doh" || src.Kind == model.KindArchiveAudio || src.Kind == model.KindGallica {
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
	// SkipShorts: yt-dlp full metadata sets media_type="short" (from
	// isShortsEligible), so this is reliable even though flat entries lack
	// duration. The min-dur policy alone cannot catch Shorts (up to 3 min,
	// and unknown durations pass `dur > 0`).
	if p.policy.SkipShorts && meta.MediaType == "short" {
		p.store.MarkSkipped(v.SourceID, v.VideoID, "youtube short")
		return errSkipped
	}

	// 2. download
	cmd := yt.DownloadCmd(cfg.Cookies, proxy, p.outDir, cfg.MaxHeight, cfg.AudioFormat, cfg.DLArgs, v.URL)
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
	var best string
	var bestMtime time.Time
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
			if !strings.HasPrefix(base, videoID+"_") {
				continue
			}
			if strings.Contains(base, ".part") {
				continue // temp or fragment files
			}
			ext := filepath.Ext(base)
			if ext != ".mp4" && ext != ".mkv" && ext != ".webm" && ext != ".flv" &&
				ext != ".m4a" && ext != ".mp3" && ext != ".flac" && ext != ".opus" && ext != ".ogg" {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(bestMtime) {
				bestMtime = info.ModTime()
				best = filepath.Join(outDir, dir.Name(), base)
			}
		}
	}
	if best == "" {
		return "", 0, fmt.Errorf("no output file for %s", videoID)
	}
	info, _ := os.Stat(best)
	return best, info.Size(), nil
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

// processNative: static-file download. Dispatch by kind:
//   - ccc: pick the best video recording + en subtitles recorded at
//     enumeration time, fetch through the warp-doh transport with parallel
//     Range stripes (see fetchStriped), .part -> atomic rename, sha256.
//   - archive-audio: archive.org metadata -> audio files (mp3>flac>ogg),
//     Range-resume fetch each, sha256, extras recorded in media_files.
//   - gallica: PDF score through the altcha PoW + cookie flow.
func (p *Pool) processNative(v model.Video, src model.Source, cfg sites.Site) error {
	switch src.Kind {
	case model.KindArchiveAudio:
		return p.processArchiveAudio(v, src, cfg)
	case model.KindGallica:
		return p.processGallica(v, src, cfg)
	}
	files, err := p.store.GetFiles(v.SourceID, v.VideoID)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "files: "+err.Error())
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
		p.store.MarkFailed(v.SourceID, v.VideoID, "mkdir: "+err.Error())
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
	if err := fetchStriped(client, videoFile.URL, base+".part", base); err != nil {
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
			FetchResume(client, subFile.URL, subPath+".part", subPath)
		}
	}
	return nil
}

// ---- archive-audio native download ----

// iaFile: one file entry of https://archive.org/metadata/<id>.
type iaFile struct {
	Name   string `json:"name"`
	Format string `json:"format"`
}

// iaMetadata: the parts of the archive.org metadata document we use.
// Title/Mediatype/Metadata also feed the music post-loop's law gate
// (internal/app/post.go) — the full /metadata/ doc is unmarshalled.
type iaMetadata struct {
	Title     string         `json:"title"`
	Mediatype string         `json:"mediatype"`
	Metadata  map[string]any `json:"metadata"`
	Server    string         `json:"server"`
	Dir       string         `json:"dir"`
	Files     []iaFile       `json:"files"`
}

// archiveMetadata fetches https://archive.org/metadata/{id} through the
// site's transport (archive site config: smart-proxy).
func ArchiveMetadata(client *http.Client, id string) (*iaMetadata, error) {
	u := "https://archive.org/metadata/" + url.PathEscape(id)
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("archive.org metadata %d: %s", resp.StatusCode, strings.TrimSpace(string(b))[:min(len(b), 160)])
	}
	var m iaMetadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// pickAudioFiles selects the audio tracks of an archive.org audio item: the
// best present format tier wins (mp3 > flac > ogg > other audio) and
// non-track files (lecture recordings, metadata) are skipped. Metadata order
// is track order. mp3 is preferred over flac: 78rpm-era transfers are
// needle-drop audio where the derived mp3 is the portable republishing
// format (small files, lossy generation is irrelevant), and the crawler's
// output feeds a size-bounded upload pipeline.
func PickAudioFiles(files []iaFile) []iaFile {
	for _, tier := range []string{"mp3", "flac", "ogg", "opus", "m4a", "wav", "aac"} {
		var chosen []iaFile
		for _, f := range files {
			n := strings.ToLower(f.Name)
			if strings.HasPrefix(n, ".") || strings.Contains(n, "lecture") {
				continue
			}
			// skip archive.org derived low-bitrate copies (track_64kb.mp3)
			if kbRe.MatchString(n) {
				continue
			}
			if strings.EqualFold(extOf(f.Name), tier) {
				chosen = append(chosen, f)
			}
		}
		if len(chosen) > 0 {
			return chosen
		}
	}
	return nil
}

var kbRe = regexp.MustCompile(`_\d+kb\.`)

// ArchiveDownloadURL builds the canonical file URL (https://archive.org/
// download/<id>/<name>, subdirs preserved, path-escaped).
func ArchiveDownloadURL(id, name string) string {
	u := url.URL{Scheme: "https", Host: "archive.org", Path: "/download/" + id + "/" + name}
	return u.String()
}

// ArchiveDirectURL builds the item-node URL (https://<server><dir>/<name>)
// from the metadata's server+dir fields — skips the /download/ redirect
// hop (measured ~2.5s vs ~5.2s first byte through the proxy). Falls back
// to the /download/ redirect URL when the metadata lacked server/dir.
func ArchiveDirectURL(m *iaMetadata, id, name string) string {
	if m != nil && m.Server != "" {
		u := url.URL{Scheme: "https", Host: m.Server, Path: m.Dir + "/" + name}
		return u.String()
	}
	return ArchiveDownloadURL(id, name)
}

// processArchiveAudio: fetch the item's audio files natively. The primary
// (preferred format tier, first track) is recorded on the video row; the rest of
// the tier's tracks go to the media_files table. All fetches use fetchResume
// (Range-resume, .part -> rename), idempotent across crash-restarts.
func (p *Pool) processArchiveAudio(v model.Video, src model.Source, cfg sites.Site) error {
	dial := cfg.Dial
	if dial == "" && sites.ProxyURL(cfg) != "" {
		dial = "proxy"
	}
	client := netx.Client(dial, sites.ProxyURL(cfg), 0) // no total timeout: large files
	meta, err := ArchiveMetadata(client, v.VideoID)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "metadata: "+err.Error())
		return fmt.Errorf("archive metadata: %w", err)
	}
	files := PickAudioFiles(meta.Files)
	if len(files) == 0 {
		p.store.MarkSkipped(v.SourceID, v.VideoID, "no audio files (flac/mp3/ogg)")
		return errSkipped
	}
	dir := filepath.Join(p.outDir, "archive-audio")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "mkdir: "+err.Error())
		return err
	}
	title := v.Title
	if title == "" {
		title = v.VideoID
	}
	// primary file (best tier, first track)
	primary := files[0]
	dest := filepath.Join(dir, v.VideoID+"_"+sanitize(title)+"."+strings.ToLower(extOf(primary.Name)))
	if _, err := os.Stat(dest); err != nil {
		if err := FetchResume(client, ArchiveDownloadURL(v.VideoID, primary.Name), dest+".part", dest); err != nil {
			p.store.MarkFailed(v.SourceID, v.VideoID, "fetch: "+err.Error())
			return fmt.Errorf("fetch %s: %w", primary.Name, err)
		}
	}
	hash, err := fileSHA256(dest)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "hash: "+err.Error())
		return err
	}
	upd := model.Video{SourceID: v.SourceID, VideoID: v.VideoID, Title: title,
		Duration: v.Duration, Published: v.Published, Channel: v.Channel,
		SizeBytes: fileSize(dest), Path: dest, SHA256: hash}
	if err := p.store.MarkDownloaded(upd); err != nil {
		return err
	}
	// extra tracks of the same tier: media_files table. A failed extra is
	// logged and skipped (the primary is already recorded done); the row
	// stays out of media_files until a later pass re-records it.
	var extras []model.MediaFile
	for _, f := range files[1:] {
		ep := filepath.Join(dir, v.VideoID+"_"+sanitize(f.Name))
		if _, err := os.Stat(ep); err != nil {
			if err := FetchResume(client, ArchiveDownloadURL(v.VideoID, f.Name), ep+".part", ep); err != nil {
				fmt.Fprintf(os.Stderr, "WARN %s extra %s: %v\n", v.VideoID, f.Name, err)
				continue
			}
		}
		h, herr := fileSHA256(ep)
		if herr != nil {
			fmt.Fprintf(os.Stderr, "WARN %s extra %s: %v\n", v.VideoID, f.Name, herr)
			continue
		}
		extras = append(extras, model.MediaFile{
			SourceID: v.SourceID, VideoID: v.VideoID,
			URL:  ArchiveDownloadURL(v.VideoID, f.Name),
			Path: ep, SHA256: h, SizeBytes: fileSize(ep),
			Ext: strings.ToLower(extOf(f.Name)),
		})
	}
	if len(extras) > 0 {
		if err := p.store.UpsertMediaFiles(v.SourceID, v.VideoID, extras); err != nil {
			return err
		}
	}
	return nil
}

// processGallica: download one ark's PDF score through the altcha PoW +
// cookie flow (see enum/gallica.go), sha256, record on the video row.
func (p *Pool) processGallica(v model.Video, src model.Source, cfg sites.Site) error {
	dir := filepath.Join(p.outDir, "gallica")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "mkdir: "+err.Error())
		return err
	}
	title := v.Title
	if title == "" {
		title = v.VideoID
	}
	dest := filepath.Join(dir, v.VideoID+"_"+sanitize(title)+".pdf")
	lock := destLock(dest)
	lock.Lock()
	defer lock.Unlock()
	// An existing dest is only trusted when it is a real, nonempty PDF
	// (a pre-.part-era partial file must be re-downloaded).
	fi, statErr := os.Stat(dest)
	if statErr != nil || fi.Size() == 0 || !enum.FileIsPDF(dest) {
		cl := p.gallicaClient(cfg)
		if err := cl.Download(context.Background(), v.VideoID, dest); err != nil {
			p.store.MarkFailed(v.SourceID, v.VideoID, "gallica: "+err.Error())
			return fmt.Errorf("gallica: %w", err)
		}
	}
	hash, err := fileSHA256(dest)
	if err != nil {
		p.store.MarkFailed(v.SourceID, v.VideoID, "hash: "+err.Error())
		return err
	}
	upd := model.Video{SourceID: v.SourceID, VideoID: v.VideoID, Title: title,
		Duration: 0, Published: v.Published, Channel: v.Channel,
		SizeBytes: fileSize(dest), Path: dest, SHA256: hash}
	return p.store.MarkDownloaded(upd)
}

// gallicaClient: one Gallica session per pool (the altcha PoW is solved once
// per session — a fresh client per video would pay the full solve each time).
func (p *Pool) gallicaClient(cfg sites.Site) *enum.Gallica {
	p.gallicaMu.Lock()
	defer p.gallicaMu.Unlock()
	if p.gallica == nil {
		p.gallica = enum.NewGallica(cfg)
	}
	return p.gallica
}

// destLock: per-destination mutex so two workers (duplicate rows, overlapping
// sources) never write the same file concurrently.
var destLocks sync.Map

func destLock(dest string) *sync.Mutex {
	v, _ := destLocks.LoadOrStore(dest, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// fetchResume downloads url to tmp with Range resume, retrying on
// connection drops (WARP egress is flaky on long transfers — verified:
// mid-stream EOF). Each attempt continues from tmp's current size; on
// success the tmp file is renamed to dst (atomic). Still used for
// subtitles (small) and as the single-stream fallback in fetchStriped
// (no-range servers, small files, probe failures).
func FetchResume(client *http.Client, url, tmp, dst string) error {
	return FetchResumeCtx(context.Background(), client, url, tmp, dst)
}

// FetchResumeCtx is FetchResume with a cancellable context — the music
// post-loop passes its signal context so SIGTERM aborts an in-flight
// download instead of finishing it.
func FetchResumeCtx(ctx context.Context, client *http.Client, url, tmp, dst string) error {
	const maxAttempts = 8
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := fetchOnce(ctx, client, url, tmp)
		if err == nil {
			return os.Rename(tmp, dst)
		}
		lastErr = err
		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}
	return lastErr
}

func fetchOnce(ctx context.Context, client *http.Client, url, tmp string) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	resume, _ := f.Stat()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
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

// ---- parallel range-fetch stripes ----
//
// A single WARP stream tops out around 130KB/s (verified), so one
// Range-resume connection per file is the throughput bottleneck. Instead we
// split each file into N disjoint byte ranges fetched concurrently
// (aria2-style), each stripe on its own WARP connection + socks hop, and
// assemble them by WriteAt into a sparse-preallocated .part file. Each
// stripe retries from its own progress on connection drops (the verified
// WARP mid-stream EOF). Politeness: the ccc mirror is volunteer-run, so
// stripes are capped at 6 (default 4) and a token bucket shared by all
// stripes caps each file's aggregate rate (default 4 MiB/s).

const (
	nativeStripesDefault = 4
	nativeStripesMax     = 6
	nativeRateCeilBytes  = 4 << 20 // per-file ceiling, bytes/s
	nativeSmallFile      = 8 << 20 // below this: single stream is enough
)

// nativeConfig resolves the per-file download tuning from the environment
// (VIDEOCRAWL_STRIPES, VIDEOCRAWL_RATE_CEIL_MB).
func nativeConfig() (stripes int, rateCeil int64) {
	stripes = nativeStripesDefault
	if v := os.Getenv("VIDEOCRAWL_STRIPES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			if n > nativeStripesMax {
				n = nativeStripesMax
			}
			stripes = n
		}
	}
	rateCeil = nativeRateCeilBytes
	if v := os.Getenv("VIDEOCRAWL_RATE_CEIL_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			rateCeil = int64(n) << 20
		}
	}
	return stripes, rateCeil
}

// fetchStriped downloads url to tmp with N parallel Range GETs and renames
// to dst on success. Falls back to the single-stream fetchResume when the
// server lacks range support, the file is small, or the probe fails.
func fetchStriped(client *http.Client, url, tmp, dst string) error {
	total, ranges, err := probeRange(client, url)
	if err != nil || !ranges || total < nativeSmallFile {
		return FetchResume(client, url, tmp, dst)
	}
	stripes, rateCeil := nativeConfig()
	// never split into stripes smaller than ~1MiB
	if stripes > 1 && total/int64(stripes) < 1<<20 {
		stripes = int(total / (1 << 20))
		if stripes < 1 {
			stripes = 1
		}
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// sparse-preallocate: holes read as zeros; stripes WriteAt disjoint
	// regions (concurrent WriteAt is safe; pwrite, no shared offset). Note:
	// striped progress is not persisted, so a restarted .part is fetched
	// from scratch (single-stream resume is kept for subs/fallback).
	if err := f.Truncate(total); err != nil {
		return err
	}
	lim := newByteLimiter(rateCeil)
	errc := make(chan error, stripes)
	var wg sync.WaitGroup
	var writtenMu sync.Mutex
	var writtenTotal int64
	for i := 0; i < stripes; i++ {
		start := total * int64(i) / int64(stripes)
		end := total * int64(i+1) / int64(stripes)
		if i == stripes-1 {
			end = total
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(start, end int64) {
			defer wg.Done()
			n, err := fetchStripe(client, url, f, start, end, lim)
			writtenMu.Lock()
			writtenTotal += n
			writtenMu.Unlock()
			if err != nil {
				errc <- fmt.Errorf("stripe [%d-%d): %w", start, end, err)
			}
		}(start, end)
	}
	wg.Wait()
	close(errc)
	for e := range errc {
		return e
	}
	if writtenTotal != total {
		return fmt.Errorf("striped fetch short: wrote %d of %d bytes", writtenTotal, total)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// fetchStripe downloads one disjoint [start,end) region of url into f via
// WriteAt, resuming from stripe-local progress on connection drops.
func fetchStripe(client *http.Client, url string, f *os.File, start, end int64, lim *byteLimiter) (int64, error) {
	const maxAttempts = 8
	var done int64
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		n, err := stripeOnce(client, url, f, start+done, end, lim)
		done += n
		if err == nil {
			return done, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return done, lastErr
}

// stripeOnce makes one Range GET for [from,end) and writes the body at
// absolute offsets, returning bytes written. Defensively handles servers
// that ignore Range (200 full body: discard the prefix) and 416 (range past
// EOF: nothing left to fetch).
func stripeOnce(client *http.Client, url string, f *os.File, from, end int64, lim *byteLimiter) (int64, error) {
	if from >= end {
		return 0, nil
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "videocrawl/1.0")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, end-1))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206: range honored, body starts at from
	case http.StatusRequestedRangeNotSatisfiable: // 416: stripe already past EOF
		return 0, nil
	case http.StatusOK: // server ignored Range: full body from 0
		if from > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, from); err != nil {
				return 0, err
			}
		}
	default:
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	n, err := copyLimitedAt(f, from, io.LimitReader(resp.Body, end-from), lim)
	if err != nil {
		return n, err
	}
	if n < end-from {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// copyLimitedAt copies r into f at absolute offset off (WriteAt: disjoint
// ranges from concurrent stripes are safe), throttled by the shared limiter.
func copyLimitedAt(f *os.File, off int64, r io.Reader, lim *byteLimiter) (int64, error) {
	buf := make([]byte, 128<<10)
	var written int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			lim.wait(n)
			wn, werr := f.WriteAt(buf[:n], off+written)
			written += int64(wn)
			if werr != nil {
				return written, werr
			}
			if wn != n {
				return written, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// probeRange asks the server for byte 0 and its total size. Returns
// (size, true) when the server honors ranges (206 + Content-Range), or
// (size, false) for a plain 200 (no range support).
func probeRange(client *http.Client, url string) (int64, bool, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "videocrawl/1.0")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // tiny, drain for keep-alive
	switch resp.StatusCode {
	case http.StatusPartialContent:
		total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		return total, ok, nil
	case http.StatusOK:
		return resp.ContentLength, false, nil
	default:
		return 0, false, fmt.Errorf("probe: http %d", resp.StatusCode)
	}
}

// parseContentRange extracts the total size from "bytes 0-0/<total>".
func parseContentRange(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0, false
	}
	total := strings.TrimSpace(v[i+1:])
	if total == "*" {
		return 0, false
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// byteLimiter: token bucket shared by all stripes of one file so the
// aggregate rate never exceeds the ceiling. nil receiver = unlimited.
// Errs toward slower under contention (polite direction).
type byteLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64
	tokens float64
	last   time.Time
}

func newByteLimiter(rate int64) *byteLimiter {
	if rate <= 0 {
		return nil
	}
	return &byteLimiter{rate: float64(rate), burst: float64(rate) / 4, last: time.Now()}
}

// wait blocks until n tokens are available.
func (l *byteLimiter) wait(n int) {
	if l == nil {
		return
	}
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
		if l.tokens >= float64(n) {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return
		}
		deficit := float64(n) - l.tokens
		l.tokens = 0
		l.mu.Unlock()
		time.Sleep(time.Duration(deficit / l.rate * float64(time.Second)))
	}
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
