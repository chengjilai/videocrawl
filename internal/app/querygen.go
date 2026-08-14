// Query generation for the 'discover' command: derive search queries from
// the uploaded-talk corpus (config/semantic-corpus.json — the desired talk
// titles). Pure function over the raw titles, no I/O:
//
//  1. tokenize each title with the corpus tokenizer (the score package's
//     token shape, case-preserving so proper names survive);
//  2. keep rare tokens (df==1) plus any token whose capitalized form
//     appears in a raw title (proper-name detection);
//  3. classify: conference-lexicon -> conference; capitalized -> speaker;
//     remaining df==1 lowercase len>=4, not a micro-stopword, not ending
//     in a year, appearing as a whole word -> topic. Diacritic fragments
//     ("rton" from "Márton", "lois" from "Mélois") never pass the
//     whole-word check and are dropped;
//  4. build budget-capped queries: '<speaker> talk' first, then per
//     conference '<conf>', '<conf> talk', '<conf> <topic>', then
//     '<topic> talk'. User-supplied --query strings are appended.
package app

import (
	"regexp"
	"sort"
	"strings"
	"unicode"

	"videocrawl/internal/score"
)

// conferenceLexicon: conference names whose talks the corpus uploads cover.
// Membership is a keep reason on its own (all-caps conference names like
// SSW/GUADEC/FOSDEM never match the capitalized-form proper-name rule).
var conferenceLexicon = map[string]bool{
	"gophercon": true, "scalacon": true, "kotlinconf": true, "guadec": true,
	"ssw": true, "fosdem": true, "goto": true, "devoxx": true,
	"nixcon": true, "pldi": true, "oplss": true,
}

// microStopwords: the tiny words the score STOPWORDS list misses (it drops
// the classic set only — "up in to of on at" survive its tokenizer). A
// df==1 topic must not be one of these; they also never classify as
// speakers ("Garbage In, ..." and "On the Path..." are capitalization
// accidents of the corpus, not proper names).
var microStopwords = map[string]bool{
	"up": true, "in": true, "to": true, "of": true, "on": true,
	"at": true, "for": true, "from": true, "with": true,
	// title-initial words that look like names but never are (e.g. "My
	// Systemd is your Kubernetes" — "My" must not become a speaker).
	"my": true, "the": true, "a": true, "an": true, "this": true,
	"our": true, "your": true, "is": true, "are": true, "how": true,
	"and": true, "or": true, "we": true, "i": true, "it": true,
}

// yearRe: a token ending in a 4-digit year ("2018", "2025") is a date, not
// a topic.
var yearRe = regexp.MustCompile(`\d{4}$`)

// qTokRe: the corpus tokenizer pattern (the same shape as score.tokenize's
// regexp), case-preserving so proper names can be detected in the raw
// titles. A lowercase pass over the same pattern reproduces score.tokenize
// exactly (len>=2, STOPWORDS dropped).
var qTokRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9+.#_-]*`)

// tokenClass: the querygen classification of a corpus token.
type tokenClass int

const (
	tokDrop tokenClass = iota
	tokConference
	tokSpeaker
	tokTopic
)

// corpusToken: one distinct token of the corpus with its statistics.
type corpusToken struct {
	lower      string // lowercase form
	display    string // first-seen raw form (for output)
	df         int    // document frequency (titles containing it)
	capped     bool   // a capitalized form appears in a raw title
	standalone bool   // appears as a whole word in some title
	firstTitle int    // first title index (stable ordering)
	class      tokenClass
}

// tokenizeCorpus computes the token statistics over the raw titles.
func tokenizeCorpus(titles []string) []corpusToken {
	type stats struct {
		df, first int
		capped    bool
		display   string
	}
	m := map[string]*stats{}
	for ti, t := range titles {
		seen := map[string]bool{}
		for _, tok := range qTokRe.FindAllString(t, -1) {
			low := strings.ToLower(tok)
			if len(low) < 2 || score.STOPWORDS[low] {
				continue
			}
			s := m[low]
			if s == nil {
				s = &stats{display: tok, first: ti}
				m[low] = s
			}
			if tok[0] >= 'A' && tok[0] <= 'Z' {
				s.capped = true
			}
			if !seen[low] {
				seen[low] = true
				s.df++
			}
		}
	}
	out := make([]corpusToken, 0, len(m))
	for low, s := range m {
		out = append(out, corpusToken{
			lower:      low,
			display:    s.display,
			df:         s.df,
			capped:     s.capped,
			standalone: standaloneToken(titles, low),
			firstTitle: s.first,
		})
	}
	for i := range out {
		out[i].class = classifyToken(out[i])
	}
	return out
}

// standaloneToken reports whether low appears in some title as a whole
// word (neither neighbor is a letter). Diacritic fragments like "rton" in
// "Márton" or "lois" in "Mélois" only ever appear glued to a letter, so
// they never pass — they are artifacts of the tokenizer, not topics.
// Searched in rune space: byte indexing would see UTF-8 continuation bytes
// (0xA9 in "é") as non-letters and misjudge the boundaries.
func standaloneToken(titles []string, low string) bool {
	lw := []rune(low)
	for _, t := range titles {
		lr := []rune(strings.ToLower(t))
		for i := 0; i+len(lw) <= len(lr); i++ {
			match := true
			for k, c := range lw {
				if lr[i+k] != c {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			prevOK := i == 0 || !unicode.IsLetter(lr[i-1])
			nextOK := i+len(lw) >= len(lr) || !unicode.IsLetter(lr[i+len(lw)])
			if prevOK && nextOK {
				return true
			}
		}
	}
	return false
}

// classifyToken: conference lexicon -> conference; capitalized (proper
// name) -> speaker; the remainder (df==1, lowercase-only, len>=4, not a
// micro-stopword, not a year, whole word) -> topic. Micro-stopwords drop
// outright: "Garbage In, ..." capitalizes "in" and "On the Path..."
// capitalizes "on", but "in talk"/"on talk" are not discovery queries.
func classifyToken(t corpusToken) tokenClass {
	if conferenceLexicon[t.lower] {
		return tokConference
	}
	if microStopwords[t.lower] {
		return tokDrop
	}
	if t.capped {
		return tokSpeaker
	}
	if t.df == 1 && len(t.lower) >= 4 && !yearRe.MatchString(t.lower) && t.standalone {
		return tokTopic
	}
	return tokDrop
}

// Query caps: total generated queries stay at queryBudget
// (speakerCap + conferenceCap*3 + topicCap) when the corpus is rich.
const (
	queryBudget   = 12 // ~12 yt-dlp searches per run; each costs seconds through the proxy
	speakerCap    = 10
	conferenceCap = 4
	topicCap      = 3
)

// byImportance: df descending (the corpus's recurring entities first),
// then first appearance in the curated corpus, then display form for
// determinism.
func byImportance(a, b corpusToken) bool {
	if a.df != b.df {
		return a.df > b.df
	}
	if a.firstTitle != b.firstTitle {
		return a.firstTitle < b.firstTitle
	}
	return a.display < b.display
}

// GenerateQueries builds the discovery query list from the corpus titles:
// '<speaker> talk' first, then per conference '<conf>', '<conf> talk' and
// '<conf> <topic>' (top topic), then '<topic> talk'. Extra user queries
// are appended (deduplicated, never trimmed by the budget).
func GenerateQueries(corpusTitles []string, extra ...string) []string {
	var confs, spkrs, topics []corpusToken
	for _, t := range tokenizeCorpus(corpusTitles) {
		switch t.class {
		case tokConference:
			confs = append(confs, t)
		case tokSpeaker:
			spkrs = append(spkrs, t)
		case tokTopic:
			topics = append(topics, t)
		}
	}
	sort.Slice(confs, func(i, j int) bool { return byImportance(confs[i], confs[j]) })
	sort.Slice(spkrs, func(i, j int) bool { return byImportance(spkrs[i], spkrs[j]) })
	sort.Slice(topics, func(i, j int) bool { return byImportance(topics[i], topics[j]) })
	if len(confs) > conferenceCap {
		confs = confs[:conferenceCap]
	}
	if len(spkrs) > speakerCap {
		spkrs = spkrs[:speakerCap]
	}
	if len(topics) > topicCap {
		topics = topics[:topicCap]
	}
	var q []string
	for _, s := range spkrs {
		q = append(q, strings.ToLower(s.display)+" talk")
	}
	for _, c := range confs {
		low := strings.ToLower(c.display)
		q = append(q, low, low+" talk")
		if len(topics) > 0 {
			q = append(q, low+" "+topics[0].lower)
		}
	}
	for _, t := range topics {
		q = append(q, t.lower+" talk")
	}
	for _, x := range extra {
		x = strings.TrimSpace(strings.ToLower(x))
		if x == "" {
			continue
		}
		dup := false
		for _, e := range q {
			if e == x {
				dup = true
				break
			}
		}
		if !dup {
			q = append(q, x)
		}
	}
	return q
}
