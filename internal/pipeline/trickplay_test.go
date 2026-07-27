package pipeline

import (
	"strings"
	"testing"
)

func TestTrickplaySheets(t *testing.T) {
	cases := []struct {
		duration float64
		want     int
	}{
		{121, 1},    // 13 frames -> 1 sheet
		{999, 1},    // 100 frames exactly -> 1 sheet
		{1000.1, 2}, // 101 frames -> 2 sheets
		{7200, 8},   // 2h: 720 frames -> 8 sheets
		{10000, 10}, // 1000 frames -> 10 sheets
		{5, 1},      // degenerate short file still gets one sheet
	}
	for _, c := range cases {
		if got := TrickplaySheets(c.duration); got != c.want {
			t.Errorf("TrickplaySheets(%v) = %d, want %d", c.duration, got, c.want)
		}
	}
}

func TestTrickplayArgs(t *testing.T) {
	args := TrickplayArgs("http://x/in.mkv", 8, "/tp/sheet%d.jpg")
	s := strings.Join(args, " ")
	for _, want := range []string{
		"-i http://x/in.mkv",
		"-vf fps=1/10,scale=320:-2,tile=10x10",
		"-frames:v 8",
		"-start_number 0",
		"/tp/sheet%d.jpg",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
	// Audio/subtitle decode is pure waste for sprite extraction.
	if !strings.Contains(s, "-an") || !strings.Contains(s, "-sn") {
		t.Errorf("trickplay must not decode audio/subtitles: %s", s)
	}
}
