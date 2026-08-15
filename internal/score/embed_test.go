package score

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// embedReq: the request shape the embedding server receives.
type embedReq struct {
	Candidate  string   `json:"candidate"`
	Candidates []string `json:"candidates"`
	Targets    []string `json:"targets"`
}

// scaledServer: a fake embedding server whose "embeddings" are the TF-IDF
// self-scores, scaled and offset (emb = k*inner.Score(x) + c, constant
// across targets). The warm matrix's row means are then exactly
// proportional to the client's TF-IDF self-scores, so the fitted
// calibration a=1/k, b=-c/k undoes the scaling and preserves ranking.
// Single-candidate requests answer with a flat row; multi-candidate with
// a matrix (the real API's shapes).
func scaledServer(k, c float64, lastReq *embedReq) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*lastReq = req
		inner := NewSemanticScorer(req.Targets)
		cands := req.Candidates
		if req.Candidate != "" {
			cands = []string{req.Candidate}
		}
		rows := make([][]float64, len(cands))
		for i, x := range cands {
			y := k*inner.Score(x) + c
			row := make([]float64, len(req.Targets))
			for j := range row {
				row[j] = y
			}
			rows[i] = row
		}
		if req.Candidate != "" {
			json.NewEncoder(w).Encode(map[string]any{"scores": rows[0]})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"scores": rows})
		}
	}))
}

func mustEmbed(t *testing.T, corpus []string) *EmbeddingScorer {
	t.Helper()
	s := New(corpus)
	es, ok := s.(*EmbeddingScorer)
	if !ok {
		t.Fatalf("New = %T, want *EmbeddingScorer", s)
	}
	if es.disabled {
		t.Fatal("embedding scorer disabled on the happy path")
	}
	return es
}

// TestEmbeddingScorerScore: single-title corpus forces the degenerate
// calibration (a=1,b=0), so Score is exactly the (clamped) mean cosine.
func TestEmbeddingScorerScore(t *testing.T) {
	corpus := []string{"Go with Versions - Russ Cox (GopherCon 2018)"}
	var last embedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		last = req
		v := 0.8
		cand := req.Candidate
		if cand == "" && len(req.Candidates) == 1 {
			cand = req.Candidates[0]
		}
		switch {
		case strings.Contains(cand, "clamp-high"):
			v = 1.4
		case strings.Contains(cand, "clamp-low"):
			v = -0.3
		}
		json.NewEncoder(w).Encode(map[string]any{"scores": []float64{v}})
	}))
	defer srv.Close()
	t.Setenv("VIDEOCRAWL_EMBED_URL", srv.URL+"/embed")

	es := mustEmbed(t, corpus)
	if es.a != 1 || es.b != 0 {
		t.Errorf("single-title calibration = a=%v b=%v, want 1/0", es.a, es.b)
	}
	if got := es.Score(corpus[0]); got != 0.8 {
		t.Errorf("Score = %v, want 0.8 (mean cosine)", got)
	}
	if got := es.Score("clamp-high whatever"); got != 1.0 {
		t.Errorf("high cosine clamped = %v, want 1.0", got)
	}
	if got := es.Score("clamp-low whatever"); got != 0.0 {
		t.Errorf("negative cosine clamped = %v, want 0.0", got)
	}
	// the single-candidate request form carries "candidate" (not the
	// candidates array) and the corpus as targets
	if last.Candidate == "" || last.Candidate != "clamp-low whatever" {
		t.Errorf("last request candidate = %q, want the single-candidate form", last.Candidate)
	}
	if len(last.Targets) != 1 || last.Targets[0] != corpus[0] {
		t.Errorf("last request targets = %v, want the corpus", last.Targets)
	}
}

// TestEmbeddingScorerBatch: multi-candidate requests use the candidates
// array form and answer a matrix; BatchScore calibrates each row mean back
// onto the TF-IDF scale (the k·y+c server scaling is exactly undone).
func TestEmbeddingScorerBatch(t *testing.T) {
	corpus := []string{
		"Go with Versions - Russ Cox (GopherCon 2018)",
		"Go Changes - Russ Cox (GopherCon 2023)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
	}
	var last embedReq
	srv := scaledServer(2.0, 0.1, &last)
	defer srv.Close()
	t.Setenv("VIDEOCRAWL_EMBED_URL", srv.URL)

	es := mustEmbed(t, corpus)
	inner := NewSemanticScorer(corpus)
	texts := []string{corpus[0], "History of the Disney Parks", corpus[2]}
	got := es.BatchScore(texts)
	for i, want := range []float64{inner.Score(texts[0]), inner.Score(texts[1]), inner.Score(texts[2])} {
		if math.Abs(got[i]-want) > 1e-9 {
			t.Errorf("BatchScore[%d] = %v, want %v (calibrated TF-IDF)", i, got[i], want)
		}
	}
	if len(last.Candidates) != 3 || last.Candidate != "" {
		t.Errorf("batch request = candidate=%q candidates=%v, want the candidates array", last.Candidate, last.Candidates)
	}
	if len(last.Targets) != 3 {
		t.Errorf("batch request targets = %v, want the corpus", last.Targets)
	}
	// empty batch: no round trip, empty result
	if out := es.BatchScore(nil); len(out) != 0 {
		t.Errorf("BatchScore(nil) = %v, want empty", out)
	}
}

// TestEmbeddingScorerDisabledOn500: construction warm fails (HTTP 500) →
// the scorer is permanently the TF-IDF inner scorer.
func TestEmbeddingScorerDisabledOn500(t *testing.T) {
	corpus := []string{
		"Go with Versions - Russ Cox (GopherCon 2018)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("VIDEOCRAWL_EMBED_URL", srv.URL)

	s := New(corpus)
	es, ok := s.(*EmbeddingScorer)
	if !ok || !es.disabled {
		t.Fatal("500 at warm must disable the embedding scorer")
	}
	inner := NewSemanticScorer(corpus)
	for _, text := range []string{corpus[0], "unrelated junk text"} {
		if got, want := s.Score(text), inner.Score(text); got != want {
			t.Errorf("disabled Score(%q) = %v, want TF-IDF %v", text, got, want)
		}
	}
	got := es.BatchScore(corpus)
	for i, c := range corpus {
		if want := inner.Score(c); got[i] != want {
			t.Errorf("disabled BatchScore[%d] = %v, want TF-IDF %v", i, got[i], want)
		}
	}
}

// TestEmbeddingScorerConnectionRefused: the server is unreachable at
// construction → same permanent TF-IDF fallback.
func TestEmbeddingScorerConnectionRefused(t *testing.T) {
	corpus := []string{"Go with Versions - Russ Cox (GopherCon 2018)"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // refuse connections
	t.Setenv("VIDEOCRAWL_EMBED_URL", url)

	s := New(corpus)
	es, ok := s.(*EmbeddingScorer)
	if !ok || !es.disabled {
		t.Fatal("unreachable server must disable the embedding scorer")
	}
	inner := NewSemanticScorer(corpus)
	if got, want := s.Score(corpus[0]), inner.Score(corpus[0]); got != want {
		t.Errorf("Score = %v, want TF-IDF %v", got, want)
	}
}

// TestEmbeddingScorerPerCallFallback: the warm succeeds but a scoring call
// fails — the call falls back to TF-IDF per text, the scorer stays
// enabled, and errors are logged at most once per minute.
func TestEmbeddingScorerPerCallFallback(t *testing.T) {
	corpus := []string{
		"Go with Versions - Russ Cox (GopherCon 2018)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // warm: 2x2 self-matrix
			json.NewEncoder(w).Encode(map[string]any{"scores": [][]float64{{0.8, 0.5}, {0.5, 0.9}}})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("VIDEOCRAWL_EMBED_URL", srv.URL)

	es := mustEmbed(t, corpus)
	inner := NewSemanticScorer(corpus)

	// capture stderr: two failing calls must log at most once (throttle)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	got1 := es.Score("some talk text")
	got2 := es.BatchScore([]string{"more text"})[0]
	w.Close()
	os.Stderr = old
	logged, _ := io.ReadAll(r)

	if got1 != inner.Score("some talk text") {
		t.Errorf("per-call fallback Score = %v, want TF-IDF %v", got1, inner.Score("some talk text"))
	}
	if got2 != inner.Score("more text") {
		t.Errorf("per-call fallback BatchScore = %v, want TF-IDF %v", got2, inner.Score("more text"))
	}
	if es.disabled {
		t.Error("per-call errors must NOT disable the scorer (only construction failures do)")
	}
	if n := bytes.Count(logged, []byte("embeddings:")); n > 1 {
		t.Errorf("logged %d fallback warnings, want <= 1 (per-minute throttle)", n)
	}
}

// TestEmbeddingCalibrationPreservesRanking: with a synthetic corpus the
// server's self-scores are the TF-IDF self-scores scaled (emb = 2y+0.1),
// so calibration fits a=1/2, b=-0.05 and exactly reconstructs the TF-IDF
// scores — a corpus title must rank above an unrelated string.
func TestEmbeddingCalibrationPreservesRanking(t *testing.T) {
	corpus := []string{
		"Go with Versions - Russ Cox (GopherCon 2018)",
		"Go Changes - Russ Cox (GopherCon 2023)",
		"Functional Imperative Programming in Flix - Magnus Madsen (GOTO 2023)",
	}
	var last embedReq
	srv := scaledServer(2.0, 0.1, &last)
	defer srv.Close()
	t.Setenv("VIDEOCRAWL_EMBED_URL", srv.URL)

	es := mustEmbed(t, corpus)
	inner := NewSemanticScorer(corpus)
	if math.Abs(es.a-0.5) > 1e-9 || math.Abs(es.b+0.05) > 1e-9 {
		t.Errorf("calibration = a=%v b=%v, want a=0.5 b=-0.05", es.a, es.b)
	}
	unrelated := "History of the Disney Parks"
	low := es.Score(unrelated)
	for _, title := range corpus {
		sc := es.Score(title)
		if sc <= low {
			t.Errorf("calibration broke ranking: %q = %.4f <= unrelated %.4f", title, sc, low)
		}
		// the scaling is exactly undone: Score == the TF-IDF self-score
		if math.Abs(sc-inner.Score(title)) > 1e-6 {
			t.Errorf("calibrated %q = %.6f, want TF-IDF %.6f", title, sc, inner.Score(title))
		}
	}
	// batch and single-candidate paths agree on the same text
	b := es.BatchScore([]string{corpus[0], unrelated})
	if math.Abs(b[0]-es.Score(corpus[0])) > 1e-12 || math.Abs(b[1]-low) > 1e-12 {
		t.Errorf("batch/single disagree: batch=%v single=%v/%v", b, es.Score(corpus[0]), low)
	}
}

// TestNewDefaultsToTFIDF: without VIDEOCRAWL_EMBED_URL, New returns the
// plain TF-IDF scorer (no server involved).
func TestNewDefaultsToTFIDF(t *testing.T) {
	t.Setenv("VIDEOCRAWL_EMBED_URL", "")
	s := New([]string{"Go with Versions - Russ Cox (GopherCon 2018)"})
	if _, ok := s.(*SemanticScorer); !ok {
		t.Errorf("New without embed URL = %T, want *SemanticScorer", s)
	}
	// an empty corpus never touches the server either (nothing to embed)
	t.Setenv("VIDEOCRAWL_EMBED_URL", "http://127.0.0.1:1/embed")
	if _, ok := New(nil).(*SemanticScorer); !ok {
		t.Error("New(nil) = embedding scorer, want plain TF-IDF")
	}
}
