package yt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubScript writes an executable stand-in for yt-dlp that echoes the given
// lines to stdout (stderr lines via the stderr prefix, when non-empty) and
// exits with code. Sets VIDEOCRAWL_YTDLP so Bin() resolves to it.
func stubScript(t *testing.T, stdout []string, stderr string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yt-dlp-stub")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if stderr != "" {
		fmt.Fprintf(&b, "echo '%s' >&2\n", stderr)
	}
	for _, l := range stdout {
		fmt.Fprintf(&b, "echo '%s'\n", l)
	}
	if code != 0 {
		fmt.Fprintf(&b, "exit %d\n", code)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIDEOCRAWL_YTDLP", p)
	return p
}

func TestParseUploadDate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"20240301", "2024-03-01"},
		{"20240229", "2024-02-29"}, // leap year
		{"20241231", "2024-12-31"},
		{"", ""},
		{"2024031", ""},    // too short
		{"202403011", ""},  // too long
		{"2024-03-01", ""}, // wrong layout
		{"20240230", ""},   // invalid day
		{"20241301", ""},   // invalid month
		{"abcdefgh", ""},   // garbage
	}
	for _, c := range cases {
		if got := ParseUploadDate(c.in); got != c.want {
			t.Errorf("ParseUploadDate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDurationSeconds(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{float64(123.9), 123}, // truncated, not rounded
		{float64(0), 0},
		{float64(90), 90},
		{int64(42), 42},
		{int(42), 42},
		{nil, 0},
		{"42", 0},   // json string durations aren't handled
		{true, 0},   // default branch
		{[]int{1}, 0},
	}
	for _, c := range cases {
		if got := DurationSeconds(c.in); got != c.want {
			t.Errorf("DurationSeconds(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestEnumFakeExec: yt-dlp is replaced by a stub printing canned JSON-lines;
// the malformed line and the trailer are skipped, entries stream through the
// callback, and exit 0 means "complete".
func TestEnumFakeExec(t *testing.T) {
	lines := []string{
		`{"id":"v1","title":"First","duration":123.5,"upload_date":"20240301","url":"https://youtu.be/v1","channel":"Ch"}`,
		`[youtube] playlist trailer line, not JSON`,
		`{"id":"v2","title":"Second","duration":60}`,
		`{"title":"no-id-line-should-be-skipped"}`,
		``, // blank line
		`{"id":"v3","title":"Third","live_status":"is_live"}`,
	}
	stubScript(t, lines, "", 0)

	var got []FlatEntry
	n, complete, err := Enum(nil, "", "", "https://example.com/list", func(e FlatEntry) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("Enum: %v", err)
	}
	if !complete {
		t.Error("want complete=true on exit 0")
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}
	if len(got) != 3 {
		t.Fatalf("callback saw %d entries, want 3", len(got))
	}
	if got[0].ID != "v1" || got[1].ID != "v2" || got[2].ID != "v3" {
		t.Fatalf("ids = %q %q %q", got[0].ID, got[1].ID, got[2].ID)
	}
	if got[0].Title != "First" || got[0].Channel != "Ch" {
		t.Errorf("v1 fields: %+v", got[0])
	}
	if d, ok := got[0].Duration.(float64); !ok || d != 123.5 {
		t.Errorf("v1 duration = %#v (%T), want float64 123.5", got[0].Duration, got[0].Duration)
	}
	if got[0].UploadDate != "20240301" {
		t.Errorf("v1 upload_date = %q", got[0].UploadDate)
	}
	if got[2].LiveStatus != "is_live" {
		t.Errorf("v3 live_status = %q", got[2].LiveStatus)
	}
	// v2 (no url/upload_date) still passes through
	if got[1].ID != "v2" || got[1].URL != "" {
		t.Errorf("v2 fields: %+v", got[1])
	}
}

// TestEnumCallbackErrorAborts: a failing callback stops the stream, reports
// the error and the count seen so far.
func TestEnumCallbackErrorAborts(t *testing.T) {
	lines := []string{
		`{"id":"v1"}`,
		`{"id":"v2"}`,
		`{"id":"v3"}`,
	}
	stubScript(t, lines, "", 0)
	sentinel := errors.New("stop at v2")
	n, _, err := Enum(nil, "", "", "u", func(e FlatEntry) error {
		if e.ID == "v2" {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if n != 1 {
		t.Fatalf("n = %d, want 1 (stopped before v2)", n)
	}
}

// TestEnumNonzeroExit: a failing yt-dlp surfaces its stderr as the error.
func TestEnumNonzeroExit(t *testing.T) {
	stubScript(t, []string{`{"id":"v1"}`}, "Request is blocked (412)", 4)
	n, complete, err := Enum(nil, "", "", "u", func(FlatEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Request is blocked (412)") {
		t.Fatalf("err = %v, want stderr 'Request is blocked (412)'", err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1 (entries before failure counted)", n)
	}
	if complete {
		t.Error("complete should be false on failure")
	}
}

// TestEnumNonzeroExitNoStderr: stderr empty → falls back to exit-status text.
func TestEnumNonzeroExitNoStderr(t *testing.T) {
	stubScript(t, []string{}, "", 3)
	_, _, err := Enum(nil, "", "", "u", func(FlatEntry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want 'exit status 3'", err)
	}
}

// TestGetMetaFakeExec: full-metadata call through the stub.
func TestGetMetaFakeExec(t *testing.T) {
	stubScript(t, []string{
		`{"id":"x","title":"Talk","duration":90,"channel":"C","upload_date":"20240101","webpage_url":"https://youtu.be/x","live_status":"not_live"}`,
	}, "", 0)
	m, err := GetMeta(nil, "", "", "https://youtu.be/x")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "x" || m.Title != "Talk" || m.Channel != "C" ||
		m.UploadDate != "20240101" || m.URL != "https://youtu.be/x" || m.LiveStatus != "not_live" {
		t.Errorf("meta: %+v", m)
	}
	if d, ok := m.Duration.(float64); !ok || d != 90 {
		t.Errorf("duration = %#v (%T), want float64 90", m.Duration, m.Duration)
	}
}

func TestGetMetaParseError(t *testing.T) {
	stubScript(t, []string{"this is not json"}, "", 0)
	if _, err := GetMeta(nil, "", "", "u"); err == nil || !strings.Contains(err.Error(), "parse meta") {
		t.Fatalf("err = %v, want 'parse meta'", err)
	}
}

func TestGetMetaFailure(t *testing.T) {
	stubScript(t, []string{}, "not found", 1)
	if _, err := GetMeta(nil, "", "", "u"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want stderr 'not found'", err)
	}
}

// TestEnumCmdBuild: the command shape (proxy/cookies wiring) without running it.
func TestEnumCmdBuild(t *testing.T) {
	t.Setenv("VIDEOCRAWL_YTDLP", "/nonexistent/yt-dlp")
	cmd := EnumCmd([]string{"--extractor-args", "x"}, "cookies.txt", "http://127.0.0.1:8888")
	args := cmd.Args
	joined := strings.Join(args, " ")
	for _, want := range []string{"--flat-playlist", "-j", "--cookies", "cookies.txt", "--proxy", "http://127.0.0.1:8888", "--extractor-args", "x", "--"} {
		if !strings.Contains(joined, want) {
			t.Errorf("EnumCmd args missing %q: %v", want, args)
		}
	}
}
