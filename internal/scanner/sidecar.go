package scanner

import (
	"path"
	"sort"
	"strings"

	"flickr/internal/model"
)

// External subtitle sidecars: non-video objects sitting next to a video whose
// basename extends the video's stem (Movie.mkv + Movie.srt, Movie.en.srt,
// Movie.en.forced.srt). They are discovered during the scan's listing pass —
// no extra bucket requests — and appended to media_info.subtitles after the
// embedded tracks.
//
// .sub is deliberately NOT recognized: the extension is ambiguous between
// MicroDVD (text, but frame-numbered — converting needs the video's fps) and
// VobSub (bitmap, would need OCR). Neither can be served as WebVTT reliably,
// so listing them would only advertise tracks we can't deliver.
var subtitleExts = map[string]string{ // ext -> codec name (matching ffprobe's vocabulary)
	".srt": "subrip",
	".ass": "ass",
	".ssa": "ass",
	".vtt": "webvtt",
}

// sidecarFile is one subtitle object found in the bucket.
type sidecarFile struct {
	Key  string
	ETag string
}

// isSubtitleKey reports whether an object key looks like a subtitle sidecar
// we can serve (AppleDouble junk excluded, same as for videos).
func isSubtitleKey(key string) bool {
	if strings.HasPrefix(path.Base(key), "._") {
		return false
	}
	_, ok := subtitleExts[strings.ToLower(path.Ext(key))]
	return ok
}

// matchSidecars filters a directory's subtitle files down to those belonging
// to the given video: same directory, basename = video stem + "." + anything.
// Requiring the "." right after the stem keeps "Movie 2.srt" from attaching
// to "Movie.mkv". Results are sorted by key so ordinals and the change
// signature are deterministic across scans.
func matchSidecars(videoKey string, dirSubs []sidecarFile) []sidecarFile {
	stem := strings.TrimSuffix(path.Base(videoKey), path.Ext(videoKey))
	dir := path.Dir(videoKey)
	var out []sidecarFile
	for _, s := range dirSubs {
		if path.Dir(s.Key) != dir {
			continue
		}
		rest, ok := strings.CutPrefix(path.Base(s.Key), stem)
		if !ok || !strings.HasPrefix(rest, ".") {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// sidecarSignature is a change fingerprint over the matched sidecar set
// (keys + etags). It is folded into the scan's skip decision so that adding,
// removing, or replacing a sidecar re-attaches subtitles without re-probing
// the (unchanged) video. Empty set -> empty string, matching the column
// default for rows written before sidecars existed.
func sidecarSignature(subs []sidecarFile) string {
	if len(subs) == 0 {
		return ""
	}
	parts := make([]string, len(subs))
	for i, s := range subs {
		parts[i] = s.Key + "=" + s.ETag
	}
	return strings.Join(parts, ";")
}

// langCodes normalizes filename language suffixes to the 3-letter codes
// ffprobe reports for embedded tracks (ISO 639-2/B), so external and
// embedded tracks speak the same vocabulary. 2-letter keys are ISO 639-1;
// 3-letter values pass through via normalizeLang.
var langCodes = map[string]string{
	"en": "eng", "es": "spa", "fr": "fre", "de": "ger", "it": "ita",
	"pt": "por", "ru": "rus", "ja": "jpn", "zh": "chi", "ko": "kor",
	"nl": "dut", "sv": "swe", "no": "nor", "da": "dan", "fi": "fin",
	"pl": "pol", "cs": "cze", "tr": "tur", "ar": "ara", "he": "heb",
	"hi": "hin", "el": "gre", "hu": "hun", "ro": "rum", "th": "tha",
	"vi": "vie", "id": "ind", "uk": "ukr",
}

var threeLetterCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range langCodes {
		m[v] = true
	}
	// 639-2/T spellings that also show up in the wild.
	for _, v := range []string{"fra", "deu", "ita", "spa", "nld", "swe", "ces", "zho", "ell", "ron"} {
		m[v] = true
	}
	return m
}()

// sidecarLanguage extracts a language from the suffix chain between video
// stem and extension ("en.forced" -> "eng"). The first recognized token
// wins; unrecognized chains yield "".
func sidecarLanguage(suffix string) string {
	for _, tok := range strings.Split(suffix, ".") {
		tok = strings.ToLower(tok)
		if code, ok := langCodes[tok]; ok {
			return code
		}
		if threeLetterCodes[tok] {
			return tok
		}
	}
	return ""
}

// externalTracks maps matched sidecar files to subtitle tracks, numbering
// ordinals from nextOrdinal (i.e. after the embedded tracks). Title is the
// raw suffix chain ("en.forced") when present — it carries the human-meaning
// bits like "forced"/"sdh" — else the sidecar's filename.
func externalTracks(videoKey string, subs []sidecarFile, nextOrdinal int) []model.SubtitleTrack {
	stem := strings.TrimSuffix(path.Base(videoKey), path.Ext(videoKey))
	var out []model.SubtitleTrack
	for _, s := range subs {
		base := path.Base(s.Key)
		ext := strings.ToLower(path.Ext(base))
		codec, ok := subtitleExts[ext]
		if !ok {
			continue
		}
		// Suffix chain: everything between the video stem and the subtitle
		// extension ("Movie.en.forced.srt" -> "en.forced", "Movie.srt" -> "").
		// Trimmed by length, not TrimSuffix, so extension casing can't leak
		// into the chain.
		suffix := strings.Trim(strings.TrimPrefix(base, stem)[:len(base)-len(stem)-len(ext)], ".")
		title := suffix
		if title == "" {
			title = base
		}
		out = append(out, model.SubtitleTrack{
			Ordinal:   nextOrdinal + len(out),
			Codec:     codec,
			Language:  sidecarLanguage(suffix),
			Title:     title,
			Supported: true,
			External:  true,
			ObjectKey: s.Key,
		})
	}
	return out
}

// attachSidecars replaces any external subtitle entries on info with tracks
// derived from the current sidecar set, keeping embedded tracks (and their
// ordinals) untouched. Safe to call on a freshly-probed info (no external
// entries yet) and on a stored one being refreshed without a re-probe.
func attachSidecars(info *model.MediaInfo, videoKey string, subs []sidecarFile) {
	if info == nil {
		return
	}
	embedded := info.Subtitles[:0]
	for _, t := range info.Subtitles {
		if !t.External {
			embedded = append(embedded, t)
		}
	}
	info.Subtitles = append(embedded, externalTracks(videoKey, subs, len(embedded))...)
	if len(info.Subtitles) == 0 {
		info.Subtitles = nil // keep "no subtitles" as JSON-omitted, like parseProbe
	}
}
