package dl

import (
	"testing"
)

func TestPickAudioFilesPrefersMP3(t *testing.T) {
	files := []iaFile{
		{Name: "01 Track.flac"},
		{Name: "01 Track.mp3"},
		{Name: "02 Track.flac"},
		{Name: "02 Track.mp3"},
		{Name: "lecture.mp3"},       // not a track
		{Name: "01 Track_64kb.mp3"}, // derived low-bitrate copy
		{Name: ".hidden.mp3"},       // junk
		{Name: "meta.xml"},          // not audio
	}
	got := PickAudioFiles(files)
	if len(got) != 2 {
		t.Fatalf("want 2 tracks, got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Name != "01 Track.mp3" && f.Name != "02 Track.mp3" {
			t.Errorf("unexpected pick %q (mp3 tier should win)", f.Name)
		}
	}
}

func TestPickAudioFilesFallsBackToFlac(t *testing.T) {
	files := []iaFile{
		{Name: "01 Track.flac"},
		{Name: "02 Track.flac"},
	}
	got := PickAudioFiles(files)
	if len(got) != 2 || got[0].Name != "01 Track.flac" {
		t.Fatalf("want flac fallback, got %+v", got)
	}
}

func TestPickAudioFilesNone(t *testing.T) {
	if got := PickAudioFiles([]iaFile{{Name: "cover.jpg"}}); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}
