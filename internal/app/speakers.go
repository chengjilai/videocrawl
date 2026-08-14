// Speaker names for the discover SPEAKER BOOST: corpus tokens whose
// capitalized form appears in a raw title exactly once (df==1 proper-name
// heuristic — single-appearance capitalized words in the curated corpus are
// overwhelmingly talk speakers like Vincent, Ambo, Molodetskikh), minus
// conference-lexicon tokens. Micro-stopwords can never be names.
package app

import (
	"sort"
	"strings"
)

// SpeakerNames extracts corpus speaker FULL NAMES from the credits after
// separators ("Talk Title - First Last", "Title | First Last", "... by
// First Last"): the speaker credit in conference titles is the segment
// after '-', '|', '—', '@', 'by' or 'with'. Title-START pairs are NOT
// names ("Go Changes", "My Systemd", "Practical Foundations" are title
// words, not people). Surname-only matches are not boosted either ("Cox"
// alone matches Heather Cox Richardson).
func SpeakerNames(corpusTitles []string) []string {
	names := map[string]string{}
	for _, t := range corpusTitles {
		for _, seg := range splitCredits(t) {
			toks := tokenizeCorpus([]string{seg})
			for i := 0; i+1 < len(toks); i++ {
				a, b := toks[i], toks[i+1]
				if !a.capped || !b.capped || a.lower == b.lower {
					continue
				}
				if microStopwords[a.lower] || microStopwords[b.lower] ||
					conferenceLexicon[a.lower] || conferenceLexicon[b.lower] {
					continue
				}
				la, lb := len(a.lower), len(b.lower)
				if la >= 2 && la <= 10 && lb >= 2 && lb <= 10 {
					names[a.lower+" "+b.lower] = a.display + " " + b.display
				}
			}
		}
	}
	out := make([]string, 0, len(names))
	for _, disp := range names {
		out = append(out, disp)
	}
	sort.Strings(out)
	return out
}

// splitCredits splits a title into the segment after each credit separator
// ('-', '|', '—', '@', 'by', 'with') plus the segment before the first.
func splitCredits(t string) []string {
	repl := strings.NewReplacer("-", " | ", "—", " | ", "|", " | ", "@", " | ", " by ", " | ", " with ", " | ")
	parts := strings.Split(repl.Replace(t), " | ")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && i > 0 {
			out = append(out, p)
		}
	}
	return out
}

func matchSpeaker(text string, speakers []string) string {
	low := strings.ToLower(text)
	for _, s := range speakers {
		if wordContains(low, strings.ToLower(s)) {
			return s
		}
	}
	return ""
}

// wordContains reports whether needle occurs in haystack (both lowercase)
// bounded by non-word characters (ASCII letters/digits are word chars;
// UTF-8 continuation bytes are >= 0x80 and never count as word chars).
func wordContains(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	n := len(needle)
	for i := 0; i+n <= len(haystack); i++ {
		if haystack[i:i+n] != needle {
			continue
		}
		prevOK := i == 0 || !isWordByte(haystack[i-1])
		nextOK := i+n >= len(haystack) || !isWordByte(haystack[i+n])
		if prevOK && nextOK {
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
