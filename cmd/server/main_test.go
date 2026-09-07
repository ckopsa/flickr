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

// A progress write is a clock position unless it carries a text field; then
// it is a place, its section read off the CFI when the client sent none, and
// out-of-range fractions are refused before they reach the store.
func TestPlaceOf(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name string
		in   progressInput
		want *model.Locator
		bad  bool
	}{
		{"clock position", progressInput{Position: 42}, nil, false},
		{"cfi + fraction + section", progressInput{Locator: "epubcfi(/6/14!/4/2)", Fraction: f(0.34), Section: 7},
			&model.Locator{CFI: "epubcfi(/6/14!/4/2)", Section: 7, Fraction: 0.34}, false},
		{"section derived from the cfi", progressInput{Locator: "epubcfi(/6/14[ch07]!/4/2/1:0)", Fraction: f(0.34)},
			&model.Locator{CFI: "epubcfi(/6/14[ch07]!/4/2/1:0)", Section: 7, Fraction: 0.34}, false},
		{"explicit section wins", progressInput{Locator: "epubcfi(/6/14!/4/2)", Section: 3},
			&model.Locator{CFI: "epubcfi(/6/14!/4/2)", Section: 3}, false},
		{"fraction alone", progressInput{Fraction: f(0)}, &model.Locator{}, false},
		{"not a cfi: stored, no section", progressInput{Locator: "page 12", Fraction: f(0.5)},
			&model.Locator{CFI: "page 12", Fraction: 0.5}, false},
		{"fraction above one", progressInput{Locator: "x", Fraction: f(1.2)}, nil, true},
		{"fraction below zero", progressInput{Locator: "x", Fraction: f(-0.1)}, nil, true},
		{"negative section", progressInput{Locator: "x", Section: -1}, nil, true},
		// A PDF's place: the page and its fraction, no CFI, no section.
		{"page + fraction", progressInput{Page: 213, Fraction: f(0.5325)}, &model.Locator{Page: 213, Fraction: 0.5325}, false},
		{"page alone", progressInput{Page: 1}, &model.Locator{Page: 1}, false},
		{"negative page", progressInput{Page: -3, Fraction: f(0.1)}, nil, true},
	} {
		got, err := placeOf(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%s: want an error, got %+v", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestSectionFromCFI(t *testing.T) {
	for cfi, want := range map[string]int{
		"epubcfi(/6/14!/4/2/1:0)":       7,
		"epubcfi(/6/14[ch07]!/4/2/1:0)": 7,
		"epubcfi(/6/2!/4/2)":            1,
		" epubcfi(/6/40!/4) ":           20,
		"epubcfi(/6/13!/4)":             0, // odd: not an element step
		"epubcfi(/6)":                   0,
		"epubcfi(/6/)":                  0,
		"/6/14!/4":                      0,
		"":                              0,
		"page 12":                       0,
	} {
		if got := model.SectionFromCFI(cfi); got != want {
			t.Errorf("SectionFromCFI(%q) = %d, want %d", cfi, got, want)
		}
	}
}

// The reader is handed a text item's bytes as the type its library expects:
// pdf.js wants application/pdf, epub.js application/epub+zip; the extension
// decides, case-insensitively, the way the scan admitted the file.
func TestBookContentType(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"Books/A/B.epub", "application/epub+zip"},
		{"Books/A/B.pdf", "application/pdf"},
		{"Books/A/B.PDF", "application/pdf"},
	} {
		if got := bookContentType(tc.key); got != tc.want {
			t.Errorf("bookContentType(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
