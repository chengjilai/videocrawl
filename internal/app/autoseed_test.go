package app

import "testing"

func TestAutoSeedDue(t *testing.T) {
	cases := []struct {
		round, every int
		want         bool
	}{
		{1, 24, true}, {24, 24, false}, {25, 24, true}, {49, 24, true},
		{1, 1, true}, {2, 1, true},
		{1, 0, false}, {5, 0, false},
	}
	for _, c := range cases {
		if got := autoSeedDue(c.round, c.every); got != c.want {
			t.Errorf("autoSeedDue(%d,%d)=%v want %v", c.round, c.every, got, c.want)
		}
	}
}
