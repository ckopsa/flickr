// Package model holds the domain types: media info, client capabilities,
// server policy, and play decisions.
//
// The capability schema is versioned from day one — clients declare what
// they can play; the server never hardcodes device-model knowledge.
package model

const CapabilitySchemaVersion = 1

// MediaInfo is the result of probing a media file. It is the immutable
// media-side input to the decision engine.
type MediaInfo struct {
	Container       string  `json:"container"`   // "mkv", "mp4", ...
	VideoCodec      string  `json:"video_codec"` // "h264", "hevc", "av1", ...
	AudioCodec      string  `json:"audio_codec"` // "aac", "ac3", "dts", ...
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	DurationSeconds float64 `json:"duration_seconds"`
	BitrateBps      int64   `json:"bitrate_bps"`
	HDR             string  `json:"hdr,omitempty"` // "", "hdr10", "dolbyvision", "hlg"
	AudioChannels   int     `json:"audio_channels"`
	FPS             float64 `json:"fps,omitempty"`
	// Telecine: film content pulldown-flagged to a higher display rate
	// (typical of DVDs). Copying passes the flags through harmlessly, but
	// re-encoding must inverse-telecine or the judder gets baked in.
	Telecine  bool            `json:"telecine,omitempty"`
	Chapters  []Chapter       `json:"chapters,omitempty"`
	Subtitles []SubtitleTrack `json:"subtitles,omitempty"`
	// AudioTracks lists every audio stream (ordinal = index among audio
	// streams, mapping to ffmpeg -map 0:a:<ordinal>). The scalar
	// AudioCodec/AudioChannels fields above stay pinned to the first stream;
	// when a client selects another track (decision.Options.AudioTrack) the
	// decision engine evaluates that track's codec/channels instead.
	AudioTracks []AudioTrack `json:"audio_tracks,omitempty"`
}

// AudioTrack is one audio stream. Default carries ffprobe's
// disposition.default flag (the container's preferred track).
type AudioTrack struct {
	Ordinal  int    `json:"ordinal"`
	Codec    string `json:"codec"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Channels int    `json:"channels"`
	Default  bool   `json:"default"`
}

// SubtitleTrack is an embedded subtitle stream. Ordinal is the index among
// subtitle streams only, so it maps directly to ffmpeg's -map 0:s:<ordinal>.
// Supported means the codec is text-based and convertible to WebVTT; bitmap
// formats (PGS, VobSub) would need OCR, which is out of scope.
type SubtitleTrack struct {
	Ordinal   int    `json:"ordinal"`
	Codec     string `json:"codec"`
	Language  string `json:"language"`
	Title     string `json:"title"`
	Supported bool   `json:"supported"`
	// External marks a sidecar file (Movie.en.srt next to Movie.mkv) rather
	// than an embedded stream; ObjectKey is the sidecar's bucket key, stored
	// so the .vtt endpoint can fetch it without re-listing the bucket.
	External  bool   `json:"external,omitempty"`
	ObjectKey string `json:"object_key,omitempty"`
}

// Enrichment is external metadata (TMDB) layered on top of identity. It is
// derived data: identity is the deterministic input, enrichment the cached
// lookup result — never the other way around.
type Enrichment struct {
	// Version marks which generation of enrichment fields this JSON carries;
	// rows older than the current version (see tmdb.EnrichmentVersion) are
	// re-enriched on the next pass even though their identity is unchanged.
	Version   int      `json:"v,omitempty"`
	TMDBID    int64    `json:"tmdb_id"`
	Title     string   `json:"title"`
	Year      int      `json:"year,omitempty"`
	Overview  string   `json:"overview"`
	HasPoster bool     `json:"has_poster"`
	Genres    []string `json:"genres,omitempty"`
	// Per-episode fields, populated for kind=episode items from the show's
	// TMDB season payload (one call per show-season per run).
	EpisodeTitle    string `json:"episode_title,omitempty"`
	EpisodeOverview string `json:"episode_overview,omitempty"`
	HasStill        bool   `json:"has_still,omitempty"`
}

// Chapter is an embedded chapter marker (scene selection target).
type Chapter struct {
	StartSeconds float64 `json:"start_seconds"`
	Title        string  `json:"title,omitempty"`
}

// ClientCapabilities is what a client declares it can play.
type ClientCapabilities struct {
	SchemaVersion    int      `json:"schema_version"`
	Containers       []string `json:"containers"`
	VideoCodecs      []string `json:"video_codecs"`
	AudioCodecs      []string `json:"audio_codecs"`
	MaxWidth         int      `json:"max_width"`
	MaxHeight        int      `json:"max_height"`
	MaxBitrateBps    int64    `json:"max_bitrate_bps,omitempty"` // 0 = no cap
	SupportsHDR      []string `json:"supports_hdr,omitempty"`
	MaxAudioChannels int      `json:"max_audio_channels"`
	// HLS segment formats the client's player accepts. Not every cast
	// receiver eats fMP4 (some Android-TV receivers are TS-only), and HEVC
	// cannot ride in TS — so this is negotiated, not assumed.
	HLSSegmentFormats []string `json:"hls_segment_formats,omitempty"`
	// Audio codecs the client's player accepts INSIDE an HLS stream. A
	// device can support a codec for direct play (AC-3 out its speakers)
	// while its streaming player rejects the same codec in segments — so
	// direct-play codecs and in-stream codecs are negotiated separately.
	HLSAudioCodecs []string `json:"hls_audio_codecs,omitempty"`
}

// Normalize fills defaults so a sparse manifest from an old or minimal
// client still yields sane decisions.
func (c *ClientCapabilities) Normalize() {
	if c.SchemaVersion == 0 || c.SchemaVersion > CapabilitySchemaVersion {
		c.SchemaVersion = CapabilitySchemaVersion
	}
	if c.MaxWidth == 0 {
		c.MaxWidth = 3840
	}
	if c.MaxHeight == 0 {
		c.MaxHeight = 2160
	}
	if c.MaxAudioChannels == 0 {
		c.MaxAudioChannels = 8
	}
	if len(c.HLSSegmentFormats) == 0 {
		c.HLSSegmentFormats = []string{"ts"}
	}
	if len(c.HLSAudioCodecs) == 0 {
		c.HLSAudioCodecs = []string{"aac"} // the one universally safe HLS audio
	}
}

// ServerPolicy is the server-side transcode policy, independent of any client.
type ServerPolicy struct {
	AllowTranscode           bool   `json:"allow_transcode"`
	MaxTranscodeHeight       int    `json:"max_transcode_height"`
	TranscodeVideoCodec      string `json:"transcode_video_codec"`
	TranscodeAudioCodec      string `json:"transcode_audio_codec"`
	TranscodeVideoBitrateBps int64  `json:"transcode_video_bitrate_bps"`
	TranscodeAudioBitrateBps int64  `json:"transcode_audio_bitrate_bps"`
}

func DefaultPolicy() ServerPolicy {
	return ServerPolicy{
		AllowTranscode:           true,
		MaxTranscodeHeight:       1080,
		TranscodeVideoCodec:      "h264",
		TranscodeAudioCodec:      "aac",
		TranscodeVideoBitrateBps: 8_000_000,
		TranscodeAudioBitrateBps: 192_000,
	}
}

type PlayMethod string

const (
	DirectPlay PlayMethod = "direct_play"
	Transcode  PlayMethod = "transcode"
	Deny       PlayMethod = "deny"
)

// TraceStep is one line of the "why did it do that?" answer.
type TraceStep struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// TranscodeTarget describes the minimal transcode: nil-codec means copy.
type TranscodeTarget struct {
	VideoCodec      string  `json:"video_codec,omitempty"` // "" = copy
	AudioCodec      string  `json:"audio_codec,omitempty"` // "" = copy
	Height          int     `json:"height,omitempty"`      // 0 = keep
	VideoBitrateBps int64   `json:"video_bitrate_bps,omitempty"`
	AudioBitrateBps int64   `json:"audio_bitrate_bps,omitempty"`
	Tonemap         bool    `json:"tonemap,omitempty"`
	SegmentFormat   string  `json:"segment_format,omitempty"` // "ts" (default) or "fmp4"
	Detelecine      bool    `json:"detelecine,omitempty"`     // inverse-telecine before encoding
	FPS             float64 `json:"fps,omitempty"`            // effective output fps (keyframe cadence)
	// AudioStreamOrdinal is which audio stream to feed the transcode
	// (ffmpeg -map 0:a:N). 0 = first stream, the historical behavior.
	// Only meaningful for transcode/remux jobs — direct play hands the
	// client the whole file and it negotiates tracks natively.
	AudioStreamOrdinal int `json:"audio_stream_ordinal,omitempty"`
	// BurnSubtitleOrdinal, when set, is the embedded subtitle stream
	// (ffmpeg -map 0:s:M) to burn into the video. Burning always implies a
	// video re-encode (VideoCodec is never "" when this is set).
	BurnSubtitleOrdinal *int `json:"burn_subtitle_ordinal,omitempty"`
	// Renditions is the adaptive-bitrate ladder, present only when video is
	// being re-encoded anyway (decode cost already paid): the primary rung
	// first, then lower quality rungs. Empty/single-entry = plain HLS.
	Renditions []Rendition `json:"renditions,omitempty"`
}

// Rendition is one rung of an ABR ladder. Height 0 means "keep source
// height" (only ever the primary rung).
type Rendition struct {
	Height          int   `json:"height"`
	VideoBitrateBps int64 `json:"video_bitrate_bps"`
}

// PlayDecision is the decision engine's output, trace included.
type PlayDecision struct {
	Method PlayMethod       `json:"method"`
	Target *TranscodeTarget `json:"target,omitempty"`
	Trace  []TraceStep      `json:"trace"`
}

// Identity is the file-to-media identification result — a deterministic,
// user-overridable step kept separate from metadata enrichment.
// Kind "extra" is bonus material (featurettes, deleted scenes) that belongs
// to the work named by Title but is not one of its episodes: Season/Episode,
// when present, say which episode it accompanies.
type Identity struct {
	Kind    string `json:"kind"` // "movie", "episode", "extra", "unknown"
	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
}
