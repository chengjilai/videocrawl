package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"videocrawl/internal/model"
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
		{"hello", 10, "hello"},       // n > len: unchanged
		{"hello world", 5, "hell…"},  // n-1 runes + ellipsis
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
