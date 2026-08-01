package main

import (
	"testing"
	"time"

	"flickr/internal/model"
)

func TestFindSubtitle(t *testing.T) {
	info := &model.MediaInfo{Subtitles: []model.SubtitleTrack{
		{Ordinal: 0, Codec: "subrip", Supported: true},
		{Ordinal: 1, Codec: "hdmv_pgs_subtitle", Supported: false},
	}}
	if tr := findSubtitle(info, 1); tr == nil || tr.Codec != "hdmv_pgs_subtitle" {
		t.Errorf("ordinal 1: %+v", tr)
	}
	if tr := findSubtitle(info, 2); tr != nil {
		t.Errorf("ordinal 2 should be absent, got %+v", tr)
	}
	if tr := findSubtitle(nil, 0); tr != nil {
		t.Errorf("nil media info should yield nil, got %+v", tr)
	}
}

func TestTelemetrySummary(t *testing.T) {
	if s := telemetrySummary(map[string]any{"event": "play_error", "item_id": 3.0}); s != "event=play_error keys=2" {
		t.Errorf("got %q", s)
	}
	if s := telemetrySummary(map[string]any{"b": 1, "a": 2}); s != "keys=a,b" {
		t.Errorf("got %q", s)
	}
}

func TestClockWindow(t *testing.T) {
	if w, err := parseClockWindow(""); w != nil || err != nil {
		t.Errorf("empty input: want nil, nil; got %+v, %v", w, err)
	}
	for _, bad := range []string{"22:00", "2200-0600", "25:00-06:00", "22:00-22:00"} {
		if _, err := parseClockWindow(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}

	at := func(hh, mm int) time.Time {
		return time.Date(2026, 8, 1, hh, mm, 0, 0, time.UTC)
	}

	// Overnight wrap: 22:00-06:00.
	w, err := parseClockWindow("22:00-06:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		hh, mm int
		open   bool
	}{
		{21, 59, false}, {22, 0, true}, {23, 30, true},
		{0, 0, true}, {5, 59, true}, {6, 0, false}, {12, 0, false},
	} {
		if got := w.open(at(tc.hh, tc.mm)); got != tc.open {
			t.Errorf("22:00-06:00 open at %02d:%02d = %v, want %v", tc.hh, tc.mm, got, tc.open)
		}
	}
	if d := w.untilOpen(at(12, 0)); d != 10*time.Hour {
		t.Errorf("untilOpen from 12:00 = %v, want 10h", d)
	}
	if d := w.untilOpen(at(23, 0)); d != 0 {
		t.Errorf("untilOpen while open = %v, want 0", d)
	}

	// Same-day window: 01:00-05:00.
	w, err = parseClockWindow("01:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	if !w.open(at(3, 0)) || w.open(at(0, 30)) || w.open(at(5, 0)) {
		t.Error("01:00-05:00 open/closed edges wrong")
	}
	if d := w.untilOpen(at(6, 0)); d != 19*time.Hour {
		t.Errorf("untilOpen from 06:00 = %v, want 19h", d)
	}
}
