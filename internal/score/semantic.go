// TF-IDF semantic scorer, ported from techcrawl-go/internal/score
// (which was itself ported from the Python techcrawl semantic.py).
//
// The reference corpus is the UPLOAD HISTORY of desired talks
// (config/semantic-corpus.json) — a candidate scores by cosine similarity
// of its TF-IDF vector against the corpus centroid: "more like what we
// already uploaded". Titles are sparse, so the corpus entries carry the
// full talk titles (incl. speaker + conference), and candidates are scored
// on title+channel.
package score

import (
	"math"
	"regexp"
	"strings"
)

var tokenRe = regexp.MustCompile(`[a-z0-9][a-z0-9+.#_-]*`)

// Tokenize splits text into corpus tokens: lowercase runs of
// [a-z0-9+.#_-] at least 2 chars long, with the score STOPWORDS dropped.
// This is the tokenizer NewSemanticScorer builds its vocabulary from, and
// the token shape discover's jaccard dedup shares (discover.tokenSet
// reproduces this function exactly).
func Tokenize(text string) []string {
	low := strings.ToLower(text)
	toks := tokenRe.FindAllString(low, -1)
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if !STOPWORDS[t] && len(t) >= 2 {
			out = append(out, t)
		}
	}
	return out
}

// SemanticScorer computes cosine similarity of a text against the exemplar
// centroid.
type SemanticScorer struct {
	idf      map[string]float64
	centroid map[string]float64
}

// NewSemanticScorer builds the scorer over the given reference corpus
// (talk titles — see config/semantic-corpus.json).
func NewSemanticScorer(corpus []string) *SemanticScorer {
	s := &SemanticScorer{
		idf:      make(map[string]float64),
		centroid: make(map[string]float64),
	}
	if len(corpus) == 0 {
		return s
	}
	// idf over the exemplar corpus (each exemplar contributes its token SET)
	df := map[string]int{}
	for _, ex := range corpus {
		seen := map[string]bool{}
		for _, tok := range Tokenize(ex) {
			if !seen[tok] {
				seen[tok] = true
				df[tok]++
			}
		}
	}
	n := float64(len(corpus))
	for tok, f := range df {
		s.idf[tok] = math.Log((1+n)/(1+float64(f))) + 1
	}
	// centroid = normalized mean of the exemplar vectors
	for _, ex := range corpus {
		for tok, w := range s.vector(ex) {
			s.centroid[tok] += w
		}
	}
	norm := 0.0
	for _, w := range s.centroid {
		norm += w * w
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for tok, w := range s.centroid {
		s.centroid[tok] = w / norm
	}
	return s
}

func (s *SemanticScorer) vector(text string) map[string]float64 {
	counts := map[string]int{}
	for _, tok := range Tokenize(text) {
		counts[tok]++
	}
	vec := map[string]float64{}
	for tok, c := range counts {
		idf, ok := s.idf[tok]
		if !ok {
			continue
		}
		vec[tok] = (1 + math.Log(float64(c))) * idf
	}
	norm := 0.0
	for _, w := range vec {
		norm += w * w
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for tok, w := range vec {
		vec[tok] = w / norm
	}
	return vec
}

// Score returns the cosine similarity of text against the exemplar centroid.
//
// Computed in one pass over the token stream (vs vector()): tokens that have
// no idf are skipped during counting, the weighted map is normalized in
// place, and the dot product accumulates directly — same arithmetic, far
// fewer intermediate allocations. Tokenization is a hand-rolled scanner
// (the regexp engine dominated the per-call cost).
func (s *SemanticScorer) Score(text string) float64 {
	low := strings.ToLower(text)
	weighted := make(map[string]float64, 64)
	n := len(low)
	for i := 0; i < n; {
		c := low[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			i++
			continue
		}
		j := i + 1
		for j < n {
			c = low[j]
			if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
				c == '+' || c == '.' || c == '#' || c == '_' || c == '-' {
				j++
			} else {
				break
			}
		}
		tok := low[i:j]
		i = j
		if len(tok) < 2 || STOPWORDS[tok] {
			continue
		}
		if _, ok := s.idf[tok]; !ok {
			continue
		}
		weighted[tok]++
	}
	// counts -> (1+log c)*idf weights
	norm := 0.0
	for tok, c := range weighted {
		w := (1 + math.Log(c)) * s.idf[tok]
		weighted[tok] = w
		norm += w * w
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	dot := 0.0
	for tok, w := range weighted {
		dot += (w / norm) * s.centroid[tok]
	}
	return dot
}
