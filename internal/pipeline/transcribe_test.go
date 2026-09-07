package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscribeAudioArgs(t *testing.T) {
	s := strings.Join(TranscribeAudioArgs("http://x/in.mkv", "/t/audio.wav"), " ")
	for _, want := range []string{
		"-i http://x/in.mkv",
		"-ac 1",                 // whisper.cpp reads mono
		"-ar 16000",             // at 16 kHz
		"-c:a pcm_s16le -f wav", // as signed-16-bit PCM, nothing else
		"/t/audio.wav",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	// The picture is the expensive half of the decode and yields no cue.
	for _, none := range []string{"-vn", "-sn", "-dn"} {
		if !strings.Contains(s, none) {
			t.Errorf("audio extraction must not decode %s: %s", none, s)
		}
	}
}

func TestWhisperArgs(t *testing.T) {
	cases := []struct {
		name     string
		language string
		want     string
	}{
		{"an empty language is auto-detected", "", "-l auto"},
		{"auto stays auto", "auto", "-l auto"},
		{"a named language is passed through", "de", "-l de"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := strings.Join(WhisperArgs("/m/ggml-base.bin", "/t/audio.wav", c.language, "/t/transcript"), " ")
			for _, want := range []string{
				"-m /m/ggml-base.bin",
				"-f /t/audio.wav",
				c.want,
				"-ovtt",             // WebVTT, the one format the .vtt route serves
				"-of /t/transcript", // ... written as <prefix>.vtt
			} {
				if !strings.Contains(s, want) {
					t.Errorf("missing %q in: %s", want, s)
				}
			}
		})
	}
}

func TestDetectedLanguage(t *testing.T) {
	cases := []struct{ out, want string }{
		{"whisper_full_with_state: auto-detected language: en (p = 0.98)\n", "en"},
		{"auto-detected language: de (p = 0.71)", "de"},
		{"main: processing 'audio.wav' (1 samples)\n", ""},
	}
	for _, c := range cases {
		if got := DetectedLanguage([]byte(c.out)); got != c.want {
			t.Errorf("DetectedLanguage(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// The whole flow over the exec seam: two commands in order, the wav handed
// from the first to the second, and only a complete transcript at destPath.
func TestTranscribeRunsBothCommands(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "7", "transcript.vtt")
	var ran []string
	tr := &Transcriber{
		Bin: "/opt/whisper-cli", Model: "/m/ggml-base.bin",
		Run: func(_ context.Context, name string, args []string) ([]byte, error) {
			ran = append(ran, name)
			switch name {
			case "ffmpeg":
				return nil, os.WriteFile(args[len(args)-1], []byte("RIFF"), 0o644)
			default:
				prefix := args[len(args)-1]
				if err := os.WriteFile(prefix+".vtt", []byte("WEBVTT\n"), 0o644); err != nil {
					return nil, err
				}
				return []byte("auto-detected language: fr (p = 0.9)\n"), nil
			}
		},
	}
	lang, err := tr.Transcribe(context.Background(), "http://x/in.mkv", dest)
	if err != nil {
		t.Fatal(err)
	}
	if lang != "fr" {
		t.Errorf("language = %q, want fr", lang)
	}
	if want := []string{"ffmpeg", "/opt/whisper-cli"}; strings.Join(ran, ",") != strings.Join(want, ",") {
		t.Errorf("ran %v, want %v", ran, want)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("no transcript at %s: %v", dest, err)
	}
	if string(b) != "WEBVTT\n" {
		t.Errorf("transcript = %q", b)
	}
	// The scratch directory is gone; only the transcript survives.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left %d files beside the transcript, want none", len(entries)-1)
	}
}

// A whisper that fails writes nothing: the next pass must be free to try
// again rather than find a half-written file cached forever.
func TestTranscribeLeavesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "transcript.vtt")
	tr := &Transcriber{
		Bin: "whisper-cli", Model: "m.bin",
		Run: func(_ context.Context, name string, args []string) ([]byte, error) {
			if name == "ffmpeg" {
				return nil, os.WriteFile(args[len(args)-1], []byte("RIFF"), 0o644)
			}
			return []byte("out of memory"), errors.New("exit status 1")
		},
	}
	if _, err := tr.Transcribe(context.Background(), "http://x/in.mkv", dest); err == nil {
		t.Fatal("want an error from a failing whisper")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("a failed run left a transcript at %s", dest)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a failed run left %d files behind", len(entries))
	}
}
