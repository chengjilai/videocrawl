package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"videocrawl/internal/model"
	"videocrawl/internal/score"
	"videocrawl/internal/store"
)

// cannedCorpus: a small corpus exercising every classification path
// (speakers, conferences, topics, micro-stopwords, years, diacritic
// fragments "rton" from "Márton" and "lois" from "Mélois").
var cannedCorpus = []string{
	"Go with Versions - Russ Cox (GopherCon 2018)",
	"Go Changes - Russ Cox (GopherCon 2023)",
	"Functional Imperative Programming in Flix - Magnus Madsen (GOTO 2023)",
	"Effectful Programming in Flix - Magnus Madsen (Lambda Days 2025)",
	"devenv is switching to Tvix (NixCon 2024)",
	"What's up with Tvix - Vincent Ambo & Florian Klink (2023)",
	"Engineering a Better Java Build Tool - Li Haoyi (Devoxx)",
	"An Introduction to the Mill Build Tool - Oliver Mélois (ScalaCon)",
	"Simplifying Kotlin Build Configuration with Amper - Márton Braun",
	"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
}

func TestGenerateQueriesClassification(t *testing.T) {
	q := GenerateQueries(cannedCorpus)
	if len(q) == 0 {
		t.Fatal("no queries generated")
	}
	has := func(want string) bool {
		for _, x := range q {
			if x == want {
				return true
			}
		}
		return false
	}
	idx := func(want string) int {
		for i, x := range q {
			if x == want {
				return i
			}
		}
		return -1
	}
	// speaker queries (df desc: go/russ/cox first among df==2)
	for _, w := range []string{"go talk", "russ talk", "cox talk", "flix talk", "magnus talk", "madsen talk", "tvix talk", "programming talk"} {
		if !has(w) {
			t.Errorf("missing speaker query %q; got %v", w, q)
		}
	}
	// conference queries (lexicon membership works even for all-caps names;
	// the conference cap keeps the top-4: gophercon, goto, nixcon, devoxx)
	for _, w := range []string{"gophercon", "gophercon talk", "goto", "goto talk", "nixcon talk", "devoxx"} {
		if !has(w) {
			t.Errorf("missing conference query %q; got %v", w, q)
		}
	}
	// '<conf> <topic>' pairing exists
	if !has("gophercon devenv") {
		t.Errorf("missing '<conf> <topic>' pairing; got %v", q)
	}
	// topic queries (lowercase-only words)
	for _, w := range []string{"devenv talk", "switching talk"} {
		if !has(w) {
			t.Errorf("missing topic query %q; got %v", w, q)
		}
	}
	// junk filtering: diacritic fragments, micro-stopwords, years, stopwords
	for _, bad := range []string{"lois", "rton", "in talk", "on talk", "up talk", "2023 talk", "2025 talk", "with talk", "from talk"} {
		if idx(bad) >= 0 {
			t.Errorf("junk query %q present: %v", bad, q)
		}
	}
	// query class ordering: speakers first, then conferences, then topics
	if !(idx("go talk") >= 0 && idx("go talk") < idx("gophercon") &&
		idx("gophercon") < idx("devenv talk")) {
		t.Errorf("class ordering broken: %v", q)
	}
	// user extras appended, deduplicated
	q2 := GenerateQueries(cannedCorpus, "custom query", "go talk", "  custom query  ")
	n := 0
	for _, x := range q2 {
		if x == "custom query" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("extra query dedup failed: %v", q2)
	}
	if len(q2) != len(q)+1 {
		t.Errorf("extras must append without trimming: %d -> %d", len(q), len(q2))
	}
}

// TestGenerateQueriesRealCorpus: pin the generated query list against the
// actual uploaded-talk corpus (config/semantic-corpus.json).
func TestGenerateQueriesRealCorpus(t *testing.T) {
	path := filepath.Join("..", "..", "config", "semantic-corpus.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("corpus not present: %v", err)
	}
	var d struct {
		Corpus []string `json:"corpus"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	q := GenerateQueries(d.Corpus)
	if len(q) != 25 {
		t.Errorf("len(q) = %d, want 25 (budget): %v", len(q), q)
	}
	has := func(want string) bool {
		for _, x := range q {
			if x == want {
				return true
			}
		}
		return false
	}
	// the expected top speakers / conferences / topics
	for _, w := range []string{"programming talk", "flix talk", "go talk", "spark talk", "build talk",
		"ssw", "gophercon talk", "guadec", "nixcon talk", "devenv talk", "switching talk", "application talk"} {
		if !has(w) {
			t.Errorf("real-corpus query missing %q: %v", w, q)
		}
	}
	for _, bad := range []string{"rton", "lois", "in talk", "on talk", "an talk", "what talk", "2025 talk", "up talk"} {
		for _, x := range q {
			if x == bad {
				t.Errorf("real-corpus junk query %q generated", bad)
			}
		}
	}
	// speakers (df desc) come before conferences come before topics
	idx := func(want string) int {
		for i, x := range q {
			if x == want {
				return i
			}
		}
		return -1
	}
	if !(idx("programming talk") < idx("ssw") && idx("ssw") < idx("switching talk")) {
		t.Errorf("real-corpus class ordering broken: %v", q)
	}
}

func TestSpeakerNames(t *testing.T) {
	names := SpeakerNames([]string{
		"What's up with Tvix - Vincent Ambo & Florian Klink (2023)",
		"Go Changes - Russ Cox (GopherCon 2023)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
		"Using Mutter as an application test framework | Carlos Garnacho @ GUADEC",
		"Robin Milner - Concept and Formality in Computing (CCS 2001)",
	})
	joined := strings.Join(names, "|")
	for _, want := range []string{"Vincent Ambo", "Florian Klink", "Russ Cox", "Richard Hipp", "Carlos Garnacho"} {
		if !strings.Contains(joined, want) {
			t.Errorf("speaker name %q missing: %v", want, names)
		}
	}
	// title-start pairs are NOT names
	for _, bad := range []string{"Go Changes", "Concept and", "Formality in"} {
		if strings.Contains(joined, bad) {
			t.Errorf("non-name pair %q extracted: %v", bad, names)
		}
	}
}

func TestMatchSpeakerBoost(t *testing.T) {
	names := []string{"Vincent", "Ambo", "Molodetskikh", "Shepherd"}
	if got := matchSpeaker("Tvix and NixOS - Vincent Ambo", names); got != "Vincent" {
		t.Errorf("matchSpeaker = %q, want Vincent", got)
	}
	// whole-word boundaries: "analysis" must not match "An"
	if got := matchSpeaker("An analysis of the kernel", []string{"An"}); got != "An" {
		t.Errorf("standalone 'An' must match: %q", got)
	}
	if got := matchSpeaker("kernel analysis", []string{"An"}); got != "" {
		t.Errorf("'An' inside 'analysis' must not match: %q", got)
	}
	if got := matchSpeaker("unrelated talk", names); got != "" {
		t.Errorf("no speaker in text: %q", got)
	}
}

func TestCorpusJaccard(t *testing.T) {
	corpus := []string{
		"Go with Versions - Russ Cox (GopherConSG 2018)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
	}
	// near-duplicate of an uploaded talk (one token differs)
	if j := corpusJaccard("Go with Versions - Russ Cox (GopherCon 2018)", corpus); j < 0.7 {
		t.Errorf("near-dup jaccard = %.2f, want >= 0.7", j)
	}
	// the same talk verbatim
	if j := corpusJaccard("Reliability Lessons From SQLite - Richard Hipp @ SSW 2026", corpus); j < 0.99 {
		t.Errorf("verbatim jaccard = %.2f", j)
	}
	// novel talk: far below the dup threshold
	if j := corpusJaccard("History of the Disney Parks", corpus); j >= 0.7 {
		t.Errorf("unrelated jaccard = %.2f, want < 0.7", j)
	}
	if j := corpusJaccard("", corpus); j != 0 {
		t.Errorf("empty title jaccard = %.2f, want 0", j)
	}
}

// gateTestApp: an App over a fresh DB with one known video.
func gateTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "vc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srcID, err := st.AddSource(model.KindYoutubeChannel, "https://www.youtube.com/channel/UCx", "", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertVideo(model.Video{
		SourceID: srcID, VideoID: "known1",
		URL: "https://www.youtube.com/watch?v=known1", Title: "Already Queued",
	}); err != nil {
		t.Fatal(err)
	}
	a := &App{DBPath: filepath.Join(dir, "vc.db")}
	return a, st
}

// TestDiscoverGates: the gate pipeline on canned candidates (no network).
func TestDiscoverGates(t *testing.T) {
	corpusTitles := []string{
		"Go with Versions - Russ Cox (GopherConSG 2018)",
		"Reliability Lessons From SQLite - Richard Hipp @ SSW 2026",
	}
	_, st := gateTestApp(t)
	knownURLs, knownIDs, err := knownVideoSets(st)
	if err != nil {
		t.Fatal(err)
	}
	newG := func(includeKnown bool, topics string) *gates {
		return &gates{
			corpusTitles: corpusTitles,
			scorer:       score.NewSemanticScorer(corpusTitles),
			filter:       topicFilter(topics),
			threshold:    0.12,
			includeKnown: includeKnown,
			knownURLs:    knownURLs,
			knownIDs:     knownIDs,
			speakers:     SpeakerNames(corpusTitles),
		}
	}
	g := newG(false, "")

	// 1. videos-table dedup: dropped by default...
	c := candidate{Source: "yt", VideoID: "known1", URL: "https://www.youtube.com/watch?v=known1", Title: "Go Changes - Russ Cox (GopherCon 2023)"}
	if g.gate(&c) {
		t.Error("known video must drop without --include-known")
	}
	// ... and tagged with --include-known
	g.includeKnown = true
	if !g.gate(&c) || !c.Known {
		t.Errorf("known video with --include-known: gate=%v known=%v", g.gate(&c), c.Known)
	}
	g.includeKnown = false

	// 2. corpus duplicate (title token-Jaccard >= 0.7 vs an uploaded talk)
	c = candidate{URL: "https://www.youtube.com/watch?v=dup1", VideoID: "dup1",
		Title: "Go with Versions - Russ Cox (GopherCon 2018)"}
	if g.gate(&c) {
		t.Error("corpus duplicate must drop")
	}
	// 3. topic filter (OR semantics, -exclude)
	g.filter = topicFilter("disney")
	c = candidate{URL: "https://www.youtube.com/watch?v=t1", VideoID: "t1",
		Title: "Go Changes - Russ Cox (GopherCon 2023)", Channel: "GopherCon"}
	if g.gate(&c) {
		t.Error("topic filter must drop non-matching titles")
	}
	g.filter = topicFilter("go,-changes")
	c = candidate{URL: "https://www.youtube.com/watch?v=t2", VideoID: "t2",
		Title: "Go from C to Go - Russ Cox (GopherCon 2019)"}
	if !g.gate(&c) {
		t.Error("topic filter must keep matching titles")
	}
	g.filter = topicFilter("")
	// 4. semantic gate drops junk
	c = candidate{URL: "https://www.youtube.com/watch?v=j1", VideoID: "j1", Title: "History of Hardees"}
	if g.gate(&c) {
		t.Error("semantic junk must drop")
	}
	// 5. non-Latin script drops
	c = candidate{URL: "https://www.youtube.com/watch?v=cn1", VideoID: "cn1", Title: "Go Changes 中文测试中文测试中文测试"}
	if g.gate(&c) {
		t.Error("non-Latin title must drop")
	}
	// 6. speaker boost: a low-semantic title naming a corpus speaker passes
	//    with score = max(score, threshold) and is tagged speaker:<name>
	g2 := newG(false, "")
	c = candidate{URL: "https://www.youtube.com/watch?v=b1", VideoID: "b1", Title: "New Talk by Richard Hipp"}
	if !g2.gate(&c) {
		t.Fatal("speaker boost must pass a speaker-named candidate")
	}
	if c.Speaker != "Richard Hipp" {
		t.Errorf("speaker = %q, want Richard Hipp", c.Speaker)
	}
	if c.Score < g2.threshold {
		t.Errorf("boosted score = %.3f, want >= threshold %.3f", c.Score, g2.threshold)
	}
	// 7. finalize re-scores with enriched title/channel (post-GetMeta) and
	//    re-applies the boost
	c = candidate{URL: "https://www.youtube.com/watch?v=b2", VideoID: "b2",
		Title: "A New Talk by Richard Hipp"}
	if !g2.gate(&c) {
		t.Fatal("speaker-named candidate must pass the gate")
	}
	c.Speaker = ""
	c.Score = 0.05
	if !g2.finalize(&c, "enriched title text") || c.Speaker != "Richard Hipp" {
		t.Errorf("finalize boost: gate=%v speaker=%q score=%.3f", c.Speaker != "Richard Hipp" || true, c.Speaker, c.Score)
	}
	if c.Score < g2.threshold {
		t.Errorf("boosted final score = %.3f, want >= %.3f", c.Score, g2.threshold)
	}
	// 8. finalize drops enriched scores below the threshold when no corpus
	//    speaker name is present (boost cannot fire)
	c = candidate{URL: "https://www.youtube.com/watch?v=b3", VideoID: "b3",
		Title: "History of Hardees"}
	ok := g2.finalize(&c, "unrelated text with no corpus vocabulary whatsoever")
	t.Logf("DBG #8: ok=%v score=%.4f speaker=%q threshold=%v filter=%v speakers=%v", ok, c.Score, c.Speaker, g2.threshold, g2.filter != nil, g2.speakers)
	if ok {
		t.Error("finalize must drop below-threshold enriched scores")
	}
}
