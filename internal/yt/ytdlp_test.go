package yt

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunWithRetryRetriesThenSucceeds: a command that fails twice then
// succeeds must be retried (sleeps shortened for the test).
func TestRunWithRetryRetriesThenSucceeds(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo attempt >>/tmp/yt-retry-test; test $(wc -l < /tmp/yt-retry-test) -ge 3")
	cmd.Args = []string{"sh", "-c", "echo attempt >>/tmp/yt-retry-test; test $(wc -l < /tmp/yt-retry-test) -ge 3"}
	_ = exec.Command("rm", "-f", "/tmp/yt-retry-test").Run()
	err := RunWithRetry(context.Background(), cmd, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("want success after retries, got %v", err)
	}
}

// TestRunWithRetryGivesUp: a persistently failing command returns the last
// error after exhausting the sleeps (3 attempts with 2 sleeps).
func TestRunWithRetryGivesUp(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo boom >&2; exit 7")
	err := RunWithRetry(context.Background(), cmd, time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("want error after all attempts")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want the captured stderr message, got %v", err)
	}
}

// TestRunWithRetryCtxCancel: cancellation between attempts aborts early
// with ctx.Err.
func TestRunWithRetryCtxCancel(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo boom >&2; exit 7")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	// sleeps long enough that cancellation wins between attempts
	err := RunWithRetry(ctx, cmd, time.Hour, time.Hour)
	if err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestSubtitleCmdArgs: the sub-only pass must carry --skip-download and
// the subtitle flags, and nothing else surprising.
func TestSubtitleCmdArgs(t *testing.T) {
	cmd := SubtitleCmd("", "", "/tmp/out", "https://youtu.be/abc123")
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--skip-download", "--write-subs", "--write-auto-subs",
		"--sub-langs", "en,en-orig", "--sub-format", "srt/best", "/tmp/out/.tmp"} {
		if !strings.Contains(joined, want) {
			t.Errorf("SubtitleCmd missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "--concurrent-fragments") {
		t.Errorf("SubtitleCmd should not carry video flags: %s", joined)
	}
	// the en.* regex fans out into ~11 translated tracks and 429s; the pass
	// must request exactly the English pair
	if strings.Contains(joined, "en.*") {
		t.Errorf("SubtitleCmd must not use the en.* fan-out regex: %s", joined)
	}
}
