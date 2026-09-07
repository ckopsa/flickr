// Package model holds the domain types: media info, client capabilities,
// server policy, and play decisions.
//
// The capability schema is versioned from day one — clients declare what
// they can play; the server never hardcodes device-model knowledge.
package model

import "strings"

const CapabilitySchemaVersion = 1

// Medium is the broad class of a media file — what kind of player it needs
// rather than what codec it carries. Video is the medium every item had
// before the field existed; audio (audiobooks, albums) and text (books)
// joined later, so a stored MediaInfo with an empty Medium is video, and
// readers go through MediumOrVideo rather than comparing the field directly.
const (
	MediumVideo = "video"
	MediumAudio = "audio"
	MediumText  = "text"
)

// Media is the set of admitted Medium values.
var Media = map[string]bool{MediumVideo: true, MediumAudio: true, MediumText: true}

// MediaInfo is the result of probing a media file. It is the immutable
// media-side input to the decision engine.
type MediaInfo struct {
	// Medium is "video", "audio" or "text" (see the Medium constants). Empty
	// in JSON written before the field existed, which always meant video.
	Medium          string  `json:"medium"`
	Container       string  `json:"container"`   // "mkv", "mp4", "m4b", "epub", ...
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
	Telecine bool `json:"telecine,omitempty"`
	// Chapters are the file's own navigation points: embedded chapter
	// markers for video and audio; for text, one entry per spine item (a
	// book has no clock, so every StartSeconds is 0 and Title carries the
	// section's TOC label or its spine id).
	Chapters []Chapter `json:"chapters,omitempty"`
	// Sections is the spine length of a text item — how many reading-order
	// documents the book is made of. 0 for video and audio.
	Sections int `json:"sections,omitempty"`
	// PageCount is a PDF's page count, read from its page tree at probe time
	// (0 when the file would not say; the reader then counts for itself). A
	// PDF has pages where an EPUB has sections: fixed, numbered, the unit
	// its locator speaks. 0 for everything that is not a PDF.
	PageCount int `json:"page_count,omitempty"`
	// Document is what a text file says about itself (its package metadata),
	// as opposed to what its path says (Identity). Only set for text.
	Document  *Document       `json:"document,omitempty"`
	Subtitles []SubtitleTrack `json:"subtitles,omitempty"`
	// AudioTracks lists every audio stream (ordinal = index among audio
	// streams, mapping to ffmpeg -map 0:a:<ordinal>). The scalar
	// AudioCodec/AudioChannels fields above stay pinned to the first stream;
	// when a client selects another track (decision.Options.AudioTrack) the
	// decision engine evaluates that track's codec/channels instead.
	AudioTracks []AudioTrack `json:"audio_tracks,omitempty"`
}

// MediumOrVideo returns the item's medium, reading the empty value stored by
// scans that predate the field as video — every item was a video then.
func (m *MediaInfo) MediumOrVideo() string {
	if m == nil || m.Medium == "" {
		return MediumVideo
	}
	return m.Medium
}

// Document is the embedded metadata of a text item (EPUB package metadata:
// dc:title, dc:creator, dc:language; a PDF's Info dictionary: /Title and
// /Author as Creator). It is recorded, not trusted over the path: identity
// stays deterministic from the object key alone.
type Document struct {
	Title    string `json:"title,omitempty"`
	Creator  string `json:"creator,omitempty"`
	Language string `json:"language,omitempty"`
}

// Locator is a place in a text — what position_seconds is to a film. A book
// has no clock, so the reader reports the EPUB CFI of the page it shows,
// the 1-based spine section that page is in (the "chapter" of the progress
// text; 0 = unknown) and the book's own percentage as a fraction in [0,1].
// A PDF's place is its Page (1-based; 0 = not a PDF place) with the same
// fraction beside it — page over page count, as the reader measured it.
// It is stored as one JSON value in playback_state.locator, beside
// position_seconds, and is nil for anything that is not text.
type Locator struct {
	CFI      string  `json:"cfi,omitempty"`
	Section  int     `json:"section,omitempty"`
	Page     int     `json:"page,omitempty"`
	Fraction float64 `json:"fraction"`
}

// SectionFromCFI reads the spine position out of an EPUB CFI without any
// knowledge of the book: the step after the package's spine element names
// the itemref as an even child index, so "epubcfi(/6/14[ch07]!/4/2/1:0)" is
// itemref 14/2 = the 7th spine item (1-based). Returns 0 when the string is
// not a CFI with a spine step. The section a client sends explicitly wins
// over this; it is the fallback for a client that reports only the CFI.
func SectionFromCFI(cfi string) int {
	s, ok := strings.CutPrefix(strings.TrimSpace(cfi), "epubcfi(/")
	if !ok {
		return 0
	}
	// Skip the first step (the spine element within the package document).
	i := strings.IndexByte(s, '/')
	if i < 0 {
		return 0
	}
	n := 0
	for _, c := range s[i+1:] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0
		}
	}
	if n < 2 || n%2 != 0 {
		return 0
	}
	return n / 2
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
	// HasBackdrop is the wide still a home screen leads with — the same
	// on-disk discipline as the poster, cached under data/backdrops.
	HasBackdrop bool `json:"has_backdrop,omitempty"`
	// What a detail page says beside the title: how long it runs (a show's
	// is its typical episode), the US certification a kids filter reads,
	// and the top-billed names. Each is absent when TMDB did not know it.
	RuntimeMinutes int      `json:"runtime_minutes,omitempty"`
	Certification  string   `json:"certification,omitempty"`
	Cast           []string `json:"cast,omitempty"`
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
	// VideoCodecs has the same split, for the same reason: a Chromecast
	// Ultra decodes HEVC happily when handed the whole file, and its HLS
	// player will not touch HEVC in a segment. Empty means "whatever the
	// device decodes, its streaming player accepts too" — the assumption
	// every client made before this field existed.
	HLSVideoCodecs []string `json:"hls_video_codecs,omitempty"`
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
	if len(c.HLSVideoCodecs) == 0 {
		// Unstated: assume the streaming player takes whatever the device
		// decodes. That is what every client got before this field existed,
		// so an old or minimal manifest keeps its exact behavior.
		c.HLSVideoCodecs = c.VideoCodecs
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
	// AudioOnly marks a transcode of an audio item: the job maps no video
	// stream at all (-vn), never carries a ladder, and VideoCodec is always
	// "" — which for an audio job means "there is no picture", not "copy the
	// video". Set by the decision engine for medium "audio".
	AudioOnly bool `json:"audio_only,omitempty"`
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
//
// Kind names what the file IS within its work:
//   - "movie", "episode" — video; Title names the film or the show.
//   - "extra" — bonus material (featurettes, deleted scenes) that belongs to
//     the work named by Title but is not one of its episodes: Season/Episode,
//     when present, say which episode it accompanies.
//   - "audiobook_part" — one file of an audiobook; Title is the book, Author
//     its author, Part its position (0 = a single-file book, or unnumbered).
//   - "track" — one file of an album; Title is the ALBUM (the work), Author
//     the artist, Part the track number, TrackTitle the track's own name.
//   - "book" — a text file; Title and Author name it, Part is unused.
//   - "unknown" — the path said nothing.
type Identity struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Author is the audiobook's author, the album's artist, the book's
	// author — whatever the directory grammar names between the category
	// and the title. Empty for video kinds.
	Author  string `json:"author,omitempty"`
	Year    int    `json:"year,omitempty"`
	Season  int    `json:"season,omitempty"`
	Episode int    `json:"episode,omitempty"`
	// Part orders the files of a multi-file audio work: an audiobook part or
	// a track number. 0 = unnumbered (or the work is a single file).
	Part int `json:"part,omitempty"`
	// TrackTitle is a track's own name — the filename after its ordinal
	// ("07 Karma Police.flac" → "Karma Police"), or the work's title when the
	// file is the whole work. Only kind "track" carries it: an audiobook's
	// parts are numbered, not named, and every other kind names its work in
	// Title.
	TrackTitle string `json:"track_title,omitempty"`
}
