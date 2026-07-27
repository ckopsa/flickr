package pipeline

import (
	"slices"
	"strings"
	"testing"

	"flickr/internal/model"
)

func argString(j Job) string {
	return strings.Join(BuildArgs(j), " ")
}

func TestCopyBothStreams(t *testing.T) {
	s := argString(Job{InputURL: "http://x/in.mkv", Target: model.TranscodeTarget{}, OutputDir: "/out"})
	for _, want := range []string{"-c:v copy", "-c:a copy", "-f hls"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	if strings.Contains(s, "-vf") || strings.Contains(s, "-af") {
		t.Errorf("copy job must not add filters: %s", s)
	}
	if !strings.Contains(s, "seg%05d.ts") || strings.Contains(s, "fmp4") {
		t.Errorf("default segment format must be TS (compatibility floor): %s", s)
	}
}

func TestFmp4SegmentsWhenNegotiated(t *testing.T) {
	s := argString(Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{SegmentFormat: "fmp4"},
		OutputDir: "/out",
	})
	for _, want := range []string{"-hls_segment_type fmp4", "init.mp4", "seg%05d.m4s"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}

func TestAudioReencodeAlignsTimeline(t *testing.T) {
	// Seeks into sync-framed codecs (TrueHD/DTS) drop leading audio; the
	// resampler must pad it back onto the video timeline or players garble.
	s := argString(Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{AudioCodec: "aac"},
		OutputDir: "/out",
	})
	if !strings.Contains(s, "-af aresample=async=1") {
		t.Errorf("audio re-encode must keep timestamps monotonic: %s", s)
	}
}

func TestFullTranscodeWithTonemapAndScale(t *testing.T) {
	j := Job{
		InputURL: "http://x/in.mkv",
		Target: model.TranscodeTarget{
			VideoCodec: "h264", AudioCodec: "aac", Height: 1080,
			VideoBitrateBps: 8_000_000, AudioBitrateBps: 192_000, Tonemap: true,
		},
		OutputDir: "/out",
	}
	s := argString(j)
	for _, want := range []string{"-c:v libx264", "tonemap=hable", "scale=-2:1080", "-b:v 8000000", "-c:a aac", "-b:a 192000"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	// Tone-map must come before scale in the filter chain.
	args := BuildArgs(j)
	vf := args[slices.Index(args, "-vf")+1]
	if strings.Index(vf, "tonemap") > strings.Index(vf, "scale=-2:1080") {
		t.Errorf("tonemap must precede scale: %s", vf)
	}
}

func TestSeekPrecedesInput(t *testing.T) {
	args := BuildArgs(Job{InputURL: "http://x/in.mkv", SeekSeconds: 750, OutputDir: "/out"})
	ss := slices.Index(args, "-ss")
	in := slices.Index(args, "-i")
	if ss == -1 || ss > in {
		t.Errorf("-ss must appear before -i for fast input seeking: %v", args)
	}
	if args[ss+1] != "750.000" {
		t.Errorf("expected seek 750.000, got %s", args[ss+1])
	}
}

func TestSeekWithAudioReencodeSplitsForPreroll(t *testing.T) {
	// Full transcode + seek: input seeks 10s early, output trims 10s, so
	// sync-framed audio (TrueHD/DTS) is decodable by the cut point and both
	// streams start exactly at the target.
	args := BuildArgs(Job{
		InputURL:    "http://x/in.mkv",
		Target:      model.TranscodeTarget{VideoCodec: "h264", AudioCodec: "aac"},
		SeekSeconds: 600,
		OutputDir:   "/out",
	})
	in := slices.Index(args, "-i")
	if args[slices.Index(args, "-ss")+1] != "590.000" {
		t.Errorf("input seek should be target minus preroll: %v", args)
	}
	rest := args[in:]
	out := slices.Index(rest, "-ss")
	if out == -1 || rest[out+1] != "10.000" {
		t.Errorf("expected 10s output-side trim after -i: %v", args)
	}
}

func TestSeekNearZeroClampsPreroll(t *testing.T) {
	args := BuildArgs(Job{
		InputURL:    "http://x/in.mkv",
		Target:      model.TranscodeTarget{VideoCodec: "h264", AudioCodec: "aac"},
		SeekSeconds: 4,
		OutputDir:   "/out",
	})
	// The whole preroll fits before the target: no input seek at all,
	// just a 4s output trim.
	ss := slices.Index(args, "-ss")
	if ss < slices.Index(args, "-i") {
		t.Fatalf("expected no input-side seek for tiny targets: %v", args)
	}
	if args[ss+1] != "4.000" {
		t.Errorf("output trim must equal the clamped seek target: %v", args)
	}
}

func TestSeekWithCopiedStreamsStaysInputSide(t *testing.T) {
	// Pure remux (both copied): output trim is impossible and unnecessary;
	// the seek stays input-side with keyframe snapping.
	args := BuildArgs(Job{
		InputURL:    "http://x/in.mkv",
		Target:      model.TranscodeTarget{},
		SeekSeconds: 600,
		OutputDir:   "/out",
	})
	if n := strings.Count(strings.Join(args, " "), "-ss "); n != 1 {
		t.Errorf("expected exactly one seek arg, got %d: %v", n, args)
	}
	if args[slices.Index(args, "-ss")+1] != "600.000" {
		t.Errorf("copy jobs seek input-side at the target: %v", args)
	}
}

var vaapiHW = &HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128",
	Encoders: map[string]string{"h264": "h264_vaapi"}}
var nvencHW = &HWAccel{Kind: "nvenc", Encoders: map[string]string{"h264": "h264_nvenc", "hevc": "hevc_nvenc"}}

func TestVAAPIJobInitsDeviceAndUploads(t *testing.T) {
	j := Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{VideoCodec: "h264", Height: 1080, Tonemap: true},
		OutputDir: "/out",
		HW:        vaapiHW,
	}
	args := BuildArgs(j)
	s := strings.Join(args, " ")
	for _, want := range []string{"-init_hw_device vaapi=va:/dev/dri/renderD128", "-c:v h264_vaapi"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	// Software filters (tonemap, scale) run first; hwupload must be last.
	vf := args[slices.Index(args, "-vf")+1]
	if !strings.HasSuffix(vf, "format=nv12,hwupload") {
		t.Errorf("hwupload must terminate the filter chain: %s", vf)
	}
	if strings.Index(vf, "tonemap") > strings.Index(vf, "hwupload") {
		t.Errorf("software tonemap must precede hwupload: %s", vf)
	}
	// Init args are global options and must precede -i.
	if slices.Index(args, "-init_hw_device") > slices.Index(args, "-i") {
		t.Errorf("-init_hw_device must precede -i: %v", args)
	}
	if strings.Contains(s, "-preset") {
		t.Errorf("vaapi has no preset concept: %s", s)
	}
}

func TestNvencUsesItsOwnPresetNamespace(t *testing.T) {
	j := Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{VideoCodec: "h264"},
		OutputDir: "/out",
		HW:        nvencHW,
	}
	s := argString(j)
	if !strings.Contains(s, "-c:v h264_nvenc") || !strings.Contains(s, "-preset p5") {
		t.Errorf("expected nvenc with p5 preset: %s", s)
	}
	if strings.Contains(s, "veryfast") {
		t.Errorf("x264 preset leaked into nvenc job: %s", s)
	}
}

func TestHWFallsBackToSoftwareForUnsupportedCodec(t *testing.T) {
	hw := &HWAccel{Kind: "vaapi", Device: "/dev/dri/renderD128",
		Encoders: map[string]string{"h264": "h264_vaapi"}} // no hevc
	j := Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{VideoCodec: "hevc"},
		OutputDir: "/out",
		HW:        hw,
	}
	s := argString(j)
	if !strings.Contains(s, "-c:v libx265") {
		t.Errorf("expected software fallback for hevc: %s", s)
	}
	if strings.Contains(s, "hwupload") || strings.Contains(s, "-init_hw_device") {
		t.Errorf("software fallback must not carry hw plumbing: %s", s)
	}
}

func TestDetelecineFilterChainAndGop(t *testing.T) {
	j := Job{
		InputURL: "http://x/in.mkv",
		Target: model.TranscodeTarget{
			VideoCodec: "h264", Detelecine: true, FPS: 23.976,
		},
		OutputDir: "/out",
	}
	args := BuildArgs(j)
	vf := args[slices.Index(args, "-vf")+1]
	if !strings.HasPrefix(vf, "fps=23.976") {
		t.Errorf("soft telecine must re-time to the film rate (never decimate): %s", vf)
	}
	g := args[slices.Index(args, "-g")+1]
	if g != "96" {
		t.Errorf("gop must follow output fps (4s at 23.976 = 96), got %s", g)
	}
}

func TestRkmppEncoderArgs(t *testing.T) {
	hw := &HWAccel{Kind: "rkmpp", Device: "/dev/mpp_service",
		Encoders: map[string]string{"h264": "h264_rkmpp", "hevc": "hevc_rkmpp"}}
	s := argString(Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{VideoCodec: "h264", VideoBitrateBps: 6_000_000},
		OutputDir: "/out",
		HW:        hw,
	})
	if !strings.Contains(s, "-c:v h264_rkmpp") || !strings.Contains(s, "format=nv12") {
		t.Errorf("expected rkmpp encoder with nv12 conversion: %s", s)
	}
	if strings.Contains(s, "-preset") {
		t.Errorf("rkmpp has no preset concept: %s", s)
	}
	if strings.Contains(s, "-init_hw_device") {
		t.Errorf("rkmpp needs no hw device init args: %s", s)
	}
}

func TestCopyJobIgnoresHW(t *testing.T) {
	s := argString(Job{InputURL: "http://x/in.mkv", Target: model.TranscodeTarget{}, OutputDir: "/out", HW: nvencHW})
	if strings.Contains(s, "nvenc") {
		t.Errorf("stream copy must not involve encoders: %s", s)
	}
}

func TestNoSeekArgWhenStartingFromZero(t *testing.T) {
	args := BuildArgs(Job{InputURL: "http://x/in.mkv", OutputDir: "/out"})
	if slices.Contains(args, "-ss") {
		t.Errorf("no -ss expected at position 0: %v", args)
	}
}
