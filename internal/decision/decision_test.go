package decision

import (
	"testing"

	"flickr/internal/model"
)

var chromecastV1 = model.ClientCapabilities{
	SchemaVersion: 1,
	Containers:    []string{"mp4"},
	VideoCodecs:   []string{"h264"},
	AudioCodecs:   []string{"aac", "mp3"},
	MaxWidth:      1920, MaxHeight: 1080,
	MaxAudioChannels: 2,
}

var modernTV = model.ClientCapabilities{
	SchemaVersion: 1,
	Containers:    []string{"mp4", "mkv"},
	VideoCodecs:   []string{"h264", "hevc", "av1"},
	AudioCodecs:   []string{"aac", "ac3", "eac3"},
	MaxWidth:      3840, MaxHeight: 2160,
	SupportsHDR:      []string{"hdr10", "hlg"},
	MaxAudioChannels: 8,
	HLSAudioCodecs:   []string{"aac", "ac3", "eac3"},
}

func hevc4kHDR() model.MediaInfo {
	return model.MediaInfo{
		Container: "mkv", VideoCodec: "hevc", AudioCodec: "ac3",
		Width: 3840, Height: 2160, DurationSeconds: 7200,
		BitrateBps: 25_000_000, HDR: "hdr10", AudioChannels: 6,
	}
}

func h264Compatible() model.MediaInfo {
	return model.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, DurationSeconds: 5400,
		BitrateBps: 6_000_000, AudioChannels: 2,
	}
}

func TestDirectPlayWhenFullyCompatible(t *testing.T) {
	d := Decide(h264Compatible(), chromecastV1, model.DefaultPolicy())
	if d.Method != model.DirectPlay {
		t.Fatalf("expected direct_play, got %s (trace: %+v)", d.Method, d.Trace)
	}
	if d.Target != nil {
		t.Fatalf("direct play must not carry a transcode target")
	}
}

func TestHDR4KToChromecastTonemapsAndScales(t *testing.T) {
	// The canonical case: 4K HEVC HDR10 file + h264-only 1080p client
	// → expect tone-mapped, downscaled h264 transcode.
	d := Decide(hevc4kHDR(), chromecastV1, model.DefaultPolicy())
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode, got %s", d.Method)
	}
	if d.Target.VideoCodec != "h264" {
		t.Errorf("expected h264 target, got %q", d.Target.VideoCodec)
	}
	if !d.Target.Tonemap {
		t.Error("expected tone-mapping for HDR file on SDR client")
	}
	if d.Target.Height != 1080 {
		t.Errorf("expected 1080p target, got %d", d.Target.Height)
	}
	if d.Target.AudioCodec != "aac" {
		t.Errorf("expected aac audio (ac3 6ch unsupported), got %q", d.Target.AudioCodec)
	}
}

func TestHDRFileDirectPlaysOnHDRTV(t *testing.T) {
	d := Decide(hevc4kHDR(), modernTV, model.DefaultPolicy())
	if d.Method != model.DirectPlay {
		t.Fatalf("expected direct_play on capable TV, got %s (trace: %+v)", d.Method, d.Trace)
	}
}

func TestBandwidthCapForcesVideoReencodeOnly(t *testing.T) {
	caps := modernTV
	caps.MaxBitrateBps = 3_000_000 // phone on cellular
	d := Decide(hevc4kHDR(), caps, model.DefaultPolicy())
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode under bitrate cap, got %s", d.Method)
	}
	if d.Target.VideoCodec == "" {
		t.Error("bitrate cap must force a video re-encode")
	}
	if d.Target.AudioCodec != "" {
		t.Error("audio is compatible and should be copied")
	}
	if d.Target.Tonemap {
		t.Error("client supports hdr10; no tone-map needed")
	}
}

func TestAudioOnlyTranscodeCopiesVideo(t *testing.T) {
	m := h264Compatible()
	m.AudioCodec = "dts"
	d := Decide(m, chromecastV1, model.DefaultPolicy())
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode, got %s", d.Method)
	}
	if d.Target.VideoCodec != "" {
		t.Error("video is compatible and should be copied")
	}
	if d.Target.AudioCodec != "aac" {
		t.Errorf("expected aac audio target, got %q", d.Target.AudioCodec)
	}
}

func TestDenyWhenPolicyForbidsTranscode(t *testing.T) {
	policy := model.DefaultPolicy()
	policy.AllowTranscode = false
	d := Decide(hevc4kHDR(), chromecastV1, policy)
	if d.Method != model.Deny {
		t.Fatalf("expected deny, got %s", d.Method)
	}
}

func TestTraceExplainsEveryFailure(t *testing.T) {
	d := Decide(hevc4kHDR(), chromecastV1, model.DefaultPolicy())
	failed := map[string]bool{}
	for _, s := range d.Trace {
		if !s.Passed {
			failed[s.Check] = true
		}
	}
	for _, want := range []string{"container", "video_codec", "audio_codec", "resolution", "hdr", "audio_channels"} {
		if !failed[want] {
			t.Errorf("trace missing failed check %q: %+v", want, d.Trace)
		}
	}
}

func TestHevcCopyNeedsFmp4Consent(t *testing.T) {
	m := hevc4kHDR() // hevc video, ac3 audio — TV plays hevc but not the container
	caps := modernTV
	caps.Containers = []string{"mp4"} // force a remux
	caps.SupportsHDR = []string{"hdr10"}
	caps.HLSSegmentFormats = []string{"ts", "fmp4"}
	d := Decide(m, caps, model.DefaultPolicy())
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode, got %s (trace %+v)", d.Method, d.Trace)
	}
	if d.Target.VideoCodec != "" {
		t.Errorf("hevc should be copied for an fmp4-capable client, got re-encode to %q", d.Target.VideoCodec)
	}
	if d.Target.SegmentFormat != "fmp4" {
		t.Errorf("copied hevc must ship in fmp4 segments, got %q", d.Target.SegmentFormat)
	}
}

func TestHevcForcesReencodeForTsOnlyClient(t *testing.T) {
	m := hevc4kHDR()
	caps := modernTV
	caps.Containers = []string{"mp4"}
	caps.SupportsHDR = []string{"hdr10"}
	caps.HLSSegmentFormats = []string{"ts"} // e.g. an Android-TV cast receiver
	d := Decide(m, caps, model.DefaultPolicy())
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode, got %s", d.Method)
	}
	if d.Target.VideoCodec != "h264" {
		t.Errorf("ts-only client + hevc source must re-encode to h264, got %q", d.Target.VideoCodec)
	}
	if d.Target.SegmentFormat != "ts" {
		t.Errorf("expected ts segments, got %q", d.Target.SegmentFormat)
	}
}

func TestDirectPlayCodecNotCopiedIntoHLS(t *testing.T) {
	// The BRAVIA case: device decodes AC-3 for direct play, but its cast
	// receiver rejects AC-3 inside TS segments. In-stream support is a
	// separate capability; without it the audio must be re-encoded.
	m := model.MediaInfo{
		Container: "mkv", VideoCodec: "mpeg2video", AudioCodec: "ac3",
		Width: 720, Height: 480, DurationSeconds: 6990,
		BitrateBps: 7_000_000, AudioChannels: 6,
	}
	caps := model.ClientCapabilities{
		SchemaVersion: 1,
		Containers:    []string{"mp4", "webm"},
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac", "ac3", "eac3"}, // direct play: yes
		MaxAudioChannels: 6,
		HLSAudioCodecs:   []string{"aac"}, // in-stream: aac only
	}
	d := Decide(m, caps, model.DefaultPolicy())
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode, got %s", d.Method)
	}
	if d.Target.AudioCodec != "aac" {
		t.Errorf("ac3 must be re-encoded for an aac-only HLS player, got copy (%q)", d.Target.AudioCodec)
	}
	found := false
	for _, s := range d.Trace {
		if s.Check == "hls_audio" && !s.Passed {
			found = true
		}
	}
	if !found {
		t.Errorf("trace must explain the in-stream audio re-encode: %+v", d.Trace)
	}
}

func TestTelecineSourceGetsInverseTelecineOnReencode(t *testing.T) {
	m := model.MediaInfo{
		Container: "mkv", VideoCodec: "mpeg2video", AudioCodec: "ac3",
		Width: 720, Height: 480, DurationSeconds: 6990,
		BitrateBps: 7_000_000, AudioChannels: 6,
		FPS: 29.97, Telecine: true,
	}
	d := Decide(m, chromecastV1, model.DefaultPolicy())
	if d.Method != model.Transcode || d.Target.VideoCodec == "" {
		t.Fatalf("expected video re-encode, got %+v", d)
	}
	if !d.Target.Detelecine {
		t.Error("telecined source must be inverse-telecined when re-encoding")
	}
	if d.Target.FPS < 23.9 || d.Target.FPS > 24.1 {
		t.Errorf("expected ~23.976 output fps, got %v", d.Target.FPS)
	}
}

func TestTelecineFlagsPassThroughOnCopy(t *testing.T) {
	// Compatible client: video is copied; pulldown flags travel with the
	// stream and the player applies them — no detelecine.
	m := model.MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 720, Height: 480, DurationSeconds: 5400,
		BitrateBps: 5_000_000, AudioChannels: 2,
		FPS: 29.97, Telecine: true,
	}
	d := Decide(m, chromecastV1, model.DefaultPolicy())
	if d.Method != model.DirectPlay {
		t.Fatalf("expected direct play, got %s", d.Method)
	}
}

func TestH264TranscodeUsesTsByDefault(t *testing.T) {
	d := Decide(hevc4kHDR(), chromecastV1, model.DefaultPolicy())
	if d.Target.SegmentFormat != "ts" {
		t.Errorf("h264 output should default to ts segments, got %q", d.Target.SegmentFormat)
	}
}

func TestSparseCapabilityManifestGetsDefaults(t *testing.T) {
	// A minimal client manifest (old schema, few fields) must not panic
	// or produce nonsense — Normalize fills defaults.
	caps := model.ClientCapabilities{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
	}
	d := Decide(h264Compatible(), caps, model.DefaultPolicy())
	if d.Method != model.DirectPlay {
		t.Fatalf("expected direct_play with defaulted limits, got %s (trace: %+v)", d.Method, d.Trace)
	}
}
