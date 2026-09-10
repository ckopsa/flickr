package pipeline

// Bitmap subtitles read into text. MP4 cannot carry a PGS or VobSub track,
// and a picture of a word is not a word a <track> can show or a search can
// find — so before a file is converted, each of its picture tracks becomes a
// .srt sidecar beside it, in the scanner's own grammar (Movie.eng.srt), and
// the scan attaches it to the new file exactly as it would to the old.
//
// Three tools, one pass: ffmpeg pulls the picture tracks out of the source
// (from the URL — the source is never downloaded whole) into a small local
// MKV; mkvextract splits that into one .sup per PGS track and one .idx/.sub
// per VobSub track; pgsrip reads PGS and subtile-ocr reads VobSub, both over
// tesseract. Every argv here is pure; the runner is the worker's.

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"flickr/internal/model"
)

// ocrLanguage is the tesseract language a track is read in, or "" for a
// track the OCR cannot read. Only English is installed; a track with no
// language tag is taken as English, which is what an unlabelled disc rip is.
func ocrLanguage(lang string) string {
	switch strings.ToLower(lang) {
	case "", "eng", "en", "und":
		return "eng"
	}
	return ""
}

// ExtractSubsArgs is pure: source URL + the picture tracks -> ffmpeg argv
// (sans binary) that copies just those streams into a small MKV. Order is
// kept, so track N of the MKV is the Nth of the list.
func ExtractSubsArgs(inputURL string, tracks []model.SubtitleTrack, outMKV string) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", inputURL, "-vn", "-an", "-dn"}
	for _, t := range tracks {
		args = append(args, "-map", "0:s:"+strconv.Itoa(t.Ordinal))
	}
	return append(args, "-c:s", "copy", "-f", "matroska", outMKV)
}

// BitmapFile is where mkvextract writes the Nth track of that MKV: a .sup
// for PGS, an .idx (with its .sub beside it) for VobSub. The PGS name
// carries the language the way pgsrip wants it (track0.en.sup): pgsrip
// filters its inputs by that suffix and rips nothing from a file without it.
func BitmapFile(dir string, n int, t model.SubtitleTrack) string {
	if t.Codec == "dvd_subtitle" {
		return path.Join(dir, "track"+strconv.Itoa(n)+".idx")
	}
	return path.Join(dir, "track"+strconv.Itoa(n)+"."+pgsripLanguage(ocrLanguage(t.Language))+".sup")
}

// SRTFile is where the OCR writes the text for BitmapFile: the same name
// with .srt, which is where pgsrip puts it unasked and where subtile-ocr is
// told to.
func SRTFile(bitmap string) string {
	return strings.TrimSuffix(bitmap, path.Ext(bitmap)) + ".srt"
}

// palette matches the palette line of a VobSub .idx. mkvextract writes the
// sixteen colours with whatever spacing the disc's own idx had — "FDFDFD,D50ECA"
// beside "35C7EF, 000000" — and subtile-ocr's parser wants exactly ", ".
var palette = regexp.MustCompile(`(?m)^palette:.*$`)

// NormalizeIDX rewrites a VobSub .idx so its palette line is spaced the one
// way subtile-ocr reads. Everything else passes through untouched.
func NormalizeIDX(idx []byte) []byte {
	return palette.ReplaceAllFunc(idx, func(line []byte) []byte {
		rest := strings.TrimPrefix(string(line), "palette:")
		var colours []string
		for _, c := range strings.Split(rest, ",") {
			if c = strings.TrimSpace(c); c != "" {
				colours = append(colours, c)
			}
		}
		return []byte("palette: " + strings.Join(colours, ", "))
	})
}

// MKVExtractArgs is pure: the small MKV + its tracks -> mkvextract argv
// (sans binary) that writes each track to BitmapFile.
func MKVExtractArgs(mkv, dir string, tracks []model.SubtitleTrack) []string {
	args := []string{"tracks", mkv}
	for n, t := range tracks {
		args = append(args, strconv.Itoa(n)+":"+BitmapFile(dir, n, t))
	}
	return args
}

// OCRCommand is pure: one extracted track -> the tool and its argv that
// writes the .srt at out. pgsrip writes beside its input under the input's
// name, so it is pointed at a copy named for the output; subtile-ocr takes
// -o.
func OCRCommand(t model.SubtitleTrack, in, out string) (name string, args []string) {
	lang := ocrLanguage(t.Language)
	if t.Codec == "dvd_subtitle" {
		return "subtile-ocr", []string{"-l", lang, "-o", out, in}
	}
	return "pgsrip", []string{"-l", pgsripLanguage(lang), "--force", in}
}

// pgsripLanguage is the two-letter form pgsrip's -l takes for the
// three-letter tesseract code the rest of this file speaks.
func pgsripLanguage(lang string) string {
	if lang == "eng" {
		return "en"
	}
	return lang
}

// SidecarKeys is pure: the object key and its picture tracks -> the bucket
// key each track's .srt is written to, in the scanner's grammar:
// <stem>.<lang>.srt, and <stem>.<lang>.<n>.srt for a second track in the
// same language (the scanner reads the language from the first token it
// knows and shows the rest as the title).
func SidecarKeys(objectKey string, tracks []model.SubtitleTrack) []string {
	stem := strings.TrimSuffix(objectKey, path.Ext(objectKey))
	seen := map[string]int{}
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		lang := ocrLanguage(t.Language)
		seen[lang]++
		if seen[lang] == 1 {
			out = append(out, fmt.Sprintf("%s.%s.srt", stem, lang))
		} else {
			out = append(out, fmt.Sprintf("%s.%s.%d.srt", stem, lang, seen[lang]))
		}
	}
	return out
}
