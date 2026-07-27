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
		SchemaVersion:    1,
		Containers:       []string{"mp4", "webm"},
		VideoCodecs:      []string{"h264"},
		AudioCodecs:      []string{"aac", "ac3", "eac3"}, // direct play: yes
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

func TestVideoReencodeGetsABRLadder(t *testing.T) {
	// 4K HEVC HDR + h264-only 1080p client: video re-encode to 1080p, so the
	// ladder is primary 1080p plus the 720p and 480p rungs.
	d := Decide(hevc4kHDR(), chromecastV1, model.DefaultPolicy())
	want := []model.Rendition{
		{Height: 1080, VideoBitrateBps: 8_000_000},
		{Height: 720, VideoBitrateBps: 3_000_000},
		{Height: 480, VideoBitrateBps: 1_200_000},
	}
	if len(d.Target.Renditions) != len(want) {
		t.Fatalf("expected %d rungs, got %+v", len(want), d.Target.Renditions)
	}
	for i, r := range want {
		if d.Target.Renditions[i] != r {
			t.Errorf("rung %d: got %+v, want %+v", i, d.Target.Renditions[i], r)
		}
	}
	found := false
	for _, s := range d.Trace {
		if s.Check == "abr_ladder" && s.Passed {
			found = true
		}
	}
	if !found {
		t.Errorf("trace must list the ladder: %+v", d.Trace)
	}
}

func TestLadderDropsRungsAtOrAbovePrimary(t *testing.T) {
	caps := chromecastV1
	caps.MaxWidth, caps.MaxHeight = 1280, 720 // primary target becomes 720p
	d := Decide(hevc4kHDR(), caps, model.DefaultPolicy())
	if d.Target.Height != 720 {
		t.Fatalf("expected 720p primary, got %d", d.Target.Height)
	}
	want := []model.Rendition{
		{Height: 720, VideoBitrateBps: 8_000_000},
		{Height: 480, VideoBitrateBps: 1_200_000},
	}
	if len(d.Target.Renditions) != 2 || d.Target.Renditions[0] != want[0] || d.Target.Renditions[1] != want[1] {
		t.Errorf("expected [720 primary, 480], got %+v", d.Target.Renditions)
	}
}

func TestLadderAlwaysHasPrimaryRung(t *testing.T) {
	caps := chromecastV1
	caps.MaxWidth, caps.MaxHeight = 640, 480 // nothing below the primary fits
	d := Decide(hevc4kHDR(), caps, model.DefaultPolicy())
	if len(d.Target.Renditions) != 1 {
		t.Fatalf("expected only the primary rung, got %+v", d.Target.Renditions)
	}
	if d.Target.Renditions[0].Height != d.Target.Height {
		t.Errorf("primary rung must mirror the target: %+v vs height %d",
			d.Target.Renditions[0], d.Target.Height)
	}
}

func TestAudioOnlyTranscodeHasNoLadder(t *testing.T) {
	m := h264Compatible()
	m.AudioCodec = "dts"
	d := Decide(m, chromecastV1, model.DefaultPolicy())
	if d.Target.VideoCodec != "" {
		t.Fatalf("precondition: video should be copied, got %q", d.Target.VideoCodec)
	}
	if d.Target.Renditions != nil {
		t.Errorf("audio-only transcode must not carry a ladder: %+v", d.Target.Renditions)
	}
}

func TestRemuxHasNoLadder(t *testing.T) {
	m := hevc4kHDR() // hevc copy into fmp4 (remux-ish: no video re-encode)
	caps := modernTV
	caps.Containers = []string{"mp4"}
	caps.SupportsHDR = []string{"hdr10"}
	caps.HLSSegmentFormats = []string{"ts", "fmp4"}
	d := Decide(m, caps, model.DefaultPolicy())
	if d.Target.VideoCodec != "" {
		t.Fatalf("precondition: hevc should be copied, got %q", d.Target.VideoCodec)
	}
	if d.Target.Renditions != nil {
		t.Errorf("copied video must not carry a ladder: %+v", d.Target.Renditions)
	}
}

func TestForcedHevcReencodeGetsLadderToo(t *testing.T) {
	// TS-only client turns an hevc copy into a re-encode — decode cost is
	// being paid, so the ladder applies here as well.
	m := hevc4kHDR()
	caps := modernTV
	caps.Containers = []string{"mp4"}
	caps.SupportsHDR = []string{"hdr10"}
	caps.HLSSegmentFormats = []string{"ts"}
	d := Decide(m, caps, model.DefaultPolicy())
	if d.Target.VideoCodec != "h264" {
		t.Fatalf("precondition: expected forced h264 re-encode, got %q", d.Target.VideoCodec)
	}
	if len(d.Target.Renditions) < 2 {
		t.Errorf("forced re-encode should still ladder: %+v", d.Target.Renditions)
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

// --- playback selections: audio track + subtitle burn-in ---

func intp(n int) *int { return &n }

func TestSelectedAudioTrackForcesAudioReencode(t *testing.T) {
	// Track 0 (aac) would have been copied; the client selected the ac3
	// commentary track, which an aac-only client needs re-encoded.
	m := h264Compatible()
	m.AudioTracks = []model.AudioTrack{
		{Ordinal: 0, Codec: "aac", Channels: 2, Default: true},
		{Ordinal: 1, Codec: "ac3", Channels: 6, Language: "eng"},
	}
	d := DecideWith(m, chromecastV1, model.DefaultPolicy(), Options{AudioTrack: intp(1)})
	if d.Method != model.Transcode {
		t.Fatalf("expected transcode for selected ac3 on aac-only client, got %s (trace %+v)", d.Method, d.Trace)
	}
	if d.Target.AudioCodec != "aac" {
		t.Errorf("selected ac3 5.1 track must be re-encoded to aac, got %q", d.Target.AudioCodec)
	}
	if d.Target.VideoCodec != "" {
		t.Errorf("video is compatible and should be copied, got %q", d.Target.VideoCodec)
	}
	if d.Target.AudioStreamOrdinal != 1 {
		t.Errorf("target must carry the selected stream ordinal, got %d", d.Target.AudioStreamOrdinal)
	}
	if s := d.Trace[0]; s.Check != "audio_track" || !s.Passed || s.Detail != "track 1 selected: ac3 5.1 (eng)" {
		t.Errorf("expected leading audio_track trace step, got %+v", d.Trace[0])
	}
}

func TestSelectedAudioTrackAllowsCopy(t *testing.T) {
	// Vice versa: the first track (ac3 5.1) would need a re-encode, but the
	// client picked the aac stereo track — audio is copied.
	m := h264Compatible()
	m.Container = "mkv" // force a remux so the audio verdict is observable
	m.AudioCodec, m.AudioChannels = "ac3", 6
	m.AudioTracks = []model.AudioTrack{
		{Ordinal: 0, Codec: "ac3", Channels: 6, Default: true},
		{Ordinal: 1, Codec: "aac", Channels: 2},
	}
	base := Decide(m, chromecastV1, model.DefaultPolicy())
	if base.Target == nil || base.Target.AudioCodec != "aac" {
		t.Fatalf("precondition: default track should re-encode audio, got %+v", base.Target)
	}
	d := DecideWith(m, chromecastV1, model.DefaultPolicy(), Options{AudioTrack: intp(1)})
	if d.Method != model.Transcode {
		t.Fatalf("expected remux transcode, got %s", d.Method)
	}
	if d.Target.AudioCodec != "" {
		t.Errorf("selected aac track is compatible and must be copied, got %q", d.Target.AudioCodec)
	}
	if d.Target.AudioStreamOrdinal != 1 {
		t.Errorf("target must map stream 1, got %d", d.Target.AudioStreamOrdinal)
	}
}

func TestSelectedAudioTrackKeepsDirectPlay(t *testing.T) {
	// Direct play hands the whole file to the client, which switches tracks
	// natively — a selected-but-compatible track never blocks direct play.
	m := h264Compatible()
	m.Container = "mp4"
	m.AudioTracks = []model.AudioTrack{
		{Ordinal: 0, Codec: "aac", Channels: 2, Default: true},
		{Ordinal: 1, Codec: "ac3", Channels: 6, Language: "eng"},
	}
	d := DecideWith(m, modernTV, model.DefaultPolicy(), Options{AudioTrack: intp(1)})
	if d.Method != model.DirectPlay {
		t.Fatalf("expected direct play (client decodes ac3 natively), got %s (trace %+v)", d.Method, d.Trace)
	}
	if d.Trace[0].Check != "audio_track" {
		t.Errorf("trace should still record the selection: %+v", d.Trace[0])
	}
}

func TestBurnForcesVideoReencode(t *testing.T) {
	m := h264Compatible()
	m.Subtitles = []model.SubtitleTrack{
		{Ordinal: 2, Codec: "hdmv_pgs_subtitle", Language: "eng", Supported: false},
	}
	if d := Decide(m, chromecastV1, model.DefaultPolicy()); d.Method != model.DirectPlay {
		t.Fatalf("precondition: without a burn this direct-plays, got %s", d.Method)
	}
	d := DecideWith(m, chromecastV1, model.DefaultPolicy(), Options{BurnSubtitle: intp(2)})
	if d.Method != model.Transcode {
		t.Fatalf("burn must force a transcode, got %s", d.Method)
	}
	if d.Target.VideoCodec == "" {
		t.Error("burn must force a video re-encode (subs only exist in decoded frames)")
	}
	if d.Target.BurnSubtitleOrdinal == nil || *d.Target.BurnSubtitleOrdinal != 2 {
		t.Errorf("target must carry the burn ordinal, got %v", d.Target.BurnSubtitleOrdinal)
	}
	found := false
	for _, s := range d.Trace {
		if s.Check == "subtitle_burn" && !s.Passed &&
			s.Detail == "burning track 2 (hdmv_pgs_subtitle, eng) — forces video re-encode" {
			found = true
		}
	}
	if !found {
		t.Errorf("trace must explain the burn: %+v", d.Trace)
	}
}

func TestBurnHevcSegmentFormatInterplay(t *testing.T) {
	// Burning an hevc source re-encodes to the policy codec (h264), so the
	// output rides TS even for a ts-only client — no fmp4 dead end.
	m := hevc4kHDR()
	m.Subtitles = []model.SubtitleTrack{{Ordinal: 0, Codec: "dvd_subtitle", Supported: false}}
	caps := modernTV
	caps.Containers = []string{"mp4"}
	caps.SupportsHDR = []string{"hdr10"}
	caps.HLSSegmentFormats = []string{"ts"}
	d := DecideWith(m, caps, model.DefaultPolicy(), Options{BurnSubtitle: intp(0)})
	if d.Method != model.Transcode || d.Target.VideoCodec != "h264" {
		t.Fatalf("expected h264 re-encode, got %+v", d)
	}
	if d.Target.SegmentFormat != "ts" {
		t.Errorf("h264 output should ship in ts, got %q", d.Target.SegmentFormat)
	}
	if len(d.Target.Renditions) < 2 {
		t.Errorf("burn pays the decode cost, so the ABR ladder applies: %+v", d.Target.Renditions)
	}
	if d.Target.BurnSubtitleOrdinal == nil || *d.Target.BurnSubtitleOrdinal != 0 {
		t.Errorf("burn ordinal lost: %v", d.Target.BurnSubtitleOrdinal)
	}
}

func TestBurnIgnoresExternalAndUnknownOrdinals(t *testing.T) {
	// The HTTP layer 400s these before Decide; the pure engine just ignores
	// ordinals that don't resolve to an embedded track.
	m := h264Compatible()
	m.Subtitles = []model.SubtitleTrack{
		{Ordinal: 3, Codec: "subrip", Supported: true, External: true, ObjectKey: "x.srt"},
	}
	for _, ord := range []int{3, 9} {
		d := DecideWith(m, chromecastV1, model.DefaultPolicy(), Options{BurnSubtitle: intp(ord)})
		if d.Method != model.DirectPlay {
			t.Errorf("ordinal %d must not trigger a burn, got %s", ord, d.Method)
		}
	}
}
