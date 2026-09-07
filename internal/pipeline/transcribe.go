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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// a library of files in no particular language wants.
func WhisperArgs(model, wav, language, outPrefix string) []string {
	if language == "" {
		language = "auto"
	}
	return []string{
		"-m", model,
		"-f", wav,
		"-l", language,
		"-ovtt",
		"-of", outPrefix,
	}
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
	out, err := t.run(ctx, t.Bin, WhisperArgs(t.Model, wav, t.Language, prefix))
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
