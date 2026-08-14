package politeness

import (
	"testing"
	"time"
)

func hostGap(l *Limiter, key string) time.Duration {
	h, ok := l.hosts[key]
	if !ok {
		return 0
	}
	return h.gap
}

func TestNoteErrorDoublesGap(t *testing.T) {
	l := New(100 * time.Millisecond)
	l.NoteError("h")
	if g := hostGap(l, "h"); g != 200*time.Millisecond {
		t.Fatalf("gap = %v, want 200ms", g)
	}
	l.NoteError("h")
	if g := hostGap(l, "h"); g != 400*time.Millisecond {
		t.Fatalf("gap = %v, want 400ms", g)
	}
}

func TestNoteErrorCapsAtMax(t *testing.T) {
	l := New(10 * time.Millisecond)
	for i := 0; i < 20; i++ {
		l.NoteError("h")
	}
	want := 10 * time.Millisecond * 64
	if g := hostGap(l, "h"); g != want {
		t.Fatalf("gap = %v, want cap %v", g, want)
	}
}

func TestNoteSuccessDecaysToFloor(t *testing.T) {
	l := New(100 * time.Millisecond)
	l.NoteError("h") // 200ms
	l.NoteSuccess("h")
	if g := hostGap(l, "h"); g != 180*time.Millisecond { // ×0.9
		t.Fatalf("gap = %v, want 180ms", g)
	}
	for i := 0; i < 100; i++ {
		l.NoteSuccess("h")
	}
	if g := hostGap(l, "h"); g != 100*time.Millisecond {
		t.Fatalf("gap = %v, want floor 100ms", g)
	}
	// successes at the floor are a no-op (no underflow)
	l.NoteSuccess("h")
	if g := hostGap(l, "h"); g != 100*time.Millisecond {
		t.Fatalf("floor broken: %v", g)
	}
}

func TestWaitFirstCallDoesNotSleep(t *testing.T) {
	l := New(10 * time.Second) // huge gap
	start := time.Now()
	l.Wait("h")
	if e := time.Since(start); e > 100*time.Millisecond {
		t.Fatalf("first Wait slept %v", e)
	}
}

func TestWaitBlocksAtLeastGap(t *testing.T) {
	l := New(60 * time.Millisecond)
	start := time.Now()
	l.Wait("h") // first: records time, no sleep
	l.Wait("h") // second: must sleep the gap
	elapsed := time.Since(start)
	if elapsed < 45*time.Millisecond {
		t.Fatalf("Wait returned after %v, want >= ~60ms", elapsed)
	}
}

func TestWaitAfterErrorUsesDoubledGap(t *testing.T) {
	l := New(30 * time.Millisecond)
	l.Wait("h")      // t0
	l.NoteError("h") // gap → 60ms
	start := time.Now()
	l.Wait("h")
	if e := time.Since(start); e < 50*time.Millisecond {
		t.Fatalf("Wait after error slept %v, want >= ~60ms", e)
	}
}

func TestHostsAreIndependent(t *testing.T) {
	l := New(100 * time.Millisecond)
	l.NoteError("a")
	l.NoteSuccess("b") // b untouched: still at min
	if g := hostGap(l, "a"); g != 200*time.Millisecond {
		t.Fatalf("a gap = %v", g)
	}
	if g := hostGap(l, "b"); g != 100*time.Millisecond {
		t.Fatalf("b gap = %v, want 100ms", g)
	}
	if g := hostGap(l, "c"); g != 0 {
		t.Fatalf("unknown host gap = %v, want 0", g)
	}
}
