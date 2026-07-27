package main

import (
	"testing"

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
