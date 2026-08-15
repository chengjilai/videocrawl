package main

import "testing"

func TestReorderArgsDashDash(t *testing.T) {
	got := reorderArgs([]string{"set-topics", "22", "--", "-interview,-panel,-nosql"}, map[string]bool{})
	want := []string{"--", "set-topics", "22", "-interview,-panel,-nosql"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// regular flag+positional mixing still works
	got = reorderArgs([]string{"add", "kind", "url", "--name", "X"}, map[string]bool{"name": false})
	if got[0] != "--name" || got[3] != "kind" {
		t.Fatalf("flag reorder broken: %v", got)
	}
}
