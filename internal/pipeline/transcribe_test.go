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

// The cues, read back out of the WebVTT: one table over everything a
// generated transcript can hold.
func TestParseVTT(t *testing.T) {
	cases := []struct {
		name string
		vtt  string
		want []Cue
	}{
		{
			name: "what whisper writes",
			vtt: "WEBVTT\n\n" +
				"00:00:01.000 --> 00:00:03.500\n" +
				"He took the job in New York.\n\n" +
				"00:00:03.500 --> 00:00:06.000\n" +
				"And he never said a word about it.\n",
			want: []Cue{
				{Start: 1, End: 3.5, Text: "He took the job in New York."},
				{Start: 3.5, End: 6, Text: "And he never said a word about it."},
			},
		},
		{
			name: "an identifier, cue settings, an hour and a wrapped line",
			vtt: "WEBVTT\n\nNOTE this file was generated\n\n" +
				"cue-7\n01:02:03.250 --> 01:02:05.000 align:start position:10%\n" +
				"Let it go,\nlet it go.\n",
			want: []Cue{{Start: 3723.25, End: 3725, Text: "Let it go, let it go."}},
		},
		{
			name: "markup is not a word anybody searched for",
			vtt:  "WEBVTT\n\n00:10.000 --> 00:12.000\n<v Michael>These are <i>my</i> people.\n",
			want: []Cue{{Start: 10, End: 12, Text: "These are my people."}},
		},
		{
			name: "an empty cue is not a line, and neither is a broken timing",
			vtt: "WEBVTT\n\n00:00.000 --> 00:02.000\n[silence]\n\n" +
				"00:02.000 --> 00:04.000\n\n\n" +
				"nonsense --> also nonsense\nnever said\n\n" +
				"00:04,000 --> 00:06,000\nStill here.\n",
			want: []Cue{
				{Start: 0, End: 2, Text: "[silence]"},
				{Start: 4, End: 6, Text: "Still here."},
			},
		},
		{name: "an empty file has no dialogue", vtt: "WEBVTT\n", want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseVTT(strings.NewReader(c.vtt))
			if len(got) != len(c.want) {
				t.Fatalf("%d cues, want %d: %+v", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("cue %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}
