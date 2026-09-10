package pipeline

import (
	"strings"
	"testing"

	"flickr/internal/model"
)

func video(container, vc, ac string, ch int) *model.MediaInfo {
	return &model.MediaInfo{
		Medium: model.MediumVideo, Container: container, VideoCodec: vc, AudioCodec: ac,
		AudioChannels: ch, Width: 1920, Height: 1080, DurationSeconds: 5400,
		AudioTracks: []model.AudioTrack{{Ordinal: 0, Codec: ac, Channels: ch, Default: true}},
	}
}

func TestConvertPlanDecides(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		mi     *model.MediaInfo
		action string
		out    string
		reason string // a substring of the reason
	}{
		{name: "unprobed", key: "Movies/A/A.mkv", mi: nil, action: ConvertSkip, out: "Movies/A/A.mkv", reason: "not probed"},
		{name: "an audiobook is not video", key: "Audiobooks/B/1.m4b", mi: &model.MediaInfo{Medium: model.MediumAudio, Container: "m4b", AudioCodec: "aac"}, action: ConvertSkip, reason: "not video"},
		{name: "already fine", key: "Movies/A/A.mp4", mi: video("mp4", "h264", "aac", 2), action: ConvertSkip, out: "Movies/A/A.mp4", reason: "already"},
		{name: "mkv with stereo aac is a remux", key: "Movies/A/A.mkv", mi: video("mkv", "h264", "aac", 2), action: ConvertRemux, out: "Movies/A/A.mp4", reason: "mkv → mp4"},
		{name: "mp4 with 5.1 first gets a stereo track in place", key: "Movies/A/A.mp4", mi: video("mp4", "h264", "aac", 6), action: ConvertRemux, out: "Movies/A/A.mp4", reason: "aac stereo first"},
		{name: "hevc is an encode", key: "Shows/S/Season 1/S01E01.mkv", mi: video("mkv", "hevc", "eac3", 6), action: ConvertEncode, out: "Shows/S/Season 1/S01E01.mp4", reason: "hevc → h264"},
		{name: "av1 is an encode", key: "Movies/A/A.mkv", mi: video("mkv", "av1", "opus", 2), action: ConvertEncode, reason: "av1 → h264"},
		{name: "4K is left alone", key: "Movies/K/K.mkv", mi: &model.MediaInfo{Medium: model.MediumVideo, Container: "mkv", VideoCodec: "hevc", AudioCodec: "aac", Width: 3840, Height: 1610}, action: ConvertSkip, out: "Movies/K/K.mkv", reason: "4K"},
		{name: "HDR is left alone", key: "Movies/H/H.mkv", mi: &model.MediaInfo{Medium: model.MediumVideo, Container: "mkv", VideoCodec: "hevc", HDR: "hdr10", Width: 1920, Height: 1080}, action: ConvertSkip, reason: "hdr10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ConvertPlan(c.key, c.mi)
			if p.Action != c.action {
				t.Errorf("action = %s (%s), want %s", p.Action, p.Reason, c.action)
			}
			if c.out != "" && p.OutKey != c.out {
				t.Errorf("out key = %q, want %q", p.OutKey, c.out)
			}
			if !strings.Contains(p.Reason, c.reason) {
				t.Errorf("reason %q does not say %q", p.Reason, c.reason)
			}
		})
	}
}

func TestConvertPlanAudioLayout(t *testing.T) {
	// 5.1 TrueHD first, a stereo AAC commentary second: the TrueHD becomes
	// an AAC stereo track in front and an E-AC-3 5.1 behind it, the
	// commentary is copied where it is.
	mi := video("mkv", "h264", "truehd", 8)
	mi.AudioTracks = []model.AudioTrack{
		{Ordinal: 0, Codec: "truehd", Channels: 8, Default: true},
		{Ordinal: 1, Codec: "aac", Channels: 2, Title: "Commentary"},
	}
	p := ConvertPlan("Movies/A/A.mkv", mi)
	want := []AudioPlan{{0, "aac", 2}, {0, "eac3", 6}, {1, "copy", 0}}
	if len(p.Audio) != len(want) {
		t.Fatalf("audio = %+v, want %+v", p.Audio, want)
	}
	for i := range want {
		if p.Audio[i] != want[i] {
			t.Errorf("audio[%d] = %+v, want %+v", i, p.Audio[i], want[i])
		}
	}

	// AC-3 5.1 first: the original is copied behind the stereo, not re-encoded.
	p = ConvertPlan("Movies/A/A.mkv", video("mkv", "h264", "ac3", 6))
	if len(p.Audio) != 2 || p.Audio[1] != (AudioPlan{0, "copy", 0}) {
		t.Errorf("ac3 5.1 → %+v, want a copied second track", p.Audio)
	}

	// A file the scanner probed before audio_tracks existed still plans
	// from the scalars.
	mi = video("mkv", "hevc", "dts", 6)
	mi.AudioTracks = nil
	p = ConvertPlan("Movies/A/A.mkv", mi)
	if len(p.Audio) != 2 || p.Audio[0] != (AudioPlan{0, "aac", 2}) || p.Audio[1] != (AudioPlan{0, "eac3", 6}) {
		t.Errorf("scalars-only dts 5.1 → %+v", p.Audio)
	}
}

func TestConvertPlanSubtitles(t *testing.T) {
	mi := video("mkv", "h264", "aac", 2)
	mi.Subtitles = []model.SubtitleTrack{
		{Ordinal: 0, Codec: "subrip", Language: "en"},
		{Ordinal: 1, Codec: "hdmv_pgs_subtitle", Language: "eng"},
		{Ordinal: 2, Codec: "ass", Language: "ja"},
		{Ordinal: 3, Codec: "hdmv_pgs_subtitle", Language: "jpn"},
		{Ordinal: 4, Codec: "dvd_subtitle"},
		{Ordinal: 0, Codec: "subrip", External: true, ObjectKey: "Movies/A/A.en.srt"},
	}
	p := ConvertPlan("Movies/A/A.mkv", mi)
	if len(p.Subtitles) != 2 || p.Subtitles[0] != 0 || p.Subtitles[1] != 2 {
		t.Errorf("text tracks carried = %v, want [0 2]", p.Subtitles)
	}
	// The English PGS and the unlabelled VobSub are read; the Japanese
	// PGS is not, since only English is installed, and is said to be lost.
	if len(p.Bitmap) != 2 || p.Bitmap[0].Ordinal != 1 || p.Bitmap[1].Ordinal != 4 {
		t.Errorf("bitmap tracks to OCR = %+v, want ordinals 1 and 4", p.Bitmap)
	}
	if p.DroppedSubtitles != 1 || !strings.Contains(p.Reason, "2 bitmap subtitle track(s) OCR'd") || !strings.Contains(p.Reason, "1 bitmap subtitle track(s) in another language dropped") {
		t.Errorf("dropped = %d (%s)", p.DroppedSubtitles, p.Reason)
	}
}

func TestOCRArgs(t *testing.T) {
	tracks := []model.SubtitleTrack{
		{Ordinal: 1, Codec: "hdmv_pgs_subtitle", Language: "eng"},
		{Ordinal: 4, Codec: "dvd_subtitle"},
		{Ordinal: 5, Codec: "hdmv_pgs_subtitle", Language: "en", Title: "SDH"},
	}
	s := strings.Join(ExtractSubsArgs("http://x/in.mkv", tracks, "/w/subs.mkv"), " ")
	for _, want := range []string{"-i http://x/in.mkv", "-vn -an -dn", "-map 0:s:1 -map 0:s:4 -map 0:s:5", "-c:s copy -f matroska /w/subs.mkv"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	s = strings.Join(MKVExtractArgs("/w/subs.mkv", "/w", tracks), " ")
	if s != "tracks /w/subs.mkv 0:/w/track0.en.sup 1:/w/track1.idx 2:/w/track2.en.sup" {
		t.Errorf("mkvextract argv = %s", s)
	}
	if SRTFile("/w/track0.en.sup") != "/w/track0.en.srt" || SRTFile("/w/track1.idx") != "/w/track1.srt" {
		t.Errorf("srt names: %s, %s", SRTFile("/w/track0.en.sup"), SRTFile("/w/track1.idx"))
	}
	name, args := OCRCommand(tracks[0], "/w/track0.en.sup", "/w/track0.en.srt")
	if name != "pgsrip" || strings.Join(args, " ") != "-l en --force /w/track0.en.sup" {
		t.Errorf("pgs: %s %v", name, args)
	}
	// The palette line as mkvextract wrote it for a real disc, and as
	// subtile-ocr will read it.
	idx := "# VobSub index file, v7\nsize: 720x480\npalette: 20D620, 35C7EF, 000000, FDFDFD,D50ECA, 00E000\ntimestamp: 00:00:03:237, filepos: 000000000\n"
	wantIDX := "# VobSub index file, v7\nsize: 720x480\npalette: 20D620, 35C7EF, 000000, FDFDFD, D50ECA, 00E000\ntimestamp: 00:00:03:237, filepos: 000000000\n"
	if got := string(NormalizeIDX([]byte(idx))); got != wantIDX {
		t.Errorf("NormalizeIDX =\n%s\nwant\n%s", got, wantIDX)
	}
	name, args = OCRCommand(tracks[1], "/w/track1.idx", "/w/track1.srt")
	if name != "subtile-ocr" || strings.Join(args, " ") != "-l eng -o /w/track1.srt /w/track1.idx" {
		t.Errorf("vobsub: %s %v", name, args)
	}
	keys := SidecarKeys("Movies/A (1999)/A.mkv", tracks)
	want := []string{"Movies/A (1999)/A.eng.srt", "Movies/A (1999)/A.eng.2.srt", "Movies/A (1999)/A.eng.3.srt"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("sidecar keys = %v, want %v", keys, want)
	}
}

func TestConvertPlanTelecine(t *testing.T) {
	mi := video("mkv", "mpeg2video", "ac3", 2)
	mi.Telecine, mi.FPS = true, 29.97
	p := ConvertPlan("Movies/D/D.mkv", mi)
	if !p.Detelecine || p.FPS < 23.9 || p.FPS > 24.0 {
		t.Errorf("detelecine = %v at %.3f, want the film rate", p.Detelecine, p.FPS)
	}
	if p.HWDecode {
		t.Error("a software filter chain must not start from frames on the card")
	}
	s := strings.Join(ConvertArgs(p, "http://x/in.mkv", "/w/out.mp4", "/dev/dri/renderD128", true), " ")
	if !strings.Contains(s, "-vf fps=23.976000,format=nv12,hwupload") {
		t.Errorf("telecine chain missing: %s", s)
	}
	// A copied H.264 never re-times: the pulldown flags pass through.
	mi = video("mkv", "h264", "ac3", 2)
	mi.Telecine, mi.FPS = true, 29.97
	if p := ConvertPlan("Movies/D/D.mkv", mi); p.Detelecine {
		t.Error("a copied video must not be re-timed")
	}
}

func TestConvertArgsEncodeOnTheCard(t *testing.T) {
	p := ConvertPlan("Shows/S/Season 1/S01E01.mkv", video("mkv", "hevc", "eac3", 6))
	s := strings.Join(ConvertArgs(p, "http://x/in.mkv", "/w/out.mp4", "/dev/dri/renderD128", true), " ")
	for _, want := range []string{
		"-vaapi_device /dev/dri/renderD128",
		"-hwaccel vaapi -hwaccel_output_format vaapi -i http://x/in.mkv", // decoded on the card, before the input
		"-map 0:v:0 -map 0:a:0 -map 0:a:0",                               // the surround track twice: stereo, then itself
		"-vf scale_vaapi=format=nv12",                                    // 10-bit squeezed on the card
		"-c:v h264_vaapi -profile:v high -level 41 -rc_mode CQP -qp 21",
		"-c:a:0 aac -ac:a:0 2 -b:a:0 192k -disposition:a:0 default",
		"-c:a:1 copy -disposition:a:1 0",
		"-movflags +faststart -f mp4 /w/out.mp4",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	if strings.Contains(s, "-c:s") {
		t.Errorf("no subtitles were mapped, none should be encoded: %s", s)
	}

	// The fallback: the same plan decoded in software and uploaded.
	s = strings.Join(ConvertArgs(p, "http://x/in.mkv", "/w/out.mp4", "/dev/dri/renderD128", false), " ")
	if strings.Contains(s, "-hwaccel") || !strings.Contains(s, "-vf format=nv12,hwupload") {
		t.Errorf("software fallback must upload plain frames: %s", s)
	}
	if !strings.Contains(s, "-vaapi_device /dev/dri/renderD128") {
		t.Errorf("the encoder still needs the device: %s", s)
	}
}

func TestConvertArgsRemuxTouchesNoVideo(t *testing.T) {
	mi := video("mkv", "h264", "aac", 2)
	mi.Subtitles = []model.SubtitleTrack{{Ordinal: 0, Codec: "subrip", Language: "en"}}
	p := ConvertPlan("Movies/A/A.mkv", mi)
	s := strings.Join(ConvertArgs(p, "http://x/in.mkv", "/w/out.mp4", "/dev/dri/renderD128", true), " ")
	for _, want := range []string{"-c:v copy", "-c:a:0 copy", "-map 0:s:0", "-c:s mov_text", "-f mp4"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	for _, none := range []string{"-vaapi_device", "-hwaccel", "-vf", "h264_vaapi"} {
		if strings.Contains(s, none) {
			t.Errorf("a remux must not touch the card (%s): %s", none, s)
		}
	}
}

func TestVerify(t *testing.T) {
	p := ConvertPlan("Movies/A/A.mkv", video("mkv", "hevc", "eac3", 6))
	good := `{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac","channels":2},{"codec_type":"audio","codec_name":"eac3","channels":6}],"format":{"duration":"5399.500000"}}`
	if err := Verify([]byte(good), p, 5400); err != nil {
		t.Errorf("a good file was refused: %v", err)
	}
	cases := []struct{ name, probe, want string }{
		{"video not h264", strings.Replace(good, `"codec_name":"h264"`, `"codec_name":"hevc"`, 1), "want h264"},
		{"first audio not stereo", strings.Replace(good, `"channels":2`, `"channels":6`, 1), "want aac stereo"},
		{"a track missing", `{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac","channels":2}],"format":{"duration":"5400"}}`, "1 audio streams, want 2"},
		{"cut short", strings.Replace(good, `"5399.500000"`, `"4800"`, 1), "duration 4800.0s"},
		{"no json", "ffprobe: not found", "no JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Verify([]byte(c.probe), p, 5400)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	// A short file tolerates five seconds; a long one two percent.
	short := ConvertPlan("Shows/S/1.mkv", video("mkv", "h264", "aac", 2))
	if err := Verify([]byte(`{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac","channels":2}],"format":{"duration":"57"}}`), short, 60); err != nil {
		t.Errorf("three seconds short of a minute was refused: %v", err)
	}
}

func TestCardProbeArgs(t *testing.T) {
	s := strings.Join(CardProbeArgs("/dev/dri/renderD128"), " ")
	for _, want := range []string{"-vaapi_device /dev/dri/renderD128", "-f lavfi", "-t 1", "hwupload", "-c:v h264_vaapi", "-f null -"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}
