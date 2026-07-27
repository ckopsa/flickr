package scanner

import (
	"testing"

	"flickr/internal/model"
)

func TestMatchSidecars(t *testing.T) {
	subs := []sidecarFile{
		{Key: "Movies/Movie (2020)/Movie.srt", ETag: "a"},
		{Key: "Movies/Movie (2020)/Movie.en.srt", ETag: "b"},
		{Key: "Movies/Movie (2020)/Movie.en.forced.srt", ETag: "c"},
		{Key: "Movies/Movie (2020)/Movie 2.srt", ETag: "d"},    // prefix but not "stem."
		{Key: "Movies/Other (2021)/Movie.srt", ETag: "e"},      // wrong directory
		{Key: "Movies/Movie (2020)/Trailer.en.srt", ETag: "f"}, // different stem
	}
	got := matchSidecars("Movies/Movie (2020)/Movie.mkv", subs)
	if len(got) != 3 {
		t.Fatalf("matched %d sidecars, want 3: %+v", len(got), got)
	}
	// Sorted by key for deterministic ordinals/signature.
	want := []string{
		"Movies/Movie (2020)/Movie.en.forced.srt",
		"Movies/Movie (2020)/Movie.en.srt",
		"Movies/Movie (2020)/Movie.srt",
	}
	for i, w := range want {
		if got[i].Key != w {
			t.Errorf("match[%d] = %q, want %q", i, got[i].Key, w)
		}
	}
	if m := matchSidecars("Movies/Movie (2020)/Movie 2.mkv", subs); len(m) != 1 || m[0].ETag != "d" {
		t.Errorf("'Movie 2' should match only its own sidecar: %+v", m)
	}
}

func TestSidecarSignature(t *testing.T) {
	if sig := sidecarSignature(nil); sig != "" {
		t.Errorf("empty set signature = %q, want empty (matches column default)", sig)
	}
	a := sidecarSignature([]sidecarFile{{Key: "x/a.srt", ETag: "1"}})
	b := sidecarSignature([]sidecarFile{{Key: "x/a.srt", ETag: "2"}})
	c := sidecarSignature([]sidecarFile{{Key: "x/a.srt", ETag: "1"}, {Key: "x/b.srt", ETag: "1"}})
	if a == b {
		t.Error("etag change must change signature")
	}
	if a == c {
		t.Error("added sidecar must change signature")
	}
	if a != sidecarSignature([]sidecarFile{{Key: "x/a.srt", ETag: "1"}}) {
		t.Error("signature must be deterministic")
	}
}

func TestIsSubtitleKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"tv/Show/S01E01.en.srt", true},
		{"tv/Show/S01E01.ass", true},
		{"tv/Show/S01E01.ssa", true},
		{"tv/Show/S01E01.vtt", true},
		{"tv/Show/S01E01.SRT", true},    // extension casing
		{"tv/Show/S01E01.sub", false},   // ambiguous MicroDVD/VobSub — not served
		{"tv/Show/._S01E01.srt", false}, // AppleDouble junk
		{"tv/Show/S01E01.mkv", false},   // a video
		{"tv/Show/S01E01.srt.bak", false},
	}
	for _, c := range cases {
		if got := isSubtitleKey(c.key); got != c.want {
			t.Errorf("isSubtitleKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestSidecarLanguage(t *testing.T) {
	cases := []struct {
		suffix, want string
	}{
		{"en", "eng"},
		{"eng", "eng"},
		{"en.forced", "eng"},
		{"forced.en", "eng"}, // order-independent: first recognized token wins
		{"es", "spa"},
		{"fra", "fra"}, // 639-2/T spelling passes through
		{"sdh", ""},    // not a language
		{"", ""},
	}
	for _, c := range cases {
		if got := sidecarLanguage(c.suffix); got != c.want {
			t.Errorf("sidecarLanguage(%q) = %q, want %q", c.suffix, got, c.want)
		}
	}
}

func TestExternalTracks(t *testing.T) {
	subs := []sidecarFile{
		{Key: "Movies/Movie/Movie.en.forced.srt", ETag: "a"},
		{Key: "Movies/Movie/Movie.es.ass", ETag: "b"},
		{Key: "Movies/Movie/Movie.vtt", ETag: "c"},
	}
	got := externalTracks("Movies/Movie/Movie.mkv", subs, 2)
	if len(got) != 3 {
		t.Fatalf("got %d tracks: %+v", len(got), got)
	}
	want := []model.SubtitleTrack{
		{Ordinal: 2, Codec: "subrip", Language: "eng", Title: "en.forced", Supported: true, External: true, ObjectKey: "Movies/Movie/Movie.en.forced.srt"},
		{Ordinal: 3, Codec: "ass", Language: "spa", Title: "es", Supported: true, External: true, ObjectKey: "Movies/Movie/Movie.es.ass"},
		{Ordinal: 4, Codec: "webvtt", Language: "", Title: "Movie.vtt", Supported: true, External: true, ObjectKey: "Movies/Movie/Movie.vtt"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("track %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAttachSidecars(t *testing.T) {
	info := &model.MediaInfo{Subtitles: []model.SubtitleTrack{
		{Ordinal: 0, Codec: "subrip", Language: "eng", Supported: true},
		{Ordinal: 1, Codec: "hdmv_pgs_subtitle", Language: "fre"},
		// A stale external entry from a previous attach — must be replaced.
		{Ordinal: 2, Codec: "subrip", External: true, ObjectKey: "old/gone.srt", Supported: true},
	}}
	attachSidecars(info, "x/Video.mkv", []sidecarFile{{Key: "x/Video.de.srt", ETag: "e"}})
	if len(info.Subtitles) != 3 {
		t.Fatalf("got %d tracks: %+v", len(info.Subtitles), info.Subtitles)
	}
	// Embedded tracks and their ordinals untouched.
	if info.Subtitles[0].Ordinal != 0 || info.Subtitles[1].Codec != "hdmv_pgs_subtitle" {
		t.Errorf("embedded tracks disturbed: %+v", info.Subtitles[:2])
	}
	ext := info.Subtitles[2]
	if !ext.External || ext.Ordinal != 2 || ext.ObjectKey != "x/Video.de.srt" || ext.Language != "ger" {
		t.Errorf("external track: %+v", ext)
	}

	// Removing all sidecars strips external entries and restores JSON
	// omission for the empty case.
	attachSidecars(info, "x/Video.mkv", nil)
	if len(info.Subtitles) != 2 {
		t.Errorf("after removal: %+v", info.Subtitles)
	}
	empty := &model.MediaInfo{Subtitles: []model.SubtitleTrack{{Ordinal: 0, External: true}}}
	attachSidecars(empty, "x/Video.mkv", nil)
	if empty.Subtitles != nil {
		t.Errorf("expected nil subtitles after removing the only (external) track, got %+v", empty.Subtitles)
	}
	// Nil info (probe-error rows) must be a no-op, not a panic.
	attachSidecars(nil, "x/Video.mkv", nil)
}
