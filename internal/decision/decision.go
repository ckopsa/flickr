// Package decision is the playback decision engine.
//
// Decide is a pure, side-effect-free function:
//
//	(MediaInfo, ClientCapabilities, ServerPolicy) -> PlayDecision
//
// No I/O, no clock, no globals — so thousands of combinations can be
// unit-tested, and every decision carries a human-readable trace answering
// "why did this transcode?".
package decision

import (
	"fmt"
	"slices"
	"strings"

	"flickr/internal/model"
)

func Decide(media model.MediaInfo, caps model.ClientCapabilities, policy model.ServerPolicy) model.PlayDecision {
	caps.Normalize()
	var trace []model.TraceStep
	check := func(name string, passed bool, detail string) bool {
		trace = append(trace, model.TraceStep{Check: name, Passed: passed, Detail: detail})
		return passed
	}

	containerOK := check("container",
		slices.Contains(caps.Containers, media.Container),
		fmt.Sprintf("file is %s; client plays %s", media.Container, strings.Join(caps.Containers, ", ")))

	videoOK := check("video_codec",
		slices.Contains(caps.VideoCodecs, media.VideoCodec),
		fmt.Sprintf("file is %s; client plays %s", media.VideoCodec, strings.Join(caps.VideoCodecs, ", ")))

	audioOK := check("audio_codec",
		slices.Contains(caps.AudioCodecs, media.AudioCodec),
		fmt.Sprintf("file is %s; client plays %s", media.AudioCodec, strings.Join(caps.AudioCodecs, ", ")))

	resOK := check("resolution",
		media.Width <= caps.MaxWidth && media.Height <= caps.MaxHeight,
		fmt.Sprintf("file is %dx%d; client max %dx%d", media.Width, media.Height, caps.MaxWidth, caps.MaxHeight))

	bitrateOK := true
	if caps.MaxBitrateBps > 0 {
		bitrateOK = check("bitrate",
			media.BitrateBps <= caps.MaxBitrateBps,
			fmt.Sprintf("file is %d kbps; client cap %d kbps", media.BitrateBps/1000, caps.MaxBitrateBps/1000))
	}

	hdrOK := true
	if media.HDR != "" {
		supported := "none"
		if len(caps.SupportsHDR) > 0 {
			supported = strings.Join(caps.SupportsHDR, ", ")
		}
		hdrOK = check("hdr",
			slices.Contains(caps.SupportsHDR, media.HDR),
			fmt.Sprintf("file is %s; client supports %s", media.HDR, supported))
	}

	channelsOK := check("audio_channels",
		media.AudioChannels <= caps.MaxAudioChannels,
		fmt.Sprintf("file has %d channels; client max %d", media.AudioChannels, caps.MaxAudioChannels))

	if containerOK && videoOK && audioOK && resOK && bitrateOK && hdrOK && channelsOK {
		trace = append(trace, model.TraceStep{Check: "verdict", Passed: true,
			Detail: "all checks passed — direct play"})
		return model.PlayDecision{Method: model.DirectPlay, Trace: trace}
	}

	if !policy.AllowTranscode {
		trace = append(trace, model.TraceStep{Check: "verdict", Passed: false,
			Detail: "incompatible and server policy forbids transcoding"})
		return model.PlayDecision{Method: model.Deny, Trace: trace}
	}

	// Build the minimal transcode: copy every stream that is already
	// compatible. Video must be re-encoded if codec, resolution, bitrate,
	// or HDR failed (tone-mapping and scaling require a decode/encode cycle).
	needsVideo := !(videoOK && resOK && bitrateOK && hdrOK)
	needsAudio := !(audioOK && channelsOK)

	// Copying audio into the HLS stream also requires the client's
	// streaming player to accept that codec in-stream — a separate question
	// from whether the device can decode it at all.
	if !needsAudio && !slices.Contains(caps.HLSAudioCodecs, media.AudioCodec) {
		trace = append(trace, model.TraceStep{Check: "hls_audio", Passed: false,
			Detail: fmt.Sprintf("client plays %s directly but not inside HLS (accepts %s in-stream) — re-encoding",
				media.AudioCodec, strings.Join(caps.HLSAudioCodecs, ", "))})
		needsAudio = true
	}

	target := &model.TranscodeTarget{FPS: media.FPS}
	var parts []string
	if needsVideo {
		target.VideoCodec = policy.TranscodeVideoCodec
		target.VideoBitrateBps = policy.TranscodeVideoBitrateBps
		parts = append(parts, "re-encode video to "+policy.TranscodeVideoCodec)
		if media.Telecine {
			// Baking pulldown into a hard re-encode produces rhythmic
			// judder; restore the underlying film rate instead.
			target.Detelecine = true
			target.FPS = media.FPS * 4 / 5
			parts = append(parts, "inverse telecine")
			trace = append(trace, model.TraceStep{Check: "telecine", Passed: true,
				Detail: fmt.Sprintf("soft telecine detected — inverse telecine to %.3f fps", target.FPS)})
		}
		if media.HDR != "" && !hdrOK {
			target.Tonemap = true
			parts = append(parts, "tone-map HDR to SDR")
		}
		if !resOK || media.Height > policy.MaxTranscodeHeight {
			target.Height = min(caps.MaxHeight, policy.MaxTranscodeHeight)
			parts = append(parts, fmt.Sprintf("scale to %dp", target.Height))
		}
	} else {
		parts = append(parts, "copy video")
	}
	if needsAudio {
		target.AudioCodec = policy.TranscodeAudioCodec
		target.AudioBitrateBps = policy.TranscodeAudioBitrateBps
		parts = append(parts, "re-encode audio to "+policy.TranscodeAudioCodec)
	} else {
		parts = append(parts, "copy audio")
	}

	// Segment-format negotiation: TS is the lowest common denominator, but
	// HEVC cannot ride in TS. If the output video is HEVC and the client
	// accepts fMP4, use it; if it doesn't, force an h264 re-encode.
	target.SegmentFormat = "ts"
	outputVideo := media.VideoCodec
	if target.VideoCodec != "" {
		outputVideo = target.VideoCodec
	}
	if outputVideo == "hevc" {
		if slices.Contains(caps.HLSSegmentFormats, "fmp4") {
			target.SegmentFormat = "fmp4"
			trace = append(trace, model.TraceStep{Check: "segment_format", Passed: true,
				Detail: "hevc output needs fMP4 segments; client accepts fmp4"})
		} else {
			target.VideoCodec = policy.TranscodeVideoCodec
			target.VideoBitrateBps = policy.TranscodeVideoBitrateBps
			parts = append(parts, "re-encode hevc to "+policy.TranscodeVideoCodec)
			trace = append(trace, model.TraceStep{Check: "segment_format", Passed: false,
				Detail: "hevc cannot ride in TS and client rejects fMP4 — re-encoding video to " +
					policy.TranscodeVideoCodec})
		}
	}

	// ABR ladder: only when video is being re-encoded anyway — that is
	// exactly when the (CPU-bound) decode cost is already being paid, and the
	// VPU handles the extra encodes. Copy/remux and audio-only transcodes
	// stay single-rendition.
	if target.VideoCodec != "" {
		target.Renditions = buildLadder(media, caps, *target)
		trace = append(trace, model.TraceStep{Check: "abr_ladder", Passed: true,
			Detail: "ladder: " + describeLadder(media, target.Renditions)})
	}

	trace = append(trace, model.TraceStep{Check: "verdict", Passed: true,
		Detail: "transcode — " + strings.Join(parts, ", ")})
	return model.PlayDecision{Method: model.Transcode, Target: target, Trace: trace}
}

// ladderRungs are the fixed lower-quality rungs offered below the primary
// transcode target.
var ladderRungs = []model.Rendition{
	{Height: 720, VideoBitrateBps: 3_000_000},
	{Height: 480, VideoBitrateBps: 1_200_000},
}

// buildLadder assembles the ABR ladder for a video re-encode: the primary
// target rung first, then any fixed rung strictly below the primary height
// and within the client's height cap. Always at least the primary rung.
func buildLadder(media model.MediaInfo, caps model.ClientCapabilities, t model.TranscodeTarget) []model.Rendition {
	primaryHeight := t.Height
	if primaryHeight == 0 {
		primaryHeight = media.Height // Height 0 = keep source height
	}
	ladder := []model.Rendition{{Height: t.Height, VideoBitrateBps: t.VideoBitrateBps}}
	for _, r := range ladderRungs {
		if r.Height >= primaryHeight {
			continue // never offer a rung at or above the primary
		}
		if caps.MaxHeight > 0 && r.Height > caps.MaxHeight {
			continue
		}
		ladder = append(ladder, r)
	}
	return ladder
}

// describeLadder renders a ladder for the trace, e.g.
// "1080p@8000kbps, 720p@3000kbps, 480p@1200kbps".
func describeLadder(media model.MediaInfo, rungs []model.Rendition) string {
	var parts []string
	for _, r := range rungs {
		h := r.Height
		if h == 0 {
			h = media.Height
		}
		parts = append(parts, fmt.Sprintf("%dp@%dkbps", h, r.VideoBitrateBps/1000))
	}
	return strings.Join(parts, ", ")
}
