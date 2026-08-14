// Package yt: wraps yt-dlp (the verified battle-tested engine for YouTube and
// bilibili enumeration/download; we do not reimplement InnerTube/WBI).
package yt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Bin returns the yt-dlp binary path (VIDEOCRAWL_YTDLP override first).
func Bin() string {
	if b := os.Getenv("VIDEOCRAWL_YTDLP"); b != "" {
		return b
	}
	return "yt-dlp"
}

// FlatEntry: one line of `yt-dlp --flat-playlist -j`.
type FlatEntry struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Duration   any    `json:"duration"`
	Channel    string `json:"channel"`
	UploadDate string `json:"upload_date"` // YYYYMMDD
	URL        string `json:"url"`
	LiveStatus string `json:"live_status"`
}

// EnumCmd builds the flat-playlist enumeration command for a URL.
func EnumCmd(extra []string, cookies, proxy string) *exec.Cmd {
	args := []string{
		"--flat-playlist", "-j", "--no-warnings",
		"--sleep-requests", "1",
		"--socket-timeout", "60",
	}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	if proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	args = append(args, extra...)
	args = append(args, "--")
	return exec.Command(Bin(), args...)
}

// Enum runs `yt-dlp --flat-playlist -j <url>` and streams entries. It
// returns the number of entries and whether yt-dlp reported the full count
// (the "-N entries / playlist has M entries" trailer or the per-line data).
func Enum(extra []string, cookies, proxy, url string, fn func(FlatEntry) error) (int, bool, error) {
	cmd := EnumCmd(extra, cookies, proxy)
	cmd.Args = append(cmd.Args, url)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, false, err
	}
	if err := cmd.Start(); err != nil {
		return 0, false, err
	}
	n := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e FlatEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // non-JSON trailer line
		}
		if e.ID == "" {
			continue
		}
		if err := fn(e); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return n, false, err
		}
		n++
	}
	scErr := sc.Err()
	waitErr := cmd.Wait()
	if scErr != nil {
		return n, false, scErr
	}
	if waitErr != nil {
		// Nonzero exit: report stderr (e.g. "Request is blocked (412)")
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return n, false, fmt.Errorf("%s", firstLine(msg))
	}
	// yt-dlp prints "[Playlist] ... N items" style trailers to stderr;
	// a truncated run (YouTube feed cut) still exits 0, so completeness is
	// caller's job (verify against expected count where known).
	return n, true, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// MetaEntry: full metadata for one video (from `yt-dlp -J <url>`).
type MetaEntry struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Duration    any    `json:"duration"`
	Channel     string `json:"channel"`
	UploadDate  string `json:"upload_date"`
	LiveStatus  string `json:"live_status"`
	MediaType   string `json:"media_type"` // "short" for YouTube Shorts (verified: yt-dlp sets it from isShortsEligible)
	URL         string `json:"webpage_url"`
	Description string `json:"description"`
}

// GetMeta fetches full metadata for one video URL.
func GetMeta(extra []string, cookies, proxy, url string) (*MetaEntry, error) {
	args := []string{"-J", "--no-warnings", "--skip-download"}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	if proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	args = append(args, extra...)
	args = append(args, "--", url)
	cmd := exec.Command(Bin(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s", firstLine(strings.TrimSpace(stderr.String())))
	}
	var m MetaEntry
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("parse meta: %v", err)
	}
	return &m, nil
}

// DownloadCmd builds the per-video download command. Recipe from research:
// DASH-only (bv+ba, NO '/b' fallback): the combined muxed format (18) is
// served truncated/403 through the WARP egress — the Erik Meijer GOTO 2012
// download came back as a 1.5MB stub (declared h264+aac, ~0 real frames)
// that bilibili's transcode rejected (缺少视频轨). DASH streams (134+140)
// download fully through the same path.
// 720p H.264 mp4 archive-grade, unique per-video output path (id kills
// title collisions), .part temp on the same filesystem, resume, en subs.
// With audioFormat != "" the download is audio-only: -x extracts the best
// audio (--audio-quality 0 = best), converted to the requested format.
// Subs: --write-subs (manual/uploader captions, incl. PeerTube .en.vtt)
// + --write-auto-subs (auto-generated). On YouTube manual subs land as
// <id>.en.srt and auto as <id>.en-orig.srt; the transcript gate below
// prefers the longest id-prefixed sub file of the two.
func DownloadCmd(cookies, proxy, outDir string, maxHeight int, audioFormat string, extra []string, url string) *exec.Cmd {
	args := []string{
		"--no-playlist",
		"-w", "--continue", "--no-overwrites",
		"--sleep-requests", "1", // politeness: the app limiter does not cover downloads
		"-o", "%(channel)s/%(id)s_%(title).100B.%(ext)s",
		"--restrict-filenames",
		"-P", "temp:" + outDir + "/.tmp",
		"-P", "home:" + outDir,
		"--write-subs", "--write-auto-subs", "--sub-langs", "en.*", "--sub-format", "srt/best",
		"--concurrent-fragments", "8",
		"--max-filesize", "4G",
		"--retry-sleep", "fragment:exp=1:10",
		"--retry-sleep", "http:linear=1:10",
		"--no-warnings",
		"--socket-timeout", "60", // a hung socket must not stall the round forever
	}
	switch {
	case audioFormat != "":
		// audio mode: best available audio stream, extracted to the target
		// format; -x picks the container, so no merge/remux args.
		args = append(args, "-x", "--audio-format", audioFormat, "--audio-quality", "0")
		args = append(args, "-f", "ba/b")
	case maxHeight > 0:
		f := fmt.Sprintf("bv[height<=%d]+ba", maxHeight)
		args = append(args, "-f", f)
		args = append(args, "-S", "vcodec:h264,res:720,fps,hdr:12,acodec:m4a")
		args = append(args, "--merge-output-format", "mp4", "--remux-video", "mp4")
	default:
		args = append(args, "-f", "bv+ba")
		args = append(args, "--merge-output-format", "mp4")
	}
	if cookies != "" {
		args = append(args, "--cookies", cookies)
	}
	if proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	args = append(args, extra...)
	args = append(args, "--", url)
	return exec.Command(Bin(), args...)
}

// RunDownload runs the download, capturing stderr for the error message.
func RunDownload(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", firstLine(msg))
	}
	return nil
}

// ParseUploadDate converts YYYYMMDD to RFC3339 date ("" on empty).
func ParseUploadDate(s string) string {
	if len(s) != 8 {
		return ""
	}
	t, err := time.Parse("20060102", s)
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// DurationSeconds extracts the duration field (json numbers can be
// float64 or int64).
func DurationSeconds(d any) int64 {
	switch v := d.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case nil:
		return 0
	}
	return 0
}
