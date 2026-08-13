// Package model: shared types for the video crawler.
package model

// Source kinds (enumeration strategies).
const (
	KindYoutubeChannel  = "youtube-channel"  // channel URL / handle / UC id
	KindYoutubePlaylist = "youtube-playlist" // any playlist URL
	KindBilibiliSpace   = "bilibili-space"   // space.bilibili.com/{mid}/video
	KindBilibiliFav     = "bilibili-fav"     // favorites list: medialist/detail/ml<fid> (收藏夹)
	KindPeertubeChannel = "peertube-channel" // {instance}/video-channels/{handle}
	KindPeertubeSearch  = "peertube-search"  // {instance} + query
	KindCCCConf         = "ccc-conf"         // media.ccc.de conference acronym
	KindCCCSearch       = "ccc-search"       // media.ccc.de event search
	KindArchiveQuery    = "archive-query"    // archive.org lucene query
	KindRSS             = "rss"              // feed URL with video enclosures
)

// Source: a seed to enumerate.
type Source struct {
	ID           int64
	Kind         string
	URL          string // seed url (kind-specific)
	Query        string // extra param: peertube search q, ccc search q, archive q
	Name         string
	Site         string // site config key: youtube|bilibili|peertube|ccc|archive|rss
	NeedsProxy   bool   // route enumeration through the proxy
	Enabled      bool
	LastEnum     string // ISO time
	EnumCount    int64
	EnumComplete bool // last enumeration reached the end (no truncation)
	Created      string
}

// Video: one discovered item in the frontier.
type Video struct {
	SourceID  int64
	VideoID   string
	URL       string
	Title     string
	Duration  int64  // seconds, 0 = unknown
	Published string // ISO date
	Channel   string
	Status    string // new|done|skipped|failed
	Attempts  int
	LastError string
	SizeBytes int64
	Path      string
	SHA256    string
	FetchedAt string
}

// Statuses.
const (
	StatusNew     = "new"
	StatusDone    = "done"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"
)

// File: one downloadable media/subtitle file.
type File struct {
	URL  string
	Size int64
	Ext  string // mp4|webm|srt|vtt
	Kind string // video|sub
}
