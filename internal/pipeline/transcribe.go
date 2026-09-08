package pipeline

// Transcription: subtitles for a file that carries none. ffmpeg pulls the
// audio down as the 16 kHz mono PCM wav whisper.cpp insists on, whisper
// writes the WebVTT beside it, and the finished cues are renamed into place —
// a killed run leaves nothing behind to be served forever.
//
// Both argv builders are pure and table-tested; the runner that spends the
// hours takes an exec seam, so the whole flow can be driven by a test without
// a whisper binary, an ffmpeg, or a byte of audio.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// transcribeTimeout bounds one item end to end. Whisper on a CPU runs at
// something like real time, so a long film is hours of work — but a wedged
// run must not hold the stage for a day.
const transcribeTimeout = 6 * time.Hour

// TranscribeAudioArgs is pure: input URL -> ffmpeg argv (sans binary) that
// writes the audio as the 16 kHz mono signed-16-bit wav whisper.cpp reads.
// Nothing else is decoded: the picture is the expensive half and no cue
// comes out of it.
func TranscribeAudioArgs(inputURL, outWav string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", inputURL,
		"-vn", "-sn", "-dn",
		"-ac", "1", "-ar", "16000",
		"-c:a", "pcm_s16le", "-f", "wav",
		outWav,
	}
}

// WhisperArgs is pure: model + wav -> whisper.cpp argv (sans binary) that
// writes <outPrefix>.vtt. An empty language means auto-detect, which is what
// a library of files in no particular language wants. Zero threads is
// whisper's own default; a box that transcribes for a living names its own.
func WhisperArgs(model, wav, language, outPrefix string, threads int) []string {
	if language == "" {
		language = "auto"
	}
	args := []string{
		"-m", model,
		"-f", wav,
		"-l", language,
		"-ovtt",
		"-of", outPrefix,
	}
	if threads > 0 {
		args = append(args, "-t", strconv.Itoa(threads))
	}
	return args
}

// detectedLang matches the line whisper.cpp prints when it has worked the
// language out for itself: "auto-detected language: en (p = 0.98)".
var detectedLang = regexp.MustCompile(`auto-detected language:\s*([a-zA-Z]{2,3})`)

// DetectedLanguage is the language whisper says it heard, or "" when it did
// not say. The transcript row records it, so a track can be labelled in the
// language it is actually in.
func DetectedLanguage(out []byte) string {
	m := detectedLang.FindSubmatch(out)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// Transcriber runs whisper.cpp over one file's audio. Bin and Model are the
// TRANSCRIBE and WHISPER_MODEL settings; a zero Transcriber is not usable,
// which is the point — without the two settings there is no stage.
type Transcriber struct {
	Bin      string // the whisper.cpp CLI (whisper-cli)
	Model    string // a ggml model file
	Language string // "" or "auto": let whisper decide
	Threads  int    // 0: whisper's own default
	// Run is the exec seam. Nil means really run the command; a test
	// substitutes it and never shells out.
	Run func(ctx context.Context, name string, args []string) ([]byte, error)
}

func (t *Transcriber) run(ctx context.Context, name string, args []string) ([]byte, error) {
	if t.Run != nil {
		return t.Run(ctx, name, args)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Transcribe writes the generated WebVTT for one file at destPath and reports
// the language it is in. The work happens in a temp directory beside the
// destination, so what appears at destPath is always a complete transcript.
func (t *Transcriber) Transcribe(ctx context.Context, inputURL, destPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, "transcribe-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // no-op for the one file that gets renamed out

	wav := filepath.Join(tmp, "audio.wav")
	if out, err := t.run(ctx, "ffmpeg", TranscribeAudioArgs(inputURL, wav)); err != nil {
		return "", fmt.Errorf("ffmpeg transcribe extract: %v: %s", err, tail(out, 500))
	}
	prefix := filepath.Join(tmp, "transcript")
	out, err := t.run(ctx, t.Bin, WhisperArgs(t.Model, wav, t.Language, prefix, t.Threads))
	if err != nil {
		return "", fmt.Errorf("whisper: %v: %s", err, tail(out, 500))
	}
	vtt := prefix + ".vtt"
	if _, err := os.Stat(vtt); err != nil {
		return "", fmt.Errorf("whisper wrote no vtt: %s", tail(out, 500))
	}
	lang := t.Language
	if lang == "" || lang == "auto" {
		lang = DetectedLanguage(out)
	}
	return lang, os.Rename(vtt, destPath)
}

// ── the cues, read back ─────────────────────────────────────────────────
//
// The transcript is written as WebVTT because that is what a <track> reads.
// It is also the only place the words of a film are written down, so it is
// read back here into cues a search index can hold: ParseVTT is pure — bytes
// in, cues out — and the stage that stores them owns no grammar of its own.

// Cue is one line of dialogue and when it is said, in seconds.
type Cue struct {
	Start float64
	End   float64
	Text  string
}

// cueTiming matches a WebVTT timing line: "00:01:02.500 --> 00:01:05.000",
// with or without the hour, and with whatever cue settings follow the end.
var cueTiming = regexp.MustCompile(`^((?:\d+:)?\d{1,2}:\d{1,2}(?:[.,]\d{1,3})?)\s*-->\s*((?:\d+:)?\d{1,2}:\d{1,2}(?:[.,]\d{1,3})?)`)

// vttTag is the markup a cue may carry — <v Speaker>, <i>, <00:00:01.000> —
// none of which is a word anybody searched for.
var vttTag = regexp.MustCompile(`</?[^>]*>`)

// ParseVTT reads a WebVTT file into its cues. A cue with no words, or one
// whose timing line does not parse, is not a line of dialogue and is
// dropped; NOTE, STYLE and REGION blocks are not cues at all. The lines of
// one cue are joined with a space, because a search is over a sentence and
// WebVTT breaks a sentence wherever it fits on screen.
func ParseVTT(r io.Reader) []Cue {
	var out []Cue
	var cur *Cue
	var text []string
	flush := func() {
		if cur != nil {
			if s := strings.TrimSpace(strings.Join(text, " ")); s != "" {
				cur.Text = s
				out = append(out, *cur)
			}
		}
		cur, text = nil, nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // one line, generously
	skipping := false                          // inside a block that is not a cue
	for sc.Scan() {
		line := strings.TrimRight(strings.TrimPrefix(sc.Text(), "\ufeff"), "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			skipping = false
			continue
		}
		if skipping {
			continue
		}
		if m := cueTiming.FindStringSubmatch(line); m != nil {
			flush()
			start, okStart := vttSeconds(m[1])
			end, okEnd := vttSeconds(m[2])
			if okStart && okEnd {
				cur = &Cue{Start: start, End: end}
			}
			continue
		}
		if cur == nil {
			// Before any timing line: the header, a block that is not a cue,
			// or the cue's own identifier — none of them dialogue.
			switch {
			case strings.HasPrefix(line, "WEBVTT"), strings.HasPrefix(line, "NOTE"),
				strings.HasPrefix(line, "STYLE"), strings.HasPrefix(line, "REGION"):
				skipping = true
			}
			continue
		}
		if s := strings.TrimSpace(vttTag.ReplaceAllString(line, "")); s != "" {
			text = append(text, s)
		}
	}
	flush()
	return out
}

// vttSeconds reads one WebVTT timestamp — "mm:ss.mmm" or "hh:mm:ss.mmm",
// with a comma for the decimal point where an SRT-minded writer used one.
func vttSeconds(s string) (float64, bool) {
	parts := strings.Split(strings.Replace(s, ",", ".", 1), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0.0
	for _, p := range parts[:len(parts)-1] {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + float64(n)
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, false
	}
	return total*60 + sec, true
}
