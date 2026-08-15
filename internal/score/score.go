// Package score: semantic relevance scoring against a reference corpus.
// The TF-IDF centroid scorer (semantic.go) is the always-available
// implementation; the embedding scorer (embed.go, VIDEOCRAWL_EMBED_URL)
// calibrates a local embedding server's cosines onto the same scale and
// falls back to TF-IDF when the server is unreachable.
package score

import "os"

// Scorer scores text against a reference corpus. Implementations: the
// TF-IDF centroid scorer (default, always available) and the embedding
// scorer (VIDEOCRAWL_EMBED_URL) which falls back to TF-IDF when the
// server is unreachable — the gates must never hard-fail on it.
type Scorer interface {
	Score(text string) float64
}

// BatchScorer is optional (type-assert): batch scoring to amortize
// per-text HTTP round trips.
type BatchScorer interface {
	BatchScore(texts []string) []float64
}

// New returns the configured scorer: VIDEOCRAWL_EMBED_URL set and
// reachable → embedding-backed (calibrated onto the TF-IDF scale, TF-IDF
// fallback on any error); otherwise the plain TF-IDF scorer. Never nil.
func New(corpus []string) Scorer {
	url := os.Getenv("VIDEOCRAWL_EMBED_URL")
	if url == "" {
		return NewSemanticScorer(corpus)
	}
	return newEmbeddingScorer(url, corpus)
}
