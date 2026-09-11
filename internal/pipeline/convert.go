package pipeline

// Conversion: the file rewritten, once, into what every device in the
// household direct-plays — MP4, H.264 8-bit at up to 1080p, an AAC stereo
// track first, text subtitles carried as mov_text — so the orangepi's one
// encoder is never spent on it again. The decision is ConvertPlan, pure over
// the probe the scanner already made; the argv is ConvertArgs, pure over the
// plan; the check afterwards is ProbeArgs and Verify. The worker that spends
// the hours (cmd/converter) shells out and nothing else.
//
// The video path is the card's: VAAPI decodes what it can (H.264, HEVC, AV1,
// VP9), scale_vaapi squeezes 10-bit to nv12 on the card, h264_vaapi encodes.
// A source the card will not decode — or one that needs a software filter
// (soft telecine) — is decoded in software and uploaded to the card for the
// encode; the worker tries the card first and falls back on failure.

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"

	"flickr/internal/model"
)

// The target, said once. Level 4.1 is the floor a first-generation
// Chromecast decodes at 1080p; CQP 21 is where h264_vaapi on VCN stops
// looking softer than the HEVC it replaces at a comparable size.
const (
	convertVideoQP       = "21"
	convertVideoLevel    = "41"
	convertStereoBitrate = "192k"
	convertSurroundRate  = "640k"
	convertMaxHeight     = 1080
	convertMaxWidth      = 1920
)

// ConvertAction is what the plan would do to the file.
const (
	ConvertSkip   = "skip"   // nothing: the reason says why
	ConvertRemux  = "remux"  // the video is copied; the container and the audio change
	ConvertEncode = "encode" // the video is re-encoded on the card
)

// AudioPlan is one output audio stream: which source stream it is made from
// and what it becomes. Codec "copy" keeps the stream as it is; "aac" and
// "eac3" re-encode it to Channels.
type AudioPlan struct {
	Ordinal  int    // source stream, among audio streams (-map 0:a:N)
	Codec    string // "copy", "aac", "eac3"
	Channels int    // 0 for copy
}

// Plan is what the converter would do to one file, decided from its probe
// alone. OutKey is the object key the result is written to: the same folder,
// the same name, .mp4 — which is the source's own key when the container was
// already MP4 and only the audio layout changes.
type Plan struct {
	Action string
	Reason string // for a skip, why; otherwise what changes, in words
	OutKey string

	VideoCopy  bool
	HWDecode   bool    // the card decodes the source; false means software decode, hwupload
	Detelecine bool    // soft telecine: re-time to FPS in software before the upload
	FPS        float64 // the film rate the detelecine re-times to

	Audio     []AudioPlan
	Subtitles []int // embedded text tracks carried as mov_text, by ordinal
	// Bitmap is the embedded picture tracks (PGS, VobSub) MP4 cannot carry:
	// each is OCR'd to a .srt sidecar beside the file BEFORE the conversion
	// (ocr.go), and a track that will not OCR leaves the file unconverted.
	// Only English (or unlabelled, taken as English) tracks: the OCR speaks
	// one language.
	Bitmap []model.SubtitleTrack
	// DroppedSubtitles counts the bitmap tracks in another language, which
	// are lost with the container. Said in the plan so a dry run shows it.
	DroppedSubtitles int
}

// mp4Audio is what rides in MP4 and plays where it lands: the browser
// profile's AAC and MP3, the TV's AC-3 and E-AC-3. Opus and FLAC are legal in
// MP4 by now and still refused by half the players, so they are re-encoded.
var mp4Audio = map[string]bool{"aac": true, "mp3": true, "ac3": true, "eac3": true}

// stereoAudio is what the browser profile direct-plays as a first track.
var stereoAudio = map[string]bool{"aac": true, "mp3": true}

// textSubtitle is a subtitle codec ffmpeg turns into mov_text; everything
// else embedded is a picture (PGS, VobSub) and cannot ride in MP4.
var textSubtitle = map[string]bool{"subrip": true, "srt": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true}

// bitmapSubtitle is a picture track: Blu-ray PGS and DVD VobSub.
var bitmapSubtitle = map[string]bool{"hdmv_pgs_subtitle": true, "dvd_subtitle": true}

// hwDecodable is what VCN decodes: the card's own list, not ffmpeg's.
var hwDecodable = map[string]bool{"h264": true, "hevc": true, "av1": true, "vp9": true}

// ConvertPlan decides what to do with one file from its probe. It never
// touches the file.
func ConvertPlan(objectKey string, mi *model.MediaInfo) Plan {
	p := Plan{Action: ConvertSkip, OutKey: objectKey}
	switch {
	case mi == nil:
		p.Reason = "not probed"
		return p
	case mi.MediumOrVideo() != model.MediumVideo:
		p.Reason = "not video"
		return p
	case mi.VideoCodec == "":
		p.Reason = "no video stream"
		return p
	case mi.Height > convertMaxHeight+200 || mi.Width > convertMaxWidth+200:
		// A 4K H.264 would be bigger than the HEVC it replaced and no
		// better; the 4K TV direct-plays what is there. (+200 admits the
		// odd 1088-line encode and a 2.35:1 frame padded out.)
		p.Reason = fmt.Sprintf("%dx%d: H.264 at 4K is bigger than the HEVC it would replace", mi.Width, mi.Height)
		return p
	case mi.HDR != "":
		p.Reason = mi.HDR + ": tone-mapping is a choice, not a conversion"
		return p
	}
	p.OutKey = strings.TrimSuffix(objectKey, path.Ext(objectKey)) + ".mp4"
	p.VideoCopy = mi.VideoCodec == "h264"
	p.HWDecode = hwDecodable[mi.VideoCodec]
	if !p.VideoCopy && mi.Telecine && mi.FPS > 0 {
		// The transcoder's rule: soft telecine re-times the original
		// progressive frames to the film rate; never fieldmatch/decimate.
		p.Detelecine, p.FPS = true, mi.FPS*4/5
		p.HWDecode = false // fps is a software filter; decode beside it
	}

	// The audio. The first stream is the one every direct-play check reads,
	// so the first output stream is that one, stereo AAC; its surround
	// original rides second where a TV can pick it; every other track keeps
	// its place, re-encoded only if MP4 will not carry it.
	tracks := mi.AudioTracks
	if len(tracks) == 0 && mi.AudioCodec != "" {
		tracks = []model.AudioTrack{{Ordinal: 0, Codec: mi.AudioCodec, Channels: mi.AudioChannels}}
	}
	audioChanged := false
	if len(tracks) > 0 {
		first := tracks[0]
		if stereoAudio[first.Codec] && first.Channels <= 2 {
			p.Audio = append(p.Audio, AudioPlan{Ordinal: first.Ordinal, Codec: "copy"})
		} else {
			audioChanged = true
			p.Audio = append(p.Audio, AudioPlan{Ordinal: first.Ordinal, Codec: "aac", Channels: 2})
			if first.Channels > 2 {
				p.Audio = append(p.Audio, surround(first))
			}
		}
		for _, t := range tracks[1:] {
			if mp4Audio[t.Codec] {
				p.Audio = append(p.Audio, AudioPlan{Ordinal: t.Ordinal, Codec: "copy"})
				continue
			}
			audioChanged = true
			if t.Channels > 2 {
				p.Audio = append(p.Audio, surround(t))
			} else {
				p.Audio = append(p.Audio, AudioPlan{Ordinal: t.Ordinal, Codec: "aac", Channels: 2})
			}
		}
	}

	// The subtitles: text rides along; pictures are read into text first,
	// where the OCR can read them.
	for _, s := range mi.Subtitles {
		if s.External {
			continue // a sidecar stays a sidecar, beside the new file as it was beside the old
		}
		switch {
		case textSubtitle[s.Codec]:
			p.Subtitles = append(p.Subtitles, s.Ordinal)
		case bitmapSubtitle[s.Codec] && ocrLanguage(s.Language) != "":
			p.Bitmap = append(p.Bitmap, s)
		default:
			p.DroppedSubtitles++
		}
	}

	var changes []string
	switch {
	case !p.VideoCopy:
		p.Action = ConvertEncode
		changes = append(changes, mi.VideoCodec+" → h264")
	case mi.Container != "mp4":
		p.Action = ConvertRemux
		changes = append(changes, mi.Container+" → mp4")
	case audioChanged:
		p.Action = ConvertRemux
	default:
		p.Reason = "already what every device plays"
		return p
	}
	if audioChanged {
		changes = append(changes, "aac stereo first")
	}
	if p.Detelecine {
		changes = append(changes, fmt.Sprintf("inverse telecine to %.3f fps", p.FPS))
	}
	if len(p.Bitmap) > 0 {
		changes = append(changes, fmt.Sprintf("%d bitmap subtitle track(s) OCR'd to sidecars", len(p.Bitmap)))
	}
	if p.DroppedSubtitles > 0 {
		changes = append(changes, fmt.Sprintf("%d bitmap subtitle track(s) in another language dropped", p.DroppedSubtitles))
	}
	p.Reason = strings.Join(changes, ", ")
	return p
}

// surround is the second life of a multichannel track: copied where MP4
// carries it, E-AC-3 at up to 5.1 otherwise (TrueHD, DTS, Opus, FLAC).
func surround(t model.AudioTrack) AudioPlan {
	if mp4Audio[t.Codec] {
		return AudioPlan{Ordinal: t.Ordinal, Codec: "copy"}
	}
	ch := t.Channels
	if ch > 6 {
		ch = 6
	}
	return AudioPlan{Ordinal: t.Ordinal, Codec: "eac3", Channels: ch}
}

// ConvertArgs is pure: plan + input URL + output path -> ffmpeg argv (sans
// binary). hw says whether the source is decoded on the card; a caller whose
// card refused the decode calls again with hw=false and the same plan. The
// device is the render node the worker was told.
func ConvertArgs(p Plan, inputURL, outPath, device string, hw bool) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	if !p.VideoCopy {
		args = append(args, "-vaapi_device", device)
		if hw && p.HWDecode {
			args = append(args, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi")
		}
	}
	args = append(args, "-i", inputURL, "-map", "0:v:0")
	for _, a := range p.Audio {
		args = append(args, "-map", "0:a:"+strconv.Itoa(a.Ordinal))
	}
	for _, s := range p.Subtitles {
		args = append(args, "-map", "0:s:"+strconv.Itoa(s))
	}

	if p.VideoCopy {
		args = append(args, "-c:v", "copy")
	} else {
		var vf []string
		if hw && p.HWDecode {
			// Frames are already on the card; nv12 is what the H.264
			// encoder takes and what a 10-bit source is not.
			vf = append(vf, "scale_vaapi=format=nv12")
		} else {
			if p.Detelecine {
				vf = append(vf, fmt.Sprintf("fps=%.6f", p.FPS))
			}
			vf = append(vf, "format=nv12", "hwupload")
		}
		args = append(args, "-vf", strings.Join(vf, ","),
			"-c:v", "h264_vaapi", "-profile:v", "high", "-level", convertVideoLevel,
			"-rc_mode", "CQP", "-qp", convertVideoQP)
	}

	for i, a := range p.Audio {
		n := strconv.Itoa(i)
		switch a.Codec {
		case "copy":
			args = append(args, "-c:a:"+n, "copy")
		case "aac":
			args = append(args, "-c:a:"+n, "aac", "-ac:a:"+n, strconv.Itoa(a.Channels), "-b:a:"+n, convertStereoBitrate)
		case "eac3":
			args = append(args, "-c:a:"+n, "eac3", "-ac:a:"+n, strconv.Itoa(a.Channels), "-b:a:"+n, convertSurroundRate)
		}
		// The first track is the one a player picks unasked; the rest are
		// there to be chosen.
		if i == 0 {
			args = append(args, "-disposition:a:0", "default")
		} else {
			args = append(args, "-disposition:a:"+n, "0")
		}
	}
	if len(p.Subtitles) > 0 {
		args = append(args, "-c:s", "mov_text")
	}
	args = append(args, "-movflags", "+faststart", "-f", "mp4", outPath)
	return args
}

// CardProbeArgs is pure: device -> ffmpeg argv (sans binary) that encodes
// one second of a generated picture on the card and writes nothing. The
// worker runs it once at start-up, so the job log says whether the card is
// live before any file is touched, and a box with no card still remuxes.
func CardProbeArgs(device string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-vaapi_device", device,
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24", "-t", "1",
		"-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi", "-f", "null", "-",
	}
}

// ProbeArgs is pure: output path -> ffprobe argv (sans binary) that answers
// what Verify reads: the container's duration and every stream's codec.
func ProbeArgs(outPath string) []string {
	return []string{
		"-v", "error",
		"-show_entries", "format=duration:stream=codec_type,codec_name,channels",
		"-of", "json",
		outPath,
	}
}

// Verify reads a ProbeArgs answer over the finished file and says whether it
// is the file the plan promised: H.264 video, the first audio track AAC at
// two channels or fewer (or the copied stereo the plan kept), as many audio
// and subtitle streams as were mapped, and a duration within two percent (or
// five seconds) of the source's. A file that fails here is never uploaded.
func Verify(probe []byte, p Plan, sourceSeconds float64) error {
	var out struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Type     string `json:"codec_type"`
			Codec    string `json:"codec_name"`
			Channels int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(probe, &out); err != nil {
		return fmt.Errorf("ffprobe answered no JSON: %w", err)
	}
	var video, audio, subs int
	var firstAudioCodec string
	var firstAudioChannels int
	for _, s := range out.Streams {
		switch s.Type {
		case "video":
			video++
			if s.Codec != "h264" {
				return fmt.Errorf("video is %s, want h264", s.Codec)
			}
		case "audio":
			if audio == 0 {
				firstAudioCodec, firstAudioChannels = s.Codec, s.Channels
			}
			audio++
		case "subtitle":
			subs++
		}
	}
	if video != 1 {
		return fmt.Errorf("%d video streams, want 1", video)
	}
	if audio != len(p.Audio) {
		return fmt.Errorf("%d audio streams, want %d", audio, len(p.Audio))
	}
	if audio > 0 && (!stereoAudio[firstAudioCodec] || firstAudioChannels > 2) {
		return fmt.Errorf("first audio is %s %dch, want aac stereo", firstAudioCodec, firstAudioChannels)
	}
	if subs != len(p.Subtitles) {
		return fmt.Errorf("%d subtitle streams, want %d", subs, len(p.Subtitles))
	}
	got, err := strconv.ParseFloat(out.Format.Duration, 64)
	if err != nil {
		return fmt.Errorf("no duration in the probe: %q", out.Format.Duration)
	}
	if sourceSeconds > 0 {
		tolerance := math.Max(5, sourceSeconds*0.02)
		if math.Abs(got-sourceSeconds) > tolerance {
			return fmt.Errorf("duration %.1fs, source %.1fs", got, sourceSeconds)
		}
	}
	return nil
}
