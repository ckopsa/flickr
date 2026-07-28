package store

import (
	"path/filepath"
	"testing"

	"flickr/internal/model"
)

func openTestLibrary(t *testing.T) *Library {
	t.Helper()
	l, err := OpenLibrary(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSidecarSigRoundtrip(t *testing.T) {
	l := openTestLibrary(t)
	items := []Item{
		{ObjectKey: "a.mkv", ETag: "e1", Size: 1, ProbeVersion: 5, SidecarSig: "a.srt=x1",
			MediaInfo: &model.MediaInfo{Container: "mkv", VideoCodec: "h264"}},
		{ObjectKey: "b.mkv", ETag: "e2", Size: 2, ProbeVersion: 5}, // no sidecars
	}
	if err := l.UpsertBatch(items); err != nil {
		t.Fatal(err)
	}
	known, err := l.Known()
	if err != nil {
		t.Fatal(err)
	}
	if known["a.mkv"].SidecarSig != "a.srt=x1" {
		t.Errorf("a.mkv sidecar sig = %q", known["a.mkv"].SidecarSig)
	}
	if known["b.mkv"].SidecarSig != "" {
		t.Errorf("b.mkv sidecar sig = %q, want empty", known["b.mkv"].SidecarSig)
	}
}

func TestUpdateSidecars(t *testing.T) {
	l := openTestLibrary(t)
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "a.mkv", ETag: "e1", ProbeVersion: 5,
			MediaInfo: &model.MediaInfo{Container: "mkv", VideoCodec: "h264"}},
		{ObjectKey: "broken.mkv", ETag: "e2", ProbeVersion: 5, ProbeError: "probe failed"},
	}); err != nil {
		t.Fatal(err)
	}
	ups := []SidecarUpdate{
		{ObjectKey: "a.mkv", SidecarSig: "a.en.srt=x2", Attach: func(mi *model.MediaInfo) {
			mi.Subtitles = append(mi.Subtitles, model.SubtitleTrack{
				Ordinal: 0, Codec: "subrip", External: true, ObjectKey: "a.en.srt", Supported: true,
			})
		}},
		// Probe-error row: sig must update (so scans stop re-flagging it)
		// even though there is no media_info to rewrite.
		{ObjectKey: "broken.mkv", SidecarSig: "broken.srt=x3", Attach: func(*model.MediaInfo) {}},
		// Row deleted between listing and update: silently skipped.
		{ObjectKey: "gone.mkv", SidecarSig: "x", Attach: func(*model.MediaInfo) {}},
	}
	if err := l.UpdateSidecars(ups); err != nil {
		t.Fatal(err)
	}
	items, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	var a *Item
	for i := range items {
		if items[i].ObjectKey == "a.mkv" {
			a = &items[i]
		}
	}
	if a == nil || a.MediaInfo == nil || len(a.MediaInfo.Subtitles) != 1 || !a.MediaInfo.Subtitles[0].External {
		t.Fatalf("a.mkv media_info after update: %+v", a)
	}
	known, err := l.Known()
	if err != nil {
		t.Fatal(err)
	}
	if known["a.mkv"].SidecarSig != "a.en.srt=x2" || known["broken.mkv"].SidecarSig != "broken.srt=x3" {
		t.Errorf("sigs after update: %+v", known)
	}
}

// TestChangeSeq: every write path stamps rows from one monotonic counter,
// FeedSeq tracks the counter, and DeleteMissing records a deletion mark
// instead of per-row tombstones.
func TestChangeSeq(t *testing.T) {
	l := openTestLibrary(t)
	if s, err := l.FeedSeq(); err != nil || s != 0 {
		t.Fatalf("fresh FeedSeq = %d, %v", s, err)
	}
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "a.mkv", ETag: "1"},
		{ObjectKey: "b.mkv", ETag: "1"},
	}); err != nil {
		t.Fatal(err)
	}
	seqs := func() map[string]int64 {
		items, err := l.ListItems()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, it := range items {
			out[it.ObjectKey] = it.Seq
		}
		return out
	}
	s0 := seqs()
	if s0["a.mkv"] != 1 || s0["b.mkv"] != 2 {
		t.Fatalf("after upsert: %v", s0)
	}

	// Each write path bumps only its row, always increasing.
	if err := l.UpdateIdentities([]IdentityUpdate{{ObjectKey: "a.mkv", Identity: model.Identity{Kind: "movie", Title: "A"}}}, 1); err != nil {
		t.Fatal(err)
	}
	if s := seqs(); s["a.mkv"] != 3 || s["b.mkv"] != 2 {
		t.Fatalf("after identity refresh: %v", s)
	}
	items, _ := l.ListItems()
	var aID int64
	for _, it := range items {
		if it.ObjectKey == "a.mkv" {
			aID = it.ID
		}
	}
	if err := l.SetEnrichment(aID, &model.Enrichment{TMDBID: 9}, &model.Identity{Kind: "movie", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := l.UpdateSidecars([]SidecarUpdate{{ObjectKey: "b.mkv", SidecarSig: "x", Attach: func(*model.MediaInfo) {}}}); err != nil {
		t.Fatal(err)
	}
	if err := l.OverrideIdentity(aID, model.Identity{Kind: "movie", Title: "A!"}); err != nil {
		t.Fatal(err)
	}
	if s := seqs(); s["a.mkv"] != 6 || s["b.mkv"] != 5 {
		t.Fatalf("after enrich+sidecar+override: %v", s)
	}
	if fs, _ := l.FeedSeq(); fs != 6 {
		t.Fatalf("FeedSeq = %d, want 6", fs)
	}

	// Deletion: no tombstone rows, just the resync mark.
	if ds, _ := l.DeletionSeq(); ds != 0 {
		t.Fatalf("premature DeletionSeq %d", ds)
	}
	if n, err := l.DeleteMissing(map[string]bool{"a.mkv": true}); err != nil || n != 1 {
		t.Fatalf("DeleteMissing: %d, %v", n, err)
	}
	ds, _ := l.DeletionSeq()
	fs, _ := l.FeedSeq()
	if ds != 7 || fs != 7 {
		t.Fatalf("after delete: deletion=%d feed=%d", ds, fs)
	}
	// A delete that removes nothing must not move the mark.
	if _, err := l.DeleteMissing(map[string]bool{"a.mkv": true}); err != nil {
		t.Fatal(err)
	}
	if ds2, _ := l.DeletionSeq(); ds2 != 7 {
		t.Fatalf("no-op delete moved mark to %d", ds2)
	}
}

func TestStatePositionsAndSeq(t *testing.T) {
	s, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPosition(1, "kid", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPosition(2, "kid", 50); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPosition(1, "dad", 900); err != nil {
		t.Fatal(err)
	}
	// Rewriting a row re-stamps its seq from the shared counter.
	if err := s.SetPosition(1, "kid", 200); err != nil {
		t.Fatal(err)
	}

	kid, err := s.PositionsFor("kid")
	if err != nil || len(kid) != 2 {
		t.Fatalf("PositionsFor: %v, %v", kid, err)
	}
	bySeq := map[int64]int64{}
	for _, p := range kid {
		bySeq[p.ItemID] = p.Seq
		if p.ClientID != "kid" || p.UpdatedAt == 0 {
			t.Errorf("row: %+v", p)
		}
	}
	if bySeq[1] != 4 || bySeq[2] != 2 {
		t.Errorf("kid seqs: %v", bySeq)
	}

	all, err := s.AllPositions()
	if err != nil || len(all) != 3 {
		t.Fatalf("AllPositions: %v, %v", all, err)
	}
	if fs, _ := s.FeedSeq(); fs != 4 {
		t.Errorf("state FeedSeq = %d, want 4", fs)
	}
}

// TestNeedingEnrichmentVersion: rows enriched before the "v" marker (or with
// an older one) are re-enriched; current-version rows with matching identity
// are not. Also exercises json_extract, pinning that the sqlite build ships
// JSON1.
func TestNeedingEnrichmentVersion(t *testing.T) {
	l := openTestLibrary(t)
	ident := model.Identity{Kind: "movie", Title: "Frozen", Year: 2013}
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "old.mkv", ETag: "1", Identity: &ident},
		{ObjectKey: "new.mkv", ETag: "2", Identity: &ident},
		{ObjectKey: "never.mkv", ETag: "3", Identity: &ident},
	}); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int64{}
	items, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		byKey[it.ObjectKey] = it.ID
	}
	// old.mkv: pre-versioning enrichment (no "v" field in the JSON).
	if err := l.SetEnrichment(byKey["old.mkv"], &model.Enrichment{TMDBID: 1, Title: "Frozen"}, &ident); err != nil {
		t.Fatal(err)
	}
	// new.mkv: current-version enrichment.
	if err := l.SetEnrichment(byKey["new.mkv"], &model.Enrichment{Version: 2, TMDBID: 1, Title: "Frozen"}, &ident); err != nil {
		t.Fatal(err)
	}

	need, err := l.NeedingEnrichment(2)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range need {
		got[it.ObjectKey] = true
	}
	if !got["old.mkv"] || !got["never.mkv"] || got["new.mkv"] {
		t.Errorf("needing enrichment: %v (want old.mkv + never.mkv only)", got)
	}
}
