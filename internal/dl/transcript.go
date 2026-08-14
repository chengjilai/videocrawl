// Transcript extraction: the subtitle file yt-dlp wrote next to a video is
// the closest thing to a full-text transcript we get without a speech-to-text
// pass. transcriptText finds the id-prefixed sub (srt/vtt/ass) under
// <outDir>/<channel>/ or <outDir>/.tmp/, prefers the LONGEST of the two
// variants YouTube yields (manual <id>.en.srt vs auto <id>.en-orig.srt),
// strips cue indices/timestamps/WEBVTT header/HTML tags and returns the plain
// text ("" when no sub exists).
package dl

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// subExts: subtitle formats we can extract plain text from.
var subExts = map[string]bool{".srt": true, ".vtt": true, ".ass": true}

var (
	// htmlTagRe strips <i>, <font color=...> etc. (srt/vtt markup).
	htmlTagRe = regexp.MustCompile(`<[^>]*>`)
	// cueTimeRe matches a cue timestamp line: "00:00:01,000 --> ..." (srt),
	// "00:00:01.000 --> ..." and "00:00:01 --> ..." (vtt), with optional
	// cue settings after the arrow.
	cueTimeRe = regexp.MustCompile(`^\s*\d{1,2}:\d{2}(:\d{2})?[,.]\d{0,3}\s*-->\s*`)
	// cueIdxRe matches a bare srt cue index line ("1", "42").
	cueIdxRe = regexp.MustCompile(`^\d{1,4}$`)
	// assOverrideRe strips {\an8}-style override tags.
	assOverrideRe = regexp.MustCompile(`\{[^}]*\}`)
)

// transcriptText returns the plain text of the best (longest) id-prefixed
// subtitle file for videoID, or "" when none exists or none can be read.
func transcriptText(outDir, videoID string) string {
	path := findSubFile(outDir, videoID)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return extractTranscript(path, string(data))
}

// findSubFile returns the longest id-prefixed subtitle file (srt/vtt/ass)
// for videoID anywhere under outDir: the channel dirs (yt-dlp output
// template %(channel)s/...) and the .tmp/ staging dir (recursive — the
// staged path mirrors the output template, .tmp/<channel>/<file>). Size is
// the discriminator because YouTube manual subs (.en.srt) are usually
// longer than the auto ones (.en-orig.srt).
func findSubFile(outDir, videoID string) string {
	var best string
	var bestSize int64
	scanDir := func(dir string) {
		files, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasPrefix(name, videoID+"_") && !strings.HasPrefix(name, videoID+".") {
				continue
			}
			if !subExts[strings.ToLower(filepath.Ext(name))] {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.Size() > bestSize {
				bestSize = info.Size()
				best = filepath.Join(dir, name)
			}
		}
	}
	// channel dirs under outDir
	if entries, err := os.ReadDir(outDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != ".tmp" {
				scanDir(filepath.Join(outDir, e.Name()))
			}
		}
	}
	// .tmp staging dir, recursively (the channel path is preserved under it)
	if err := filepath.WalkDir(filepath.Join(outDir, ".tmp"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, videoID+"_") && !strings.HasPrefix(name, videoID+".") {
			return nil
		}
		if !subExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > bestSize {
			bestSize = info.Size()
			best = path
		}
		return nil
	}); err != nil {
		// .tmp missing is fine (only staged subs live there)
	}
	return best
}

// extractTranscript converts subtitle content to plain text. ass keeps only
// the Dialogue payload; srt/vtt drop the WEBVTT header, NOTE/STYLE blocks,
// cue indices and timestamp lines.
func extractTranscript(path, data string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ass":
		return extractASS(data)
	}
	return extractSRTVTT(data)
}

func extractSRTVTT(data string) string {
	var out []string
	inNote, inStyle := false, false
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(htmlTagRe.ReplaceAllString(sc.Text(), ""))
		if line == "" {
			inNote, inStyle = false, false
			continue
		}
		if inStyle {
			if strings.HasPrefix(line, "}") {
				inStyle = false
			}
			continue
		}
		if inNote {
			continue
		}
		switch {
		case strings.HasPrefix(line, "WEBVTT"): // header line
		case strings.HasPrefix(line, "Kind:"), strings.HasPrefix(line, "Language:"), strings.HasPrefix(line, "X-"):
			// vtt header fields (Kind: captions, Language: en,
			// X-TIMESTAMP-MAP=...)
		case line == "NOTE":
			inNote = true
		case line == "STYLE":
			inStyle = true
		case cueTimeRe.MatchString(line) || strings.Contains(line, "-->"):
			// cue timestamp line (with or without settings)
		case cueIdxRe.MatchString(line):
			// srt cue index
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, " ")
}

func extractASS(data string) string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(htmlTagRe.ReplaceAllString(sc.Text(), ""))
		if !strings.HasPrefix(line, "Dialogue:") {
			continue
		}
		// Format: Layer, Start, End, Style, Name, MarginL, MarginR,
		// MarginV, Effect, Text — 9 commas before the payload.
		parts := strings.SplitN(line, ",", 10)
		if len(parts) < 10 {
			continue
		}
		text := strings.TrimSpace(assOverrideRe.ReplaceAllString(parts[9], ""))
		text = strings.ReplaceAll(text, `\N`, " ")
		if text != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, " ")
}
