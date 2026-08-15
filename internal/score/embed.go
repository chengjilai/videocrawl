// The embedding-backed scorer: semantic scoring delegated to a local
// embedding server (VIDEOCRAWL_EMBED_URL — the lab's
// Qwen3-Embedding-0.6B endpoint, ~0.07s per 40-target call). Corpus
// scores are cosine means calibrated onto the TF-IDF scale (least squares
// fit of the corpus self-scores), so the existing thresholds keep their
// meaning. ANY failure — construction (server unreachable, HTTP != 200,
// bad response shape) or per-call — falls back to the plain TF-IDF
// scorer; the gates must never hard-fail on the embedding backend.
package score

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// EmbeddingScorer implements Scorer and BatchScorer on top of the local
// embedding server. Safe for concurrent use (the crawl-loop shares one
// instance across workers/gates): the mutable warm state is mutex-guarded.
type EmbeddingScorer struct {
	url    string
	corpus []string

	inner *SemanticScorer // TF-IDF scorer: calibration reference + fallback
	a, b  float64         // calibration: tfidf ≈ a*embedding + b

	client *http.Client // 30s timeout; connection reuse

	mu       sync.Mutex  // guards targets/disabled/lastWarn (shared across workers)
	targets  [][]float64 // embedded corpus targets — the warm call's self-similarity matrix
	disabled bool        // construction failed → permanent TF-IDF fallback
	lastWarn time.Time   // per-call error log throttle (once per minute)
}

// newEmbeddingScorer builds the scorer and warms it: one corpus-vs-corpus
// round trip fits the TF-IDF calibration. On ANY warm error the scorer is
// permanently disabled and delegates everything to the TF-IDF inner
// scorer (the "embeddings: disabled: ..." line is printed once to stderr).
// Never returns nil.
func newEmbeddingScorer(url string, corpus []string) Scorer {
	s := &EmbeddingScorer{
		url:    url,
		corpus: corpus,
		inner:  NewSemanticScorer(corpus),
		client: &http.Client{Timeout: 30 * time.Second},
	}
	if len(corpus) == 0 {
		// nothing to embed or score against — the plain TF-IDF scorer
		// (scores 0 for everything) is the honest answer
		return s.inner
	}
	if err := s.warm(); err != nil {
		s.mu.Lock()
		s.disabled = true
		s.mu.Unlock()
		fmt.Fprintf(os.Stderr, "embeddings: disabled: %v (TF-IDF fallback)\n", err)
	}
	return s
}

// warm embeds the corpus against itself once (the "targets", embedded
// lazily on first use and cached under mu) and fits the calibration:
// x = embedding self-score (mean cosine vs the corpus targets),
// y = TF-IDF self-score; least squares a = Cov(x,y)/Var(x),
// b = meanY − a·meanX maps embedding scores onto the TF-IDF scale.
// A corpus of <2 titles or a zero-variance x leaves a=1,b=0 (identity).
func (s *EmbeddingScorer) warm() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targets != nil {
		return nil // already warmed
	}
	rows, err := s.embedRows(s.corpus, s.corpus)
	if err != nil {
		return err
	}
	s.targets = rows // the embedded corpus targets (warm state)
	n := len(s.corpus)
	if n < 2 {
		s.a, s.b = 1, 0
		return nil
	}
	xs := make([]float64, n)
	ys := make([]float64, n)
	mx, my := 0.0, 0.0
	for i, t := range s.corpus {
		xs[i] = rowMean(rows[i])
		ys[i] = s.inner.Score(t)
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(n)
	my /= float64(n)
	var cov, vx float64
	for i := 0; i < n; i++ {
		dx := xs[i] - mx
		cov += dx * (ys[i] - my)
		vx += dx * dx
	}
	if vx == 0 {
		s.a, s.b = 1, 0
		return nil
	}
	s.a = cov / vx
	s.b = my - s.a*mx
	return nil
}

// Score returns the calibrated mean cosine of text vs the corpus targets,
// clamped to [0,1]. On a per-call error the TF-IDF inner score is
// returned (errors logged at most once per minute).
func (s *EmbeddingScorer) Score(text string) float64 {
	if s.off() {
		return s.inner.Score(text)
	}
	rows, err := s.embedRows([]string{text}, s.corpus)
	if err != nil {
		s.warnOnce(err)
		return s.inner.Score(text)
	}
	return clamp01(s.a*rowMean(rows[0]) + s.b)
}

// BatchScore scores all texts in one round trip (per-candidate row means,
// calibrated and clamped). On a per-call error every text falls back to
// the TF-IDF inner scorer.
func (s *EmbeddingScorer) BatchScore(texts []string) []float64 {
	out := make([]float64, len(texts))
	if s.off() {
		for i, t := range texts {
			out[i] = s.inner.Score(t)
		}
		return out
	}
	if len(texts) == 0 {
		return out
	}
	rows, err := s.embedRows(texts, s.corpus)
	if err != nil {
		s.warnOnce(err)
		for i, t := range texts {
			out[i] = s.inner.Score(t)
		}
		return out
	}
	for i, row := range rows {
		out[i] = clamp01(s.a*rowMean(row) + s.b)
	}
	return out
}

// off reports whether construction failed and the scorer is permanently
// the TF-IDF inner scorer.
func (s *EmbeddingScorer) off() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabled
}

// warnOnce logs a per-call fallback error to stderr, at most once per
// minute (workers can hit the server concurrently; a dead server must not
// spam the log).
func (s *EmbeddingScorer) warnOnce(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastWarn) < time.Minute {
		return
	}
	s.lastWarn = time.Now()
	fmt.Fprintf(os.Stderr, "embeddings: %v (TF-IDF fallback)\n", err)
}

// embedRows: one POST to the embedding server. Request: {"candidate":
// <text>} for a single text or {"candidates": [...]} for many, always
// with {"targets": <corpus>}; response {"scores": ...} — a flat cosine
// row for a single candidate, or a candidates×targets matrix. Returns the
// rows (one per candidate, each of length len(targets)).
func (s *EmbeddingScorer) embedRows(candidates, targets []string) ([][]float64, error) {
	if len(candidates) == 0 {
		return nil, errors.New("no candidates to embed")
	}
	body := map[string]any{"targets": targets}
	if len(candidates) == 1 {
		body["candidate"] = candidates[0]
	} else {
		body["candidates"] = candidates
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Post(s.url, "application/json", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed server: http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<24))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Scores json.RawMessage `json:"scores"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("embed server: %v", err)
	}
	if len(parsed.Scores) == 0 || string(parsed.Scores) == "null" {
		return nil, errors.New("embed server: no scores in response")
	}
	rows, err := parseScoreMatrix(parsed.Scores, len(candidates), len(targets))
	if err != nil {
		return nil, fmt.Errorf("embed server: %v", err)
	}
	return rows, nil
}

// parseScoreMatrix accepts both response shapes: a flat row (single
// candidate, len == len(targets)) and a candidates×targets matrix. The
// row/column counts must match the request, else an error (bad JSON shape
// → caller falls back to TF-IDF).
func parseScoreMatrix(raw json.RawMessage, ncand, ntarget int) ([][]float64, error) {
	var flat []float64
	if err := json.Unmarshal(raw, &flat); err == nil {
		if ncand != 1 {
			return nil, fmt.Errorf("flat scores for %d candidates", ncand)
		}
		if len(flat) != ntarget {
			return nil, fmt.Errorf("flat scores: got %d, want %d", len(flat), ntarget)
		}
		return [][]float64{flat}, nil
	}
	var nested [][]float64
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, fmt.Errorf("bad scores shape: %v", err)
	}
	if len(nested) != ncand {
		return nil, fmt.Errorf("score rows: got %d, want %d", len(nested), ncand)
	}
	for i, row := range nested {
		if len(row) != ntarget {
			return nil, fmt.Errorf("score row %d: got %d, want %d", i, len(row), ntarget)
		}
	}
	return nested, nil
}

func rowMean(row []float64) float64 {
	m := 0.0
	for _, v := range row {
		m += v
	}
	return m / float64(len(row))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
