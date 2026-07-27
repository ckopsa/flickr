package pipeline

import (
	"slices"
	"strings"
	"testing"

	"flickr/internal/model"
)

func ladder3() []model.Rendition {
	return []model.Rendition{
		{Height: 1080, VideoBitrateBps: 8_000_000},
		{Height: 720, VideoBitrateBps: 3_000_000},
		{Height: 480, VideoBitrateBps: 1_200_000},
	}
}

func abrJob() Job {
	return Job{
		InputURL: "http://x/in.mkv",
		Target: model.TranscodeTarget{
			VideoCodec: "h264", AudioCodec: "aac",
			Height: 1080, VideoBitrateBps: 8_000_000, AudioBitrateBps: 192_000,
			Renditions: ladder3(),
		},
		OutputDir: "/out",
	}
}

func TestABRThreeRungLadderArgs(t *testing.T) {
	args := BuildArgs(abrJob())
	s := strings.Join(args, " ")
	for _, want := range []string{
		"-var_stream_map v:0,a:0 v:1,a:1 v:2,a:2",
		"-master_pl_name master.m3u8",
		"/out/index_%v.m3u8",
		// Flat segment names: ffmpeg writes only the basename into the
		// variant playlist, so v%v/ subdirs would 404 on the client.
		"/out/v%v_seg%05d.ts",
		"-b:v:0 8000000", "-b:v:1 3000000", "-b:v:2 1200000",
		"-c:v:0 libx264", "-c:v:1 libx264", "-c:v:2 libx264",
		"-c:a aac",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	// Single decode, per-variant scale: one split feeding three chains.
	fc := args[slices.Index(args, "-filter_complex")+1]
	if !strings.Contains(fc, "split=3[s0][s1][s2]") {
		t.Errorf("expected a 3-way split: %s", fc)
	}
	for _, want := range []string{"[s0]scale=-2:1080", "[s1]scale=-2:720", "[s2]scale=-2:480"} {
		if !strings.Contains(fc, want) {
			t.Errorf("missing per-variant %q in: %s", want, fc)
		}
	}
	// Exactly one audio map per variant.
	if n := strings.Count(s, "-map 0:a:0"); n != 3 {
		t.Errorf("expected 3 audio maps (one per variant), got %d: %s", n, s)
	}
	// Single-rendition playlist must not appear.
	if strings.Contains(s, "/out/index.m3u8") {
		t.Errorf("ABR job must not write the single-rendition playlist: %s", s)
	}
}

func TestABRSharedFiltersPrecedeSplit(t *testing.T) {
	j := abrJob()
	j.Target.Detelecine = true
	j.Target.FPS = 23.976
	j.Target.Tonemap = true
	args := BuildArgs(j)
	fc := args[slices.Index(args, "-filter_complex")+1]
	split := strings.Index(fc, "split=")
	if i := strings.Index(fc, "fps=23.976"); i == -1 || i > split {
		t.Errorf("detelecine fps must run once, before the split: %s", fc)
	}
	if i := strings.Index(fc, "tonemap=hable"); i == -1 || i > split {
		t.Errorf("tonemap must run once, before the split: %s", fc)
	}
	if strings.Count(fc, "tonemap=") != 1 || strings.Count(fc, "fps=") != 1 {
		t.Errorf("shared filters must appear exactly once: %s", fc)
	}
	// GOP follows the detelecined output rate.
	if g := args[slices.Index(args, "-g")+1]; g != "96" {
		t.Errorf("gop must follow output fps (4s at 23.976 = 96), got %s", g)
	}
}

func TestABRRkmppEncoderPerVariant(t *testing.T) {
	hw := &HWAccel{Kind: "rkmpp", Device: "/dev/mpp_service",
		Encoders: map[string]string{"h264": "h264_rkmpp"}}
	j := abrJob()
	j.HW = hw
	args := BuildArgs(j)
	s := strings.Join(args, " ")
	for _, want := range []string{"-c:v:0 h264_rkmpp", "-c:v:1 h264_rkmpp", "-c:v:2 h264_rkmpp"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	// rkmpp takes plain nv12 frames: every variant chain must end in
	// format=nv12 (after its scale), never before shared filters.
	fc := args[slices.Index(args, "-filter_complex")+1]
	for _, want := range []string{
		"[s0]scale=-2:1080,format=nv12[v0]",
		"[s1]scale=-2:720,format=nv12[v1]",
		"[s2]scale=-2:480,format=nv12[v2]",
	} {
		if !strings.Contains(fc, want) {
			t.Errorf("missing %q in: %s", want, fc)
		}
	}
	if strings.Contains(s, "-preset") && !strings.Contains(s, "-preset:v") {
		t.Errorf("rkmpp has no preset concept: %s", s)
	}
	if strings.Contains(s, "-init_hw_device") {
		t.Errorf("rkmpp needs no hw device init args: %s", s)
	}
}

func TestABRVaapiUploadsPerVariant(t *testing.T) {
	j := abrJob()
	j.HW = vaapiHW
	args := BuildArgs(j)
	fc := args[slices.Index(args, "-filter_complex")+1]
	if strings.Count(fc, "format=nv12,hwupload") != 3 {
		t.Errorf("vaapi must upload at the end of each variant chain: %s", fc)
	}
}

func TestABRFmp4VariantNaming(t *testing.T) {
	j := abrJob()
	j.Target.SegmentFormat = "fmp4"
	s := argString(j)
	for _, want := range []string{
		"-hls_segment_type fmp4",
		"-hls_fmp4_init_filename init_%v.mp4",
		"/out/v%v_seg%05d.m4s",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}

func TestABRCopyAudioSharedAcrossVariants(t *testing.T) {
	j := abrJob()
	j.Target.AudioCodec = "" // compatible audio: copied into each variant
	s := argString(j)
	if !strings.Contains(s, "-c:a copy") {
		t.Errorf("compatible audio should be copied: %s", s)
	}
	if strings.Contains(s, "aresample") {
		t.Errorf("copied audio must not be filtered: %s", s)
	}
}

func TestSingleRenditionStaysOnPlainPath(t *testing.T) {
	j := abrJob()
	j.Target.Renditions = j.Target.Renditions[:1]
	s := argString(j)
	if strings.Contains(s, "var_stream_map") || strings.Contains(s, "master.m3u8") {
		t.Errorf("single rung must keep the plain single-playlist output: %s", s)
	}
	if !strings.Contains(s, "/out/index.m3u8") {
		t.Errorf("single rung must write index.m3u8: %s", s)
	}
}

func TestCopyJobIgnoresStrayRenditions(t *testing.T) {
	j := Job{
		InputURL:  "http://x/in.mkv",
		Target:    model.TranscodeTarget{Renditions: ladder3()}, // video copy
		OutputDir: "/out",
	}
	s := argString(j)
	if !strings.Contains(s, "-c:v copy") || strings.Contains(s, "var_stream_map") {
		t.Errorf("copy jobs must stay untouched by a stray ladder: %s", s)
	}
}

func TestABRSeekKeepsPrerollSplit(t *testing.T) {
	j := abrJob()
	j.SeekSeconds = 600
	args := BuildArgs(j)
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

func TestABRSessionWaitsForMasterName(t *testing.T) {
	s := &Session{Job: abrJob()}
	if s.PlaylistName() != "master.m3u8" {
		t.Errorf("ABR sessions must hand out the master playlist, got %s", s.PlaylistName())
	}
	single := &Session{Job: Job{Target: model.TranscodeTarget{VideoCodec: "h264"}}}
	if single.PlaylistName() != "index.m3u8" {
		t.Errorf("single-rendition sessions keep index.m3u8, got %s", single.PlaylistName())
	}
}
