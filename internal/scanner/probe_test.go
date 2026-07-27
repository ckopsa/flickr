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

func TestParseProbeNoVideoStream(t *testing.T) {
	if _, err := parseProbe([]byte(`{"streams":[{"codec_type":"audio","codec_name":"mp3"}]}`), "x.mp4"); err == nil {
		t.Error("expected error for file with no video stream")
	}
}
