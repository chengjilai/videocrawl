package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"videocrawl/internal/model"
)

// newTestStore opens a fresh SQLite DB in a temp dir (WAL + prepared
// statements, same as production).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insVideo(t *testing.T, s *Store, v model.Video) {
	t.Helper()
	if err := s.UpsertVideo(v); err != nil {
		t.Fatalf("UpsertVideo(%d/%s): %v", v.SourceID, v.VideoID, err)
	}
}

func getOne(t *testing.T, s *Store, srcID int64, videoID string) model.Video {
	t.Helper()
	rows, err := s.VideoRows("", 1000)
	if err != nil {
		t.Fatalf("VideoRows: %v", err)
	}
	for _, v := range rows {
		if v.SourceID == srcID && v.VideoID == videoID {
			return v
		}
	}
	t.Fatalf("video %d/%s not found", srcID, videoID)
	return model.Video{}
}

func ids(vs []model.Video) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.VideoID
	}
	return out
}

// setVideoState drives the status/attempts bookkeeping columns directly
// (UpsertVideo always inserts status=new attempts=0; only the download
// path mutates these).
func setVideoState(t *testing.T, s *Store, srcID int64, videoID, status string, attempts int) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE videos SET status=?, attempts=? WHERE source_id=? AND video_id=?`,
		status, attempts, srcID, videoID); err != nil {
		t.Fatalf("setVideoState(%s): %v", videoID, err)
	}
}

// ---- videos ----

// TestUpsertVideoDedup: the (source_id, video_id) PK means re-inserting the
// same video no-ops; the first row wins.
func TestUpsertVideoDedup(t *testing.T) {
	s := newTestStore(t)
	insVideo(t, s, model.Video{SourceID: 1, VideoID: "abc", URL: "https://youtu.be/abc", Title: "original"})
	// same PK, different payload — INSERT OR IGNORE must keep the first row
	insVideo(t, s, model.Video{SourceID: 1, VideoID: "abc", URL: "https://youtu.be/other", Title: "changed"})

	rows, err := s.VideoRows("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Title != "original" {
		t.Errorf("first-row-wins violated: title=%q", rows[0].Title)
	}
	if rows[0].URL != "https://youtu.be/abc" {
		t.Errorf("URL changed: %q", rows[0].URL)
	}
	if rows[0].Status != model.StatusNew {
		t.Errorf("status=%q, want new", rows[0].Status)
	}
	// same video id under a different source is a different row (PK is composite)
	insVideo(t, s, model.Video{SourceID: 2, VideoID: "abc", URL: "https://youtu.be/abc"})
	if rows, _ := s.VideoRows("", 10); len(rows) != 2 {
		t.Errorf("composite PK violated: %d rows", len(rows))
	}
}

// TestNextForDownloadOrdering: oldest published first, undated last, only
// new/failed with attempts < 5.
func TestNextForDownloadOrdering(t *testing.T) {
	s := newTestStore(t)
	v := func(id, published, status string, attempts int) model.Video {
		return model.Video{SourceID: 1, VideoID: id, URL: "u/" + id, Published: published, Status: status, Attempts: attempts}
	}
	insVideo(t, s, v("a2024", "2024-01-01", model.StatusNew, 0))
	insVideo(t, s, v("b2023", "2023-01-01", model.StatusNew, 0))
	insVideo(t, s, v("cundated", "", model.StatusNew, 0))
	insVideo(t, s, v("ddone", "2022-01-01", model.StatusDone, 0))
	insVideo(t, s, v("efail", "2021-01-01", model.StatusFailed, 0))
	insVideo(t, s, v("fmaxatt", "2020-01-01", model.StatusNew, 5))
	insVideo(t, s, v("gmaxfail", "2019-01-01", model.StatusFailed, 5))
	// seed the bookkeeping columns UpsertVideo doesn't touch
	setVideoState(t, s, 1, "ddone", model.StatusDone, 0)
	setVideoState(t, s, 1, "efail", model.StatusFailed, 0)
	setVideoState(t, s, 1, "fmaxatt", model.StatusNew, 5)
	setVideoState(t, s, 1, "gmaxfail", model.StatusFailed, 5)

	got, err := s.NextForDownload(10, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"efail", "b2023", "a2024", "cundated"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, ids(got))
	}
	for i, w := range want {
		if got[i].VideoID != w {
			t.Fatalf("order[%d] = %s, want %s (all: %v)", i, got[i].VideoID, w, ids(got))
		}
	}
	// LIMIT is honored
	if got2, _ := s.NextForDownload(2, 0, 0); len(got2) != 2 || got2[0].VideoID != "efail" || got2[1].VideoID != "b2023" {
		t.Fatalf("limit 2: %v", ids(got2))
	}
	// same-date tiebreak falls back to (source_id, video_id) — deterministic
	insVideo(t, s, v("h2023", "2023-01-01", model.StatusNew, 0))
	if got3, _ := s.NextForDownload(10, 0, 0); got3[1].VideoID != "b2023" || got3[2].VideoID != "h2023" {
		t.Fatalf("tiebreak: %v", ids(got3))
	}
}

// TestMarkTransitions: MarkDownloaded / MarkSkipped / MarkFailed set status,
// record last_error and increment attempts.
func TestMarkTransitions(t *testing.T) {
	s := newTestStore(t)
	insVideo(t, s, model.Video{SourceID: 7, VideoID: "x", URL: "u/x"})

	// MarkDownloaded fills the full payload
	upd := model.Video{
		SourceID: 7, VideoID: "x", Title: "T", Duration: 120,
		Published: "2024-01-01", Channel: "C", SizeBytes: 99, Path: "/p/x.mp4", SHA256: "deadbeef",
	}
	if err := s.MarkDownloaded(upd); err != nil {
		t.Fatal(err)
	}
	v := getOne(t, s, 7, "x")
	if v.Status != model.StatusDone || v.Attempts != 1 {
		t.Errorf("after MarkDownloaded: status=%q attempts=%d", v.Status, v.Attempts)
	}
	if v.Title != "T" || v.Duration != 120 || v.Published != "2024-01-01" || v.Channel != "C" {
		t.Errorf("after MarkDownloaded: meta %+v", v)
	}
	if v.SizeBytes != 99 || v.Path != "/p/x.mp4" || v.SHA256 != "deadbeef" {
		t.Errorf("after MarkDownloaded: file %+v", v)
	}
	if v.LastError != "" {
		t.Errorf("last_error not cleared: %q", v.LastError)
	}

	// MarkSkipped
	if err := s.MarkSkipped(7, "x", "too short: 30s"); err != nil {
		t.Fatal(err)
	}
	v = getOne(t, s, 7, "x")
	if v.Status != model.StatusSkipped || v.Attempts != 2 || v.LastError != "too short: 30s" {
		t.Errorf("after MarkSkipped: status=%q attempts=%d last_error=%q", v.Status, v.Attempts, v.LastError)
	}

	// MarkFailed
	if err := s.MarkFailed(7, "x", "http 412"); err != nil {
		t.Fatal(err)
	}
	v = getOne(t, s, 7, "x")
	if v.Status != model.StatusFailed || v.Attempts != 3 || v.LastError != "http 412" {
		t.Errorf("after MarkFailed: status=%q attempts=%d last_error=%q", v.Status, v.Attempts, v.LastError)
	}

	// a failed video with attempts < 5 stays eligible
	if got, _ := s.NextForDownload(10, 0, 0); len(got) != 1 || got[0].VideoID != "x" {
		t.Errorf("failed video (attempts=3) not re-queued: %v", ids(got))
	}
	// after enough failures the attempts<5 filter kicks in
	if err := s.MarkFailed(7, "x", "again"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed(7, "x", "again"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.NextForDownload(10, 0, 0); len(got) != 0 {
		t.Errorf("attempts>=5 video re-queued: %v", ids(got))
	}
}

// TestUpdateMeta: enrichment backfill (title/duration/published/channel).
func TestUpdateMeta(t *testing.T) {
	s := newTestStore(t)
	insVideo(t, s, model.Video{SourceID: 3, VideoID: "y", URL: "u/y", Title: "old"})
	if err := s.UpdateMeta(3, "y", "new title", 777, "2024-05-05", "chan"); err != nil {
		t.Fatal(err)
	}
	v := getOne(t, s, 3, "y")
	if v.Title != "new title" || v.Duration != 777 || v.Published != "2024-05-05" || v.Channel != "chan" {
		t.Errorf("UpdateMeta did not persist: %+v", v)
	}
	// UpdateMeta on a missing row is a no-op (no error, nothing created)
	if err := s.UpdateMeta(3, "nope", "x", 1, "", ""); err != nil {
		t.Errorf("UpdateMeta missing row: %v", err)
	}
	if rows, _ := s.VideoRows("", 10); len(rows) != 1 {
		t.Errorf("UpdateMeta created a row: %d", len(rows))
	}
}

// TestNextForUpload: only done videos, oldest published first, undated
// last, limit honored; uploaded videos never reappear.
func TestNextForUpload(t *testing.T) {
	s := newTestStore(t)
	v := func(id, published, status string) model.Video {
		return model.Video{SourceID: 1, VideoID: id, URL: "u/" + id, Published: published, Status: status}
	}
	insVideo(t, s, v("a2024", "2024-01-01", model.StatusNew))
	insVideo(t, s, v("b2023", "2023-01-01", model.StatusNew))
	insVideo(t, s, v("ddone", "2022-01-01", model.StatusNew))
	insVideo(t, s, v("eundated", "", model.StatusNew))
	insVideo(t, s, v("fnew", "2020-01-01", model.StatusNew))
	setVideoState(t, s, 1, "ddone", model.StatusDone, 0)
	setVideoState(t, s, 1, "eundated", model.StatusDone, 0)
	setVideoState(t, s, 1, "b2023", model.StatusDone, 0)
	setVideoState(t, s, 1, "a2024", model.StatusDone, 0)

	got, err := s.NextForUpload(10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ddone", "b2023", "a2024", "eundated"} // new excluded
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, ids(got))
	}
	for i, w := range want {
		if got[i].VideoID != w {
			t.Fatalf("order[%d] = %s, want %s (all: %v)", i, got[i].VideoID, w, ids(got))
		}
	}
	if got2, _ := s.NextForUpload(2); len(got2) != 2 || got2[0].VideoID != "ddone" || got2[1].VideoID != "b2023" {
		t.Fatalf("limit 2: %v", ids(got2))
	}
}

// TestUploadMarked: sets status=uploaded + bvid; the video leaves both the
// upload queue and the download queue.
func TestUploadMarked(t *testing.T) {
	s := newTestStore(t)
	insVideo(t, s, model.Video{SourceID: 1, VideoID: "v1", URL: "u/v1"})
	setVideoState(t, s, 1, "v1", model.StatusDone, 0)
	if err := s.UploadMarked(1, "v1", "BV1xx411c7mD"); err != nil {
		t.Fatal(err)
	}
	v := getOne(t, s, 1, "v1")
	if v.Status != model.StatusUploaded || v.BVID != "BV1xx411c7mD" {
		t.Errorf("after UploadMarked: status=%q bvid=%q", v.Status, v.BVID)
	}
	if got, _ := s.NextForUpload(10); len(got) != 0 {
		t.Errorf("uploaded video still in upload queue: %v", ids(got))
	}
	if got, _ := s.NextForDownload(10, 0, 0); len(got) != 0 {
		t.Errorf("uploaded video in download queue: %v", ids(got))
	}
}

// TestBvidMigration: an old-schema DB (videos without the bvid column) is
// migrated on Open so upload tracking works.
func TestBvidMigration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.db")
	// build a pre-bvid videos table by hand (the original schema)
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE videos (
		source_id INTEGER NOT NULL,
		video_id TEXT NOT NULL,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		duration INTEGER NOT NULL DEFAULT 0,
		published TEXT NOT NULL DEFAULT '',
		channel TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'new',
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		size_bytes INTEGER NOT NULL DEFAULT 0,
		path TEXT NOT NULL DEFAULT '',
		sha256 TEXT NOT NULL DEFAULT '',
		fetched_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (source_id, video_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO videos (source_id,video_id,url,status) VALUES (1,'old','u/old','done')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(p) // must migrate
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UploadMarked(1, "old", "BV1old123456"); err != nil {
		t.Fatal(err)
	}
	v := getOne(t, s, 1, "old")
	if v.Status != model.StatusUploaded || v.BVID != "BV1old123456" {
		t.Errorf("migrated DB: status=%q bvid=%q", v.Status, v.BVID)
	}
}

// TestVideoFilesRoundtrip: UpsertFiles/GetFiles persist and replace by PK.
func TestVideoFilesRoundtrip(t *testing.T) {
	s := newTestStore(t)
	files := []model.File{
		{URL: "https://cdn/x.mp4", Size: 1234, Ext: "mp4", Kind: "video"},
		{URL: "https://cdn/x.en.srt", Size: 10, Ext: "srt", Kind: "sub"},
		{URL: "https://cdn/x.en.vtt", Size: 20, Ext: "vtt", Kind: "sub"},
	}
	if err := s.UpsertFiles(5, "vid", files); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFiles(5, "vid")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 files, got %d", len(got))
	}
	byURL := map[string]model.File{}
	for _, f := range got {
		byURL[f.URL] = f
	}
	if f, ok := byURL["https://cdn/x.mp4"]; !ok || f.Kind != "video" || f.Ext != "mp4" || f.Size != 1234 {
		t.Errorf("video file wrong: %+v", byURL["https://cdn/x.mp4"])
	}
	if f, ok := byURL["https://cdn/x.en.srt"]; !ok || f.Kind != "sub" || f.Ext != "srt" {
		t.Errorf("sub file wrong: %+v", byURL["https://cdn/x.en.srt"])
	}

	// re-upsert replaces by (source_id, video_id, kind, url), no duplicates
	files[0].Size = 9999
	files = append(files, model.File{URL: "https://cdn/x.mp4", Kind: "video", Ext: "mp4", Size: 8888}) // dup PK of files[0]
	if err := s.UpsertFiles(5, "vid", files); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetFiles(5, "vid")
	if len(got2) != 3 {
		t.Fatalf("re-upsert duplicated rows: %d", len(got2))
	}
	for _, f := range got2 {
		if f.URL == "https://cdn/x.mp4" && f.Size != 8888 {
			t.Errorf("replace did not apply: %+v", f)
		}
	}

	// per-video isolation
	if err := s.UpsertFiles(5, "vid2", []model.File{{URL: "https://cdn/y.mp4", Kind: "video", Ext: "mp4"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFiles(5, "vid2"); len(got) != 1 {
		t.Errorf("vid2 files: %d", len(got))
	}
	if got, _ := s.GetFiles(5, "vid"); len(got) != 3 {
		t.Errorf("vid files changed by vid2 upsert: %d", len(got))
	}
	if got, _ := s.GetFiles(5, "missing"); len(got) != 0 {
		t.Errorf("missing video files: %d", len(got))
	}
}

// ---- sources ----

func TestSourceCRUD(t *testing.T) {
	s := newTestStore(t)
	url := "https://www.youtube.com/playlist?list=UUabcdefghijklmnopqrstuvwx"
	id, err := s.AddSource(model.KindYoutubeChannel, url, "", "chan")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("first id = %d, want 1", id)
	}
	// same URL → dedup to the existing id, no second row
	id2, err := s.AddSource(model.KindYoutubeChannel, url, "", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("dedup id = %d, want %d", id2, id)
	}
	id3, err := s.AddSource(model.KindRSS, "https://example.com/feed", "", "feed")
	if err != nil {
		t.Fatal(err)
	}
	if id3 == id {
		t.Fatalf("second AddSource reused id %d", id)
	}
	src3, err := s.GetSource(id3)
	if err != nil {
		t.Fatal(err)
	}
	if src3.Kind != model.KindRSS || src3.Site != "rss" || src3.URL != "https://example.com/feed" {
		t.Errorf("GetSource(%d): %+v", id3, src3)
	}

	src, err := s.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != model.KindYoutubeChannel || src.Site != "youtube" {
		t.Errorf("GetSource: kind=%q site=%q", src.Kind, src.Site)
	}
	if !src.Enabled {
		t.Error("source should default to enabled")
	}
	if src.Created == "" {
		t.Error("created timestamp missing")
	}

	list, err := s.ListSources(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListSources(false): %d, want 2", len(list))
	}
	en, err := s.ListSources(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(en) != 2 {
		t.Fatalf("ListSources(true): %d, want 2 (all enabled)", len(en))
	}

	// SetSourceEnum persists count/completeness + timestamp
	if err := s.SetSourceEnum(id, 42, true); err != nil {
		t.Fatal(err)
	}
	src, _ = s.GetSource(id)
	if src.EnumCount != 42 || !src.EnumComplete {
		t.Errorf("enum state: count=%d complete=%v", src.EnumCount, src.EnumComplete)
	}
	if src.LastEnum == "" {
		t.Error("last_enum not set")
	}

	// DeleteSource cascades to the frontier
	insVideo(t, s, model.Video{SourceID: id, VideoID: "v1", URL: "u/v1"})
	if err := s.DeleteSource(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSource(id); err == nil {
		t.Error("GetSource after DeleteSource: want error")
	}
	rows, _ := s.VideoRows("", 10)
	for _, v := range rows {
		if v.SourceID == id {
			t.Errorf("video %s survived source delete", v.VideoID)
		}
	}
	list, _ = s.ListSources(false)
	if len(list) != 1 {
		t.Fatalf("after delete: %d sources, want 1", len(list))
	}
}

func TestMetaSetGet(t *testing.T) {
	s := newTestStore(t)
	if got := s.MetaGet("k"); got != "" {
		t.Errorf("MetaGet empty = %q", got)
	}
	if err := s.MetaSet("k", "v"); err != nil {
		t.Fatal(err)
	}
	if got := s.MetaGet("k"); got != "v" {
		t.Errorf("MetaGet = %q, want v", got)
	}
	if err := s.MetaSet("k", "v2"); err != nil { // upsert, not duplicate
		t.Fatal(err)
	}
	if got := s.MetaGet("k"); got != "v2" {
		t.Errorf("MetaGet after update = %q, want v2", got)
	}
}
