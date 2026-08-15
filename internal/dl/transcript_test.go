package dl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"videocrawl/internal/model"
	"videocrawl/internal/score"
	"videocrawl/internal/store"
)

func writeSub(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTranscriptTextSRT: cue indices, timestamps and HTML tags are stripped;
// the longest id-prefixed sub wins (manual .en.srt beats auto .en-orig.srt
// when it is longer).
func TestTranscriptTextSRT(t *testing.T) {
	dir := t.TempDir()
	ch := filepath.Join(dir, "Some_Channel")
	if err := os.MkdirAll(ch, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSub(t, ch, "abc123_Short_auto.en-orig.srt", "1\n00:00:01,000 --> 00:00:02,500\nonly this line\n")
	writeSub(t, ch, "abc123_Long_manual.en.srt",
		"1\n00:00:01,000 --> 00:00:02,500\nHello <i>world</i> from the manual sub\n\n"+
			"2\n00:00:03,000 --> 00:00:04,000\nSecond cue, with <font color=\"#fff\">markup</font>\n")

	got := transcriptText(dir, "abc123")
	want := "Hello world from the manual sub Second cue, with markup"
	if got != want {
		t.Fatalf("transcriptText = %q, want %q", got, want)
	}
}

// TestTranscriptTextVTT: WEBVTT header, Kind/Language fields, NOTE blocks,
// timestamps (dot ms + cue settings) are dropped.
func TestTranscriptTextVTT(t *testing.T) {
	dir := t.TempDir()
	ch := filepath.Join(dir, "Chan")
	if err := os.MkdirAll(ch, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSub(t, ch, "v9_Some_Talk.en.vtt",
		"WEBVTT\nKind: captions\nLanguage: en\n\nNOTE\nThis is a comment\nthat spans two lines\n\n"+
			"00:00:01.000 --> 00:00:02.500 align:start position:0%\nFirst <i>cue</i>\n\n"+
			"STYLE\n::cue {\n  color: yellow;\n}\n\n"+
			"00:00:03 --> 00:00:04\nSecond cue\n")
	got := transcriptText(dir, "v9")
	want := "First cue Second cue"
	if got != want {
		t.Fatalf("transcriptText = %q, want %q", got, want)
	}
}

// TestTranscriptTextASS: only the Dialogue payload text is kept; section
// headers, Format/Style lines and override tags are dropped.
func TestTranscriptTextASS(t *testing.T) {
	dir := t.TempDir()
	ch := filepath.Join(dir, "Chan")
	if err := os.MkdirAll(ch, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSub(t, ch, "a1_Sub.ass",
		"[Script Info]\nTitle: x\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"+
			"Dialogue: 0,0:00:00.00,0:00:02.00,Default,,0,0,0,,Hello {\\an8}world\\Nsecond line\n"+
			"Style: Default,Arial,20,&H00FFFFFF\n"+
			"Dialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,bye, and comma\n")
	got := transcriptText(dir, "a1")
	want := "Hello world second line bye, and comma"
	if got != want {
		t.Fatalf("transcriptText = %q, want %q", got, want)
	}
}

// TestTranscriptTextTmpDir: yt-dlp stages subs in <outDir>/.tmp before
// moving them next to the video; a sub found there is used too.
func TestTranscriptTextTmpDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSub(t, filepath.Join(dir, ".tmp"), "zz9_Still_Staging.en.srt",
		"1\n00:00:01,000 --> 00:00:02,000\nstaged transcript line\n")
	got := transcriptText(dir, "zz9")
	if !strings.Contains(got, "staged transcript line") {
		t.Fatalf("transcriptText = %q, want staged line", got)
	}
}

// TestTranscriptTextMissing: no sub on disk -> "" (the gate's lenient pass).
func TestTranscriptTextMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Chan"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a same-prefix mp4 must not be mistaken for a transcript
	writeSub(t, filepath.Join(dir, "Chan"), "x1_Video.mp4", "not a transcript")
	if got := transcriptText(dir, "x1"); got != "" {
		t.Fatalf("transcriptText = %q, want \"\"", got)
	}
}

// TestGateTranscript: the relevance gate — scorer off = pass, missing
// transcript = pass (score 0), below threshold = skipped "transcript",
// at/above threshold = pass with the score recorded.
func TestGateTranscript(t *testing.T) {
	dir := t.TempDir()
	ch := filepath.Join(dir, "Chan")
	if err := os.MkdirAll(ch, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSub(t, ch, "abc_On_Topic.en.srt",
		"1\n00:00:01,000 --> 00:00:02,000\nGo with Versions and reliability lessons from SQLite\n")
	writeSub(t, ch, "zzz_Off_Topic.en.srt",
		"1\n00:00:01,000 --> 00:00:02,000\ncooking pasta with tomatoes basil garlic and wine\n")

	corpus := []string{"Go with Versions - Russ Cox (GopherCon 2018)", "Reliability Lessons From SQLite - Richard Hipp"}

	// scorer off (no corpus): everything passes with score 0
	p := &Pool{outDir: dir, transcriptThreshold: 0.15}
	if sc, err := p.gateTranscript(model.Video{VideoID: "zzz"}); err != nil || sc != 0 {
		t.Fatalf("no scorer: sc=%v err=%v, want 0/nil", sc, err)
	}

	// scorer on + strict threshold: any transcript skips with reason "transcript"
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertVideo(model.Video{SourceID: 1, VideoID: "zzz", URL: "u/zzz"}); err != nil {
		t.Fatal(err)
	}
	p = &Pool{store: st, outDir: dir, scorer: score.NewSemanticScorer(corpus), transcriptThreshold: 0.99}
	if _, err := p.gateTranscript(model.Video{SourceID: 1, VideoID: "zzz"}); err != errSkipped {
		t.Fatalf("below threshold: err=%v, want errSkipped", err)
	}
	rows, _ := st.VideoRows(model.StatusSkipped, 10)
	if len(rows) != 1 || rows[0].LastError != "transcript" {
		t.Fatalf("skipped row: %+v", rows)
	}

	// missing transcript = pass even below threshold (lenient)
	if sc, err := p.gateTranscript(model.Video{SourceID: 1, VideoID: "missing"}); err != nil || sc != 0 {
		t.Fatalf("missing transcript: sc=%v err=%v, want 0/nil", sc, err)
	}

	// normal threshold: on-topic passes with score, off-topic skips
	p.transcriptThreshold = 0.15
	sc, err := p.gateTranscript(model.Video{SourceID: 1, VideoID: "abc"})
	if err != nil || sc < 0.15 {
		t.Fatalf("on-topic: sc=%v err=%v, want sc>=0.15 nil", sc, err)
	}
	if _, err := p.gateTranscript(model.Video{SourceID: 1, VideoID: "zzz"}); err != errSkipped {
		t.Fatalf("off-topic: err=%v, want errSkipped", err)
	}
}

// TestTranscriptThresholdEnv: VIDEOCRAWL_TRANSCRIPT_THRESHOLD overrides the
// 0.15 default.
func TestTranscriptThresholdEnv(t *testing.T) {
	if got := TranscriptThreshold(); got != 0.15 {
		t.Fatalf("default threshold = %v, want 0.15", got)
	}
	t.Setenv("VIDEOCRAWL_TRANSCRIPT_THRESHOLD", "0.42")
	if got := TranscriptThreshold(); got != 0.42 {
		t.Fatalf("env threshold = %v, want 0.42", got)
	}
	t.Setenv("VIDEOCRAWL_TRANSCRIPT_THRESHOLD", "junk")
	if got := TranscriptThreshold(); got != 0.15 {
		t.Fatalf("bad env threshold = %v, want fallback 0.15", got)
	}
}
