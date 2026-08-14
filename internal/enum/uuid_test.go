package enum

import (
	"strings"
	"testing"
)

func TestUUIDv4(t *testing.T) {
	a := uuidv4()
	// shape: 8-4-4-4-12, all hex
	if len(a) != 36 {
		t.Fatalf("len=%d: %q", len(a), a)
	}
	parts := strings.Split(a, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 ||
		len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("bad grouping: %q", a)
	}
	for _, r := range a {
		if r != '-' && !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("non-hex char %q in %q", r, a)
		}
	}
	// version nibble must be 4, variant nibble must be 8/9/a/b
	if parts[2][0] != '4' {
		t.Errorf("version nibble = %q, want 4", parts[2][0])
	}
	if v := parts[3][0]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("variant nibble = %q, want 8/9/a/b", v)
	}

	// uniqueness sanity across the two calls the enumerator makes
	b := uuidv4()
	if a == b {
		t.Errorf("two UUIDv4 calls returned the same value %q", a)
	}
	// buvid3 keeps the yt-dlp "<uuid>infoc" cookie format
	if got := biliBuvid3(); !strings.HasSuffix(got, "infoc") || len(got) != 41 {
		t.Errorf("buvid3 shape wrong: %q", got)
	}
}
