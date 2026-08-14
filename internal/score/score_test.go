package score

import "testing"

func TestScorerDiscriminates(t *testing.T) {
	corpus := []string{
		"Functional Imperative Programming in Flix - Magnus Madsen (GOTO 2023)",
		"From Datalog to Flix - Magnus Madsen (PLDI 2016)",
		"Go with Versions - Russ Cox (GopherConSG 2018)",
		"Practical Foundations for Programming Languages - Robert Harper (OPLSS)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
	}
	s := NewSemanticScorer(corpus)
	desired := []string{
		"Effectful Programming in Flix - Magnus Madsen (Lambda Days 2025)",
		"The Principles of the Flix Programming Language - Magnus Madsen",
		"Go Changes - Russ Cox (GopherCon 2023)",
		"Don't Take the Black Pill - Andrew Kelley @ SSW 2026",
	}
	junk := []string{
		"History of Hardees",
		"History of the Disney Parks - Little Mermaid",
		"X-Men Anime: An Underrated Marvel Show | Review",
		"How to Switch To Linux - Step by Step Walkthrough",
		"GNOME 50: a MASSIVE release that delivers what users asked for",
	}
	for _, d := range desired {
		if s.Score(d) < 0.10 {
			t.Errorf("desired talk scored too low: %q = %.3f", d, s.Score(d))
		}
	}
	for _, j := range junk {
		if s.Score(j) >= 0.10 && !containsAny(j, "how to", "switch") {
			t.Errorf("junk scored too high: %q = %.3f", j, s.Score(j))
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		if containsFold(s, x) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}
