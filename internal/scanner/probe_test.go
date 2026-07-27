package scanner

import "testing"

const sampleProbe = `{
  "format": {"duration": "5400.25", "bit_rate": "12000000"},
  "streams": [
    {"codec_type": "video", "codec_name": "hevc", "width": 3840, "height": 2160, "color_transfer": "smpte2084"},
    {"codec_type": "audio", "codec_name": "ac3", "channels": 6}
  ],
  "chapters": [
    {"start_time": "0.000000", "tags": {"title": "Opening"}},
    {"start_time": "754.500000", "tags": {}},
    {"start_time": "2100.000000", "tags": {"title": "Finale"}}
  ]
}`

func TestParseProbe(t *testing.T) {
	info, err := parseProbe([]byte(sampleProbe), "movies/Film.2020.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if info.VideoCodec != "hevc" || info.HDR != "hdr10" || info.Container != "mkv" {
		t.Errorf("got %+v", info)
	}
	if info.AudioCodec != "ac3" || info.AudioChannels != 6 {
		t.Errorf("got audio %s/%d", info.AudioCodec, info.AudioChannels)
	}
	// Single audio stream still yields a one-entry track list.
	if len(info.AudioTracks) != 1 || info.AudioTracks[0].Codec != "ac3" || info.AudioTracks[0].Channels != 6 {
		t.Errorf("audio tracks: %+v", info.AudioTracks)
	}
	if len(info.Chapters) != 3 {
		t.Fatalf("expected 3 chapters, got %d", len(info.Chapters))
	}
	if info.Chapters[1].StartSeconds != 754.5 {
		t.Errorf("chapter start: %v", info.Chapters[1])
	}
	// Untitled chapters get a numbered fallback so the UI always has a label.
	if info.Chapters[1].Title != "Chapter 2" {
		t.Errorf("chapter title fallback: %q", info.Chapters[1].Title)
	}
}

const sampleProbeWithSubs = `{
  "format": {"duration": "1200", "bit_rate": "5000000"},
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080},
    {"codec_type": "audio", "codec_name": "aac", "channels": 2, "disposition": {"default": 1}, "tags": {"language": "eng", "title": "Stereo"}},
    {"codec_type": "subtitle", "codec_name": "subrip", "tags": {"language": "eng", "title": "English"}},
    {"codec_type": "audio", "codec_name": "ac3", "channels": 6, "disposition": {"default": 0}, "tags": {"language": "fre"}},
    {"codec_type": "subtitle", "codec_name": "hdmv_pgs_subtitle", "tags": {"language": "fre"}},
    {"codec_type": "subtitle", "codec_name": "ass", "tags": {}}
  ]
}`

func TestParseProbeSubtitles(t *testing.T) {
	info, err := parseProbe([]byte(sampleProbeWithSubs), "tv/Show.S01E01.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Subtitles) != 3 {
		t.Fatalf("expected 3 subtitle tracks, got %d: %+v", len(info.Subtitles), info.Subtitles)
	}
	// Ordinals count subtitle streams only — the interleaved audio stream
	// must not shift them (they map to ffmpeg -map 0:s:<ordinal>).
	first := info.Subtitles[0]
	if first.Ordinal != 0 || first.Codec != "subrip" || first.Language != "eng" || first.Title != "English" || !first.Supported {
		t.Errorf("track 0: %+v", first)
	}
	// Bitmap subtitles are listed but unsupported (no OCR).
	pgs := info.Subtitles[1]
	if pgs.Ordinal != 1 || pgs.Codec != "hdmv_pgs_subtitle" || pgs.Language != "fre" || pgs.Supported {
		t.Errorf("track 1: %+v", pgs)
	}
	if ass := info.Subtitles[2]; ass.Ordinal != 2 || !ass.Supported {
		t.Errorf("track 2: %+v", ass)
	}
}

// TestParseProbeAudioTracks: every audio stream is listed, ordinals counting
// audio streams only (-> -map 0:a:<n>, the interleaved subtitle stream must
// not shift them), while the first-stream scalars stay unchanged for the
// decision engine.
func TestParseProbeAudioTracks(t *testing.T) {
	info, err := parseProbe([]byte(sampleProbeWithSubs), "tv/Show.S01E01.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if info.AudioCodec != "aac" || info.AudioChannels != 2 {
		t.Errorf("scalar audio fields must stay first-stream: %s/%d", info.AudioCodec, info.AudioChannels)
	}
	if len(info.AudioTracks) != 2 {
		t.Fatalf("expected 2 audio tracks, got %+v", info.AudioTracks)
	}
	first := info.AudioTracks[0]
	if first.Ordinal != 0 || first.Codec != "aac" || first.Channels != 2 ||
		first.Language != "eng" || first.Title != "Stereo" || !first.Default {
		t.Errorf("track 0: %+v", first)
	}
	second := info.AudioTracks[1]
	if second.Ordinal != 1 || second.Codec != "ac3" || second.Channels != 6 ||
		second.Language != "fre" || second.Default {
		t.Errorf("track 1: %+v", second)
	}
}

func TestParseProbeNoSubtitlesOmitted(t *testing.T) {
	info, err := parseProbe([]byte(sampleProbe), "movies/Film.2020.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if info.Subtitles != nil {
		t.Errorf("expected nil subtitles (omitted from JSON), got %+v", info.Subtitles)
	}
}

func TestParseProbeNoVideoStream(t *testing.T) {
	if _, err := parseProbe([]byte(`{"streams":[{"codec_type":"audio","codec_name":"mp3"}]}`), "x.mp4"); err == nil {
		t.Error("expected error for file with no video stream")
	}
}
