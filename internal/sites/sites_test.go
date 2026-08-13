package sites

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// usePolicyPath points the package at an arbitrary policy file (tests) and
// restores the default afterwards.
func usePolicyPath(t *testing.T, p string) {
	t.Helper()
	old := policyPath
	policyPath = p
	resetPolicyCache()
	t.Cleanup(func() { policyPath = old; resetPolicyCache() })
}

// writePolicy writes pol to a temp egress-policy.json and points the package
// at it. Returns the file path.
func writePolicy(t *testing.T, pol string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "egress-policy.json")
	if err := os.WriteFile(p, []byte(pol), 0o644); err != nil {
		t.Fatal(err)
	}
	usePolicyPath(t, p)
	return p
}

func TestBlockedPolicy(t *testing.T) {
	writePolicy(t, `{
  "default": "direct",
  "domains": {
    "youtube.com": "warp",
    "bilibili.com": "doh",
    "github.com": "warp",
    "api.github.com": "direct",
    "archive.org": "warp"
  }
}`)
	cases := []struct {
		host string
		want bool
	}{
		{"youtube.com", true},          // warp
		{"www.youtube.com", true},      // warp, subdomain
		{"bilibili.com", true},         // doh
		{"api.bilibili.com", true},     // doh, subdomain
		{"github.com", true},           // warp
		{"api.github.com", false},      // most specific suffix: direct wins
		{"deep.api.github.com", false}, // direct wins over github.com warp
		{"archive.org", true},          // warp
		{"media.ccc.de", false},        // unlisted, default direct
		{"example.com", false},         // unlisted, default direct
		{"YOUTUBE.COM", true},          // case-insensitive
	}
	for _, c := range cases {
		if got := Blocked(c.host); got != c.want {
			t.Errorf("Blocked(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	acts := PolicyActions()
	if acts["youtube.com"] != "warp" || acts["bilibili.com"] != "doh" || acts["api.github.com"] != "direct" {
		t.Errorf("PolicyActions() = %v", acts)
	}
	if _, ok := acts["nope.com"]; ok {
		t.Errorf("PolicyActions() contains unlisted domain: %v", acts)
	}
}

func TestBlockedPolicyDefault(t *testing.T) {
	writePolicy(t, `{"default": "warp", "domains": {"example.com": "direct"}}`)
	if !Blocked("unlisted.org") {
		t.Error("Blocked(unlisted.org) = false, want true (default warp)")
	}
	if Blocked("example.com") {
		t.Error("Blocked(example.com) = true, want false (direct)")
	}
}

func TestBlockedFallback(t *testing.T) {
	// Unreadable path → baked list.
	usePolicyPath(t, filepath.Join(t.TempDir(), "missing.json"))
	for _, h := range []string{"youtube.com", "www.youtube.com", "media.ccc.de", "github.com", "archive.org"} {
		if !Blocked(h) {
			t.Errorf("Blocked(%q) = false, want true (baked fallback)", h)
		}
	}
	if Blocked("example.com") {
		t.Error("Blocked(example.com) = true, want false (unlisted)")
	}
	if acts := PolicyActions(); acts["youtube.com"] != "warp" || len(acts) != len(bakedBlocked) {
		t.Errorf("PolicyActions() fallback = %v", acts)
	}

	// A previously parsed policy is dropped once the file disappears:
	// fallback (baked) takes over, not the stale policy.
	writePolicy(t, `{"default": "warp", "domains": {}}`)
	if !Blocked("example.com") {
		t.Fatal("policy default warp should block example.com")
	}
	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	resetPolicyCache()
	if Blocked("example.com") {
		t.Error("after removal, baked fallback should not block unlisted hosts")
	}
}

func TestBlockedPolicyReload(t *testing.T) {
	p := writePolicy(t, `{"default": "direct", "domains": {"a.example": "warp"}}`)
	if !Blocked("a.example") {
		t.Fatal("initial policy should block a.example")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite, then restore the original mtime: cache must serve old content.
	if err := os.WriteFile(p, []byte(`{"default": "direct", "domains": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time.Now(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if !Blocked("a.example") {
		t.Error("same mtime should serve the cached policy")
	}
	// Bumped mtime: policy re-read.
	if err := os.Chtimes(p, time.Now(), fi.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if Blocked("a.example") {
		t.Error("mtime bump should re-read the new policy")
	}
}
