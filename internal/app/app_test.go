package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"videocrawl/internal/model"
	"videocrawl/internal/store"
)

const testUCID = "UCabcdefghijklmnopqrstuv" // "UC" + 22 chars = 24 total

// stubYTDLP installs an executable stand-in for yt-dlp whose stdout is
// canned (used by resolveChannelID for @handle seeds) and sets
// VIDEOCRAWL_YTDLP. No network involved.
func stubYTDLP(t *testing.T, stdout, stderr string, code int) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yt-dlp-stub")
	script := "#!/bin/sh\n"
	if stderr != "" {
		script += "echo '" + stderr + "' >&2\n"
	}
	script += "cat <<'VC_EOF'\n" + stdout + "\nVC_EOF\n"
	if code != 0 {
		script += fmt.Sprintf("exit %d\n", code)
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIDEOCRAWL_YTDLP", p)
}

func TestNormalize(t *testing.T) {
	uuPlaylist := "https://www.youtube.com/playlist?list=UUabcdefghijklmnopqrstuv"
	cases := []struct {
		name       string
		kind, raw  string
		query      string
		wantURL    string
		wantQuery  string
		wantErrSub string
		ytdlp      bool // needs the stub to answer with testUCID
	}{
		// youtube-channel: UC id / channel URL resolved locally; @handle via
		// the (stubbed) yt-dlp metadata call.
		{"youtube uc id", model.KindYoutubeChannel, testUCID, "", uuPlaylist, "", "", false},
		{"youtube channel url", model.KindYoutubeChannel, "https://www.youtube.com/channel/" + testUCID + "/videos", "", uuPlaylist, "", "", false},
		{"youtube channel url bare", model.KindYoutubeChannel, "https://www.youtube.com/channel/" + testUCID, "", uuPlaylist, "", "", false},
		{"youtube handle", model.KindYoutubeChannel, "@GNOME", "", uuPlaylist, "", "", true},
		{"youtube handle bare", model.KindYoutubeChannel, "GNOME", "", uuPlaylist, "", "", true},
		{"youtube handle url", model.KindYoutubeChannel, "https://www.youtube.com/@GNOME/videos", "", uuPlaylist, "", "", true},
		{"youtube short uc treated as handle", model.KindYoutubeChannel, "UCshort", "", uuPlaylist, "", "", true},

		// youtube-playlist
		{"youtube playlist id", model.KindYoutubePlaylist, "PL1234567890", "", "https://www.youtube.com/playlist?list=PL1234567890", "", "", false},
		{"youtube playlist url", model.KindYoutubePlaylist, "https://www.youtube.com/playlist?list=PL1234567890", "", "https://www.youtube.com/playlist?list=PL1234567890", "", "", false},

		// bilibili-space
		{"bilibili mid", model.KindBilibiliSpace, "306049207", "", "https://space.bilibili.com/306049207/video", "", "", false},
		{"bilibili space url", model.KindBilibiliSpace, "https://space.bilibili.com/306049207/video", "", "https://space.bilibili.com/306049207/video", "", "", false},
		{"bilibili space url no suffix", model.KindBilibiliSpace, "https://space.bilibili.com/306049207", "", "https://space.bilibili.com/306049207/video", "", "", false},
		{"bilibili bad mid", model.KindBilibiliSpace, "abc", "", "", "", "needs a numeric mid", false},
		{"bilibili empty mid", model.KindBilibiliSpace, "https://space.bilibili.com//video", "", "", "", "needs a numeric mid", false},

		// bilibili-fav (pure string parsing, no network)
		{"bilibili fav digits", model.KindBilibiliFav, "12345", "", "https://www.bilibili.com/medialist/detail/ml12345", "", "", false},
		{"bilibili fav url fid", model.KindBilibiliFav, "https://space.bilibili.com/12345/favlist?fid=54321", "", "https://www.bilibili.com/medialist/detail/ml54321", "", "", false},
		{"bilibili fav medialist url", model.KindBilibiliFav, "https://www.bilibili.com/medialist/detail/ml98765", "", "https://www.bilibili.com/medialist/detail/ml98765", "", "", false},
		{"bilibili fav garbage", model.KindBilibiliFav, "not-a-fid", "", "", "", "needs a numeric media_id", false},

		// peertube
		{"peertube channel", model.KindPeertubeChannel, "https://tilvids.com/video-channels/fosstodon", "", "https://tilvids.com/video-channels/fosstodon", "", "", false},
		{"peertube channel bare", model.KindPeertubeChannel, "tilvids.com/video-channels/fosstodon", "", "", "", "needs a full URL", false},
		{"peertube search", model.KindPeertubeSearch, "https://tilvids.com", "linux", "https://tilvids.com", "linux", "", false},
		{"peertube search no query", model.KindPeertubeSearch, "https://tilvids.com", "", "", "", "needs --query", false},
		{"peertube search bare url", model.KindPeertubeSearch, "tilvids.com", "linux", "", "", "needs an instance URL", false},

		// media.ccc.de
		{"ccc conf", model.KindCCCConf, "37c3", "", "https://media.ccc.de/public/conferences/37c3", "", "", false},
		{"ccc conf slashes", model.KindCCCConf, "/37c3/", "", "https://media.ccc.de/public/conferences/37c3", "", "", false},
		{"ccc search", model.KindCCCSearch, "x", "chaos", "https://media.ccc.de/public/events/search", "chaos", "", false},
		{"ccc search no query", model.KindCCCSearch, "x", "", "", "", "needs --query", false},

		// archive.org
		{"archive query", model.KindArchiveQuery, "x", "mediatype:movies", "https://archive.org/advancedsearch.php", "mediatype:movies", "", false},
		{"archive query no q", model.KindArchiveQuery, "x", "", "", "", "needs --query", false},

		// rss
		{"rss url", model.KindRSS, "https://example.com/feed.xml", "", "https://example.com/feed.xml", "", "", false},
		{"rss bare", model.KindRSS, "example.com/feed.xml", "", "", "", "needs a feed URL", false},

		{"unknown kind", "bogus-kind", "x", "", "", "", "unknown kind", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.ytdlp {
				stubYTDLP(t, `{"channel_id": "`+testUCID+`"}`, "", 0)
			}
			url, q, err := normalize(c.kind, c.raw, c.query)
			if c.wantErrSub != "" {
				if err == nil {
					t.Fatalf("normalize(%s, %q, %q) = (%q, %q), want error containing %q",
						c.kind, c.raw, c.query, url, q, c.wantErrSub)
				}
				if !strings.Contains(err.Error(), c.wantErrSub) {
					t.Fatalf("normalize(%s, %q, %q) err = %q, want substring %q",
						c.kind, c.raw, c.query, err.Error(), c.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize(%s, %q, %q): %v", c.kind, c.raw, c.query, err)
			}
			if url != c.wantURL || q != c.wantQuery {
				t.Fatalf("normalize(%s, %q, %q) = (%q, %q), want (%q, %q)",
					c.kind, c.raw, c.query, url, q, c.wantURL, c.wantQuery)
			}
		})
	}
}

// TestNormalizeUnexpectedChannelID: the stub returns a non-UC id → the
// seed is rejected with a clear error (no network).
func TestNormalizeUnexpectedChannelID(t *testing.T) {
	stubYTDLP(t, `{"channel_id": "xyz"}`, "", 0)
	_, _, err := normalize(model.KindYoutubeChannel, "@handle", "")
	if err == nil || !strings.Contains(err.Error(), "unexpected channel id") {
		t.Fatalf("err = %v, want 'unexpected channel id'", err)
	}
}

// TestNormalizeYTDLPError: the stub exits non-zero → the yt-dlp failure
// is wrapped with context.
func TestNormalizeYTDLPError(t *testing.T) {
	stubYTDLP(t, "", "yt-dlp: Video unavailable", 1)
	_, _, err := normalize(model.KindYoutubeChannel, "@handle", "")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "resolve channel") || !strings.Contains(err.Error(), "yt-dlp:") {
		t.Fatalf("err = %q, want 'resolve channel: yt-dlp: ...'", err.Error())
	}
}

func TestGuessName(t *testing.T) {
	cases := []struct{ kind, raw, want string }{
		{model.KindYoutubeChannel, "https://www.youtube.com/@GNOME", "youtube-channel:GNOME"},
		{model.KindYoutubeChannel, "@GNOME", "youtube-channel:GNOME"},
		{model.KindYoutubeChannel, testUCID, "youtube-channel:abcdefghijklmnopqrstuv"},
		{model.KindYoutubeChannel, "https://www.youtube.com/channel/" + testUCID, "youtube-channel:abcdefghijklmnopqrstuv"},
		{model.KindYoutubePlaylist, "PL1234567890", "youtube-playlist:playlist"},
		{model.KindBilibiliSpace, "https://space.bilibili.com/306049207/video", "bilibili-space:video"},
		{model.KindBilibiliFav, "https://space.bilibili.com/12345/favlist?fid=54321", "bilibili-fav:54321"},
		{model.KindBilibiliFav, "54321", "bilibili-fav:54321"},
		{model.KindCCCConf, "https://media.ccc.de/public/conferences/37c3", "ccc-conf:37c3"},
		{model.KindRSS, "https://example.com/feed.xml", "rss:feed.xml"},
		{model.KindArchiveQuery, "query-term", "archive-query:query-term"},
		{model.KindYoutubeChannel, "Some Channel Name", "youtube-channel:Some Channel Name"},
	}
	for _, c := range cases {
		if got := guessName(c.kind, c.raw); got != c.want {
			t.Errorf("guessName(%q, %q) = %q, want %q", c.kind, c.raw, got, c.want)
		}
	}
}

func TestTrunc(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 5, "hello"},
		{"hello", 10, "hello"},      // n > len: unchanged
		{"hello world", 5, "hell…"}, // n-1 runes + ellipsis
		{"", 5, ""},
		{"héllo", 3, "hé…"}, // rune-aware, not byte-aware
		{"日本語", 2, "日…"},
		{"hello", 1, "…"},
		{"a", 1, "a"},
	}
	for _, c := range cases {
		if got := trunc(c.s, c.n); got != c.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

// ---- upload ----

// uploadStub installs a fake upload_web.py (valid python3, since uploadOne
// shells out via python3): it records its full argv to $UPLOAD_STUB_ARGS
// and prints a canned stdout with the given exit code.
func uploadStub(t *testing.T, stdout string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload_web.py")
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("UPLOAD_STUB_ARGS", argsFile)
	script := "#!/usr/bin/env python3\n"
	script += "import os, sys\n"
	script += "with open(os.environ['UPLOAD_STUB_ARGS'], 'w') as f: f.write('\\x00'.join(sys.argv))\n"
	if stdout != "" {
		script += "print(" + strconv.Quote(stdout) + ")\n"
	}
	if code != 0 {
		script += fmt.Sprintf("sys.exit(%d)\n", code)
	}
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func stubArgs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("UPLOAD_STUB_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\x00") // NUL-separated: values may span newlines
}

func hasArg(args []string, key, want string) bool {
	for i, a := range args {
		if a == key && i+1 < len(args) && args[i+1] == want {
			return true
		}
	}
	return false
}

// uploadTestApp builds an App over a fresh DB with one done video and its
// file on disk. kind/url/channel drive the source row and video row.
func uploadTestApp(t *testing.T, kind, url, channel string) (*App, *store.Store, model.Video) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "vc.db")
	outDir := filepath.Join(dir, "out")
	videoFile := filepath.Join(outDir, "chan", "vid1_some_title.mp4")
	if err := os.MkdirAll(filepath.Dir(videoFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoFile, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srcID, err := st.AddSource(kind, url, "", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertVideo(model.Video{SourceID: srcID, VideoID: "vid1", URL: url, Title: "Talk Title", Channel: channel}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDownloaded(model.Video{SourceID: srcID, VideoID: "vid1", Title: "Talk Title",
		Published: "2024-01-01", Channel: channel, SizeBytes: 4, Path: videoFile, SHA256: "aa"}); err != nil {
		t.Fatal(err)
	}
	a := &App{DBPath: dbPath, OutDir: outDir}
	return a, st, model.Video{SourceID: srcID, VideoID: "vid1", URL: url, Title: "Talk Title", Channel: channel}
}

func TestParseUploadAllowlist(t *testing.T) {
	if _, err := parseUploadAllowlist(""); err == nil {
		t.Fatal("empty allowlist must refuse")
	}
	g, err := parseUploadAllowlist("cc")
	if err != nil {
		t.Fatal(err)
	}
	if !g.allows(model.Source{ID: 1, Site: "ccc"}) || g.allows(model.Source{ID: 1, Site: "youtube"}) {
		t.Errorf("'cc' must allow site=ccc only")
	}
	g, err = parseUploadAllowlist("3, 7")
	if err != nil {
		t.Fatal(err)
	}
	if !g.allows(model.Source{ID: 3, Site: "youtube"}) || !g.allows(model.Source{ID: 7}) || g.allows(model.Source{ID: 4}) {
		t.Errorf("id list: got %+v", g)
	}
	if _, err := parseUploadAllowlist("cc,bogus"); err == nil {
		t.Fatal("non-numeric token must be rejected")
	}
}

func TestUploadDryRun(t *testing.T) {
	a, st, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	a.UploadScript = uploadStub(t, "", 0)
	if err := a.Upload(0, true, "cc", "", a.OutDir); err != nil {
		t.Fatal(err)
	}
	// nothing may change in dry-run mode
	rows, _ := st.VideoRows("", 10)
	if len(rows) != 1 || rows[0].Status != model.StatusDone || rows[0].BVID != "" {
		t.Errorf("dry-run mutated the DB: %+v", rows)
	}
	// no uploader invocation
	if _, err := os.Stat(os.Getenv("UPLOAD_STUB_ARGS")); err == nil {
		t.Error("dry-run invoked the uploader")
	}
}

func TestUploadHappyPath(t *testing.T) {
	a, st, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	// an English subtitle file next to the video (yt-dlp --write-auto-subs)
	rows, _ := st.VideoRows("", 10)
	videoPath := rows[0].Path
	if err := os.WriteFile(strings.TrimSuffix(videoPath, ".mp4")+".en.srt", []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.UploadScript = uploadStub(t, "SUBMIT OK — https://www.bilibili.com/video/BV1test000001\n", 0)
	if err := a.Upload(0, false, "cc", "", a.OutDir); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.VideoRows("", 10)
	if len(rows) != 1 || rows[0].Status != model.StatusUploaded || rows[0].BVID != "BV1test000001" {
		t.Errorf("after upload: %+v", rows)
	}
	args := stubArgs(t)
	if args[0] != a.UploadScript || args[1] != videoPath {
		t.Errorf("argv: %q", args)
	}
	if !hasArg(args, "--title", "Talk Title") || !hasArg(args, "--source", "https://media.ccc.de/v/37c3-1") || !hasArg(args, "--tag", "科技") {
		t.Errorf("argv missing expected flags: %q", args)
	}
	desc := ""
	for i, a2 := range args {
		if a2 == "--desc" && i+1 < len(args) {
			desc = args[i+1]
		}
	}
	wantDesc := "转载自: https://media.ccc.de/v/37c3-1\n演讲者: CCC\n版权归原作者与主办方所有, 本视频为转载\n(含英文字幕)"
	if desc != wantDesc {
		t.Errorf("desc = %q, want %q", desc, wantDesc)
	}
}

func TestUploadNoSubsNoSubsLine(t *testing.T) {
	a, _, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	a.UploadScript = uploadStub(t, "SUBMIT OK — https://www.bilibili.com/video/BV1test000002\n", 0)
	if err := a.Upload(0, false, "cc", "", a.OutDir); err != nil {
		t.Fatal(err)
	}
	args := stubArgs(t)
	desc := ""
	for i, a2 := range args {
		if a2 == "--desc" && i+1 < len(args) {
			desc = args[i+1]
		}
	}
	if strings.Contains(desc, "含英文字幕") {
		t.Errorf("desc claims subs without a sub file: %q", desc)
	}
}

func TestUploadAllowlistGate(t *testing.T) {
	a, st, _ := uploadTestApp(t, model.KindYoutubeChannel, "https://youtu.be/abc", "Chan")
	a.UploadScript = uploadStub(t, "SUBMIT OK — https://www.bilibili.com/video/BV1test000003\n", 0)
	// 'cc' must not allow a youtube source
	if err := a.Upload(0, false, "cc", "", a.OutDir); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.VideoRows("", 10)
	if rows[0].Status != model.StatusDone {
		t.Errorf("skipped video changed state: %+v", rows[0])
	}
	// the numeric source id opens the gate
	if err := a.Upload(0, false, fmt.Sprintf("%d", rows[0].SourceID), "", a.OutDir); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.VideoRows("", 10)
	if rows[0].Status != model.StatusUploaded || rows[0].BVID != "BV1test000003" {
		t.Errorf("id allowlist upload failed: %+v", rows[0])
	}
}

func TestUploadRefusesWithoutAllowlist(t *testing.T) {
	a, _, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	a.UploadScript = uploadStub(t, "", 0)
	if err := a.Upload(0, false, "", "", a.OutDir); err == nil || !strings.Contains(err.Error(), "upload-allowlist") {
		t.Fatalf("err = %v, want allowlist refusal", err)
	}
}

func TestUploadFailureKeepsDone(t *testing.T) {
	a, st, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	a.UploadScript = uploadStub(t, "SUBMIT failed: 601 rate limited", 1)
	if err := a.Upload(0, false, "cc", "", a.OutDir); err != nil {
		t.Fatal(err) // failures are logged, not fatal
	}
	rows, _ := st.VideoRows("", 10)
	if rows[0].Status != model.StatusDone || rows[0].BVID != "" {
		t.Errorf("failed upload changed state: %+v", rows[0])
	}
}

func TestUploadPathPrefixRewrite(t *testing.T) {
	a, st, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	rows, _ := st.VideoRows("", 10)
	local := rows[0].Path
	// simulate the lab→aturing sync: the DB says /home/sjtu/..., the file
	// lives under the local outDir
	if err := st.MarkDownloaded(model.Video{SourceID: rows[0].SourceID, VideoID: "vid1",
		Title: "Talk Title", Published: "2024-01-01", Channel: "CCC", SizeBytes: 4,
		Path: "/home/sjtu/Videos/Crawl/chan/vid1_some_title.mp4", SHA256: "aa"}); err != nil {
		t.Fatal(err)
	}
	a.UploadScript = uploadStub(t, "SUBMIT OK — https://www.bilibili.com/video/BV1test000004\n", 0)
	rewrite := "/home/sjtu/Videos/Crawl:" + filepath.Dir(local)
	if err := a.Upload(0, false, "cc", rewrite, a.OutDir); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.VideoRows("", 10)
	if rows[0].Status != model.StatusUploaded || rows[0].BVID != "BV1test000004" {
		t.Errorf("rewrite upload failed: %+v", rows[0])
	}
	args := stubArgs(t)
	if args[1] != local {
		t.Errorf("uploader got %q, want rewritten %q", args[1], local)
	}
}

func TestUploadFallbackScan(t *testing.T) {
	a, st, _ := uploadTestApp(t, model.KindCCCConf, "https://media.ccc.de/v/37c3-1", "CCC")
	rows, _ := st.VideoRows("", 10)
	local := rows[0].Path
	// stale lab path, no rewrite: the id-prefix scan of outDir must find it
	if err := st.MarkDownloaded(model.Video{SourceID: rows[0].SourceID, VideoID: "vid1",
		Title: "Talk Title", Published: "2024-01-01", Channel: "CCC", SizeBytes: 4,
		Path: "/home/sjtu/Videos/Crawl/chan/vid1_some_title.mp4", SHA256: "aa"}); err != nil {
		t.Fatal(err)
	}
	a.UploadScript = uploadStub(t, "SUBMIT OK — https://www.bilibili.com/video/BV1test000005\n", 0)
	if err := a.Upload(0, false, "cc", "", a.OutDir); err != nil {
		t.Fatal(err)
	}
	args := stubArgs(t)
	if args[1] != local {
		t.Errorf("uploader got %q, want scanned %q", args[1], local)
	}
}

func TestUploadTitleTruncatedTo80(t *testing.T) {
	long := strings.Repeat("很长的标题", 20) // 100 runes
	got := uploadTitle(long)
	if r := []rune(got); len(r) != 80 {
		t.Fatalf("len = %d, want 80", len(r))
	}
	if got != uploadTitle(long) {
		t.Fatal("unstable")
	}
	if uploadTitle("short") != "short" {
		t.Fatal("short titles must pass through")
	}
}
