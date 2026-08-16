// Package store: SQLite persistence. The frontier lives in the DB (research
// lesson: TubeSync UNIQUE(source,key), gallery-dl archive table) so resume,
// dedup and concurrent workers are free. WAL + prepared statements, the same
// pragmas techcrawl-go measured as fastest.
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // pure-Go (wasm-derived), static binary

	"videocrawl/internal/model"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: single writer, avoid SQLITE_BUSY
	s := &Store{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`) // best-effort; errors ignored
	return s.db.Close()
}

func (s *Store) init() error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA synchronous=NORMAL`,
		`CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			url TEXT NOT NULL,
			query TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			site TEXT NOT NULL DEFAULT '',
			needs_proxy INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			last_enum TEXT NOT NULL DEFAULT '',
			enum_count INTEGER NOT NULL DEFAULT 0,
			enum_complete INTEGER NOT NULL DEFAULT 0,
			created TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sources_url_query ON sources(url, query)`,
		`DROP INDEX IF EXISTS idx_sources_url`,
		`CREATE TABLE IF NOT EXISTS videos (
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
			bvid TEXT NOT NULL DEFAULT '',
			fetched_at TEXT NOT NULL DEFAULT '',
			score REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (source_id, video_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_videos_status ON videos(status, published)`,
		`CREATE TABLE IF NOT EXISTS video_files (
			source_id INTEGER NOT NULL,
			video_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			url TEXT NOT NULL,
			ext TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (source_id, video_id, kind, url)
		)`,
		`CREATE TABLE IF NOT EXISTS media_files (
			source_id INTEGER NOT NULL,
			video_id TEXT NOT NULL,
			url TEXT NOT NULL,
			path TEXT NOT NULL DEFAULT '',
			sha256 TEXT NOT NULL DEFAULT '',
			size_bytes INTEGER NOT NULL DEFAULT 0,
			ext TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (source_id, video_id, url)
		)`,
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}
	// migration: bvid (upload tracking) postdates the original schema, and
	// CREATE TABLE IF NOT EXISTS never touches existing tables, so add the
	// column explicitly when an old DB is opened (guard: only when missing).
	if err := s.ensureColumn("videos", "bvid", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	// migration: transcript_score (relevance gate) postdates the original
	// schema too; default 0 = unscored/no transcript.
	if err := s.ensureColumn("videos", "transcript_score", "REAL NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	// migration: score (discover relevance ranking) postdates the original
	// schema too; default 0 = unscored (legacy rows keep oldest-first order).
	if err := s.ensureColumn("videos", "score", "REAL NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if err := s.ensureColumn("sources", "topics", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}

// ensureColumn adds a column to an existing table when it is missing
// (CREATE TABLE IF NOT EXISTS cannot migrate old databases).
func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl))
	return err
}

// ---- sources ----

func (s *Store) AddSource(kind, url, query, name, topics string) (int64, error) {
	site := siteFor(kind)
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO sources (kind,url,query,name,topics,site,created) VALUES (?,?,?,?,?,?,?)`,
		kind, url, query, name, topics, site, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		// exists: return existing id
		var eid int64
		if err := s.db.QueryRow(`SELECT id FROM sources WHERE url=?`, url).Scan(&eid); err != nil {
			return 0, err
		}
		return eid, nil
	}
	return id, nil
}

// SetSourceTopics updates a source's topic filter column.
func (s *Store) SetSourceTopics(id int64, topics string) error {
	_, err := s.db.Exec(`UPDATE sources SET topics=? WHERE id=?`, topics, id)
	return err
}

func (s *Store) ListSources(onlyEnabled bool) ([]model.Source, error) {
	q := `SELECT id,kind,url,query,name,topics,site,needs_proxy,enabled,last_enum,enum_count,enum_complete,created FROM sources`
	if onlyEnabled {
		q += ` WHERE enabled=1`
	}
	q += ` ORDER BY id`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Source
	for rows.Next() {
		var src model.Source
		if err := rows.Scan(&src.ID, &src.Kind, &src.URL, &src.Query, &src.Name,
			&src.Topics, &src.Site, &src.NeedsProxy, &src.Enabled, &src.LastEnum, &src.EnumCount,
			&src.EnumComplete, &src.Created); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func (s *Store) GetSource(id int64) (model.Source, error) {
	var src model.Source
	err := s.db.QueryRow(
		`SELECT id,kind,url,query,name,site,needs_proxy,enabled,last_enum,enum_count,enum_complete,created FROM sources WHERE id=?`,
		id).Scan(&src.ID, &src.Kind, &src.URL, &src.Query, &src.Name, &src.Site,
		&src.NeedsProxy, &src.Enabled, &src.LastEnum, &src.EnumCount, &src.EnumComplete, &src.Created)
	return src, err
}

func (s *Store) DeleteSource(id int64) error {
	for _, q := range []string{
		`DELETE FROM media_files WHERE source_id=?`,
		`DELETE FROM video_files WHERE source_id=?`,
		`DELETE FROM videos WHERE source_id=?`,
		`DELETE FROM sources WHERE id=?`,
	} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetSourceEnum(srcID int64, count int64, complete bool) error {
	_, err := s.db.Exec(
		`UPDATE sources SET last_enum=?, enum_count=?, enum_complete=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), count, boolInt(complete), srcID)
	return err
}

// ---- videos ----

// UpsertVideo inserts a discovered video or no-ops if known (PK dedup).
// Returns the result (RowsAffected tells callers whether the row was new).
func (s *Store) UpsertVideo(v model.Video) (sql.Result, error) {
	return s.db.Exec(
		`INSERT OR IGNORE INTO videos (source_id,video_id,url,title,duration,published,channel,status,score,fetched_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		v.SourceID, v.VideoID, v.URL, v.Title, v.Duration, v.Published, v.Channel,
		model.StatusNew, v.Score, time.Now().UTC().Format(time.RFC3339))
}

// UpsertFiles records native-download candidate files for a video.
func (s *Store) UpsertFiles(srcID int64, videoID string, files []model.File) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range files {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO video_files (source_id,video_id,kind,url,ext,size_bytes) VALUES (?,?,?,?,?,?)`,
			srcID, videoID, f.Kind, f.URL, f.Ext, f.Size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetFiles(srcID int64, videoID string) ([]model.File, error) {
	rows, err := s.db.Query(
		`SELECT kind,url,ext,size_bytes FROM video_files WHERE source_id=? AND video_id=? ORDER BY kind`,
		srcID, videoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.File
	for rows.Next() {
		var f model.File
		if err := rows.Scan(&f.Kind, &f.URL, &f.Ext, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertMediaFiles records additional files of a downloaded video (e.g.
// archive-audio tracks beyond the primary).
func (s *Store) UpsertMediaFiles(srcID int64, videoID string, files []model.MediaFile) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range files {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO media_files (source_id,video_id,url,path,sha256,size_bytes,ext) VALUES (?,?,?,?,?,?,?)`,
			srcID, videoID, f.URL, f.Path, f.SHA256, f.SizeBytes, f.Ext); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NextForDownload returns up to n videos ready to process: scored rows
// (discover candidates) first in relevance order (score desc), then unscored
// rows oldest published first (TubeSync lesson: oldest-first so a slow queue
// doesn't starve old videos; also makes the crawl time-unbiased).
// SOURCE FAIRNESS: the unscored pool is interleaved round-robin across
// sources (within-source oldest-first preserved), so one large channel
// (GOTO: 3.4k entries) cannot fill every round — the mix spreads across
// sources. minDur/maxDur (seconds; 0 = unset) pre-filter on the KNOWN
// duration so videos that would only be skipped later don't burn
// queue/limit slots. duration=0 (unknown) always passes.
func (s *Store) NextForDownload(n int, minDur, maxDur int64) ([]model.Video, error) {
	q := `SELECT source_id,video_id,url,title,duration,published,channel,status,attempts,last_error,size_bytes,path,sha256,bvid,fetched_at,transcript_score,score
		 FROM videos WHERE status IN ('new','failed') AND attempts < 5`
	var args []any
	if minDur > 0 || maxDur > 0 {
		q += ` AND (duration = 0`
		if minDur > 0 {
			q += ` OR duration >= ?`
			args = append(args, minDur)
		}
		if maxDur > 0 {
			q += ` OR duration <= ?`
			args = append(args, maxDur)
		}
		q += `)`
	}
	// pull a pool 4× larger than the batch so the round-robin has rows from
	// several sources to interleave (the pool itself is already ordered
	// scored-first, then oldest-first).
	q += ` ORDER BY (score>0) DESC, score DESC, (published='') ASC, published ASC, source_id, video_id LIMIT ?`
	args = append(args, n*4)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pool []model.Video
	for rows.Next() {
		var v model.Video
		if err := rows.Scan(&v.SourceID, &v.VideoID, &v.URL, &v.Title, &v.Duration,
			&v.Published, &v.Channel, &v.Status, &v.Attempts, &v.LastError,
			&v.SizeBytes, &v.Path, &v.SHA256, &v.BVID, &v.FetchedAt, &v.TranscriptScore, &v.Score); err != nil {
			return nil, err
		}
		pool = append(pool, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// interleave: scored (relevance) rows first in pool order, then a
	// round-robin drain across sources (ascending source id, one row per
	// pass) preserving within-source oldest-first order.
	bySource := map[int64][]model.Video{}
	var sourceOrder []int64
	seenSrc := map[int64]bool{}
	for _, v := range pool {
		if v.Score > 0 {
			continue // scored rows are emitted first, directly from the pool
		}
		if !seenSrc[v.SourceID] {
			seenSrc[v.SourceID] = true
			sourceOrder = append(sourceOrder, v.SourceID)
		}
		bySource[v.SourceID] = append(bySource[v.SourceID], v)
	}
	out := make([]model.Video, 0, n)
	for _, v := range pool {
		if v.Score > 0 {
			out = append(out, v)
			if len(out) >= n {
				return out, nil
			}
		}
	}
	for i := 0; len(out) < n; i++ {
		added := false
		for _, sid := range sourceOrder {
			if i < len(bySource[sid]) {
				out = append(out, bySource[sid][i])
				added = true
				if len(out) >= n {
					return out, nil
				}
			}
		}
		if !added {
			break
		}
	}
	return out, nil
}

// NextForUpload returns up to n downloaded videos not yet republished:
// scored rows first in relevance order (score desc), then unscored rows
// oldest published first (same relative order as the download queue, so the
// oldest content leaves the frontier first).
func (s *Store) NextForUpload(n int) ([]model.Video, error) {
	rows, err := s.db.Query(
		`SELECT source_id,video_id,url,title,duration,published,channel,status,attempts,last_error,size_bytes,path,sha256,bvid,fetched_at,transcript_score,score
		 FROM videos WHERE status=? ORDER BY (score>0) DESC, score DESC, (published='') ASC, published ASC, source_id, video_id LIMIT ?`,
		model.StatusDone, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Video
	for rows.Next() {
		var v model.Video
		if err := rows.Scan(&v.SourceID, &v.VideoID, &v.URL, &v.Title, &v.Duration,
			&v.Published, &v.Channel, &v.Status, &v.Attempts, &v.LastError,
			&v.SizeBytes, &v.Path, &v.SHA256, &v.BVID, &v.FetchedAt, &v.TranscriptScore, &v.Score); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UploadMarked records a successful bilibili upload: the video leaves the
// upload queue and is never re-uploaded.
func (s *Store) UploadMarked(srcID int64, videoID, bvid string) error {
	_, err := s.db.Exec(
		`UPDATE videos SET status=?, bvid=? WHERE source_id=? AND video_id=?`,
		model.StatusUploaded, bvid, srcID, videoID)
	return err
}
func (s *Store) CountByStatus() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM videos GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var st string
		var c int64
		if err := rows.Scan(&st, &c); err != nil {
			return nil, err
		}
		out[st] = c
	}
	return out, rows.Err()
}

// VideoRows lists videos filtered by status ("" = all).
func (s *Store) VideoRows(status string, limit int) ([]model.Video, error) {
	q := `SELECT source_id,video_id,url,title,duration,published,channel,status,attempts,last_error,size_bytes,path,sha256,bvid,fetched_at,transcript_score,score FROM videos`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY published DESC, source_id, video_id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Video
	for rows.Next() {
		var v model.Video
		if err := rows.Scan(&v.SourceID, &v.VideoID, &v.URL, &v.Title, &v.Duration,
			&v.Published, &v.Channel, &v.Status, &v.Attempts, &v.LastError,
			&v.SizeBytes, &v.Path, &v.SHA256, &v.BVID, &v.FetchedAt, &v.TranscriptScore, &v.Score); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpdateMeta persists metadata learned at enrichment time (bilibili flat
// mode yields no dates; GetMeta fills them so ordering works next pass).
func (s *Store) UpdateMeta(srcID int64, videoID, title string, duration int64, published, channel string) error {
	_, err := s.db.Exec(
		`UPDATE videos SET title=?, duration=?, published=?, channel=? WHERE source_id=? AND video_id=?`,
		title, duration, published, channel, srcID, videoID)
	return err
}

// MarkDownloaded records a successful download.
func (s *Store) MarkDownloaded(v model.Video) error {
	_, err := s.db.Exec(
		`UPDATE videos SET status=?, title=?, duration=?, published=?, channel=?, size_bytes=?, path=?, sha256=?, transcript_score=?, attempts=attempts+1, last_error='', fetched_at=? WHERE source_id=? AND video_id=?`,
		model.StatusDone, v.Title, v.Duration, v.Published, v.Channel, v.SizeBytes,
		v.Path, v.SHA256, v.TranscriptScore, time.Now().UTC().Format(time.RFC3339), v.SourceID, v.VideoID)
	return err
}

func (s *Store) MarkSkipped(srcID int64, videoID, reason string) error {
	_, err := s.db.Exec(
		`UPDATE videos SET status=?, last_error=?, attempts=attempts+1 WHERE source_id=? AND video_id=?`,
		model.StatusSkipped, reason, srcID, videoID)
	return err
}

func (s *Store) MarkFailed(srcID int64, videoID, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE videos SET status=?, last_error=?, attempts=attempts+1 WHERE source_id=? AND video_id=?`,
		model.StatusFailed, errMsg, srcID, videoID)
	return err
}

// Meta helpers.
func (s *Store) MetaGet(k string) string {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

func (s *Store) MetaSet(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta (k,v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func siteFor(kind string) string {
	switch kind {
	case model.KindYoutubeChannel, model.KindYoutubePlaylist:
		return "youtube"
	case model.KindBilibiliSpace, model.KindBilibiliFav:
		return "bilibili"
	case model.KindPeertubeChannel, model.KindPeertubeSearch:
		return "peertube"
	case model.KindCCCConf, model.KindCCCSearch:
		return "ccc"
	case model.KindArchiveQuery, model.KindArchiveAudio:
		return "archive"
	case model.KindGallica:
		return "gallica"
	case model.KindRSS:
		return "rss"
	case model.KindDiscover:
		return "youtube"
	}
	return ""
}
