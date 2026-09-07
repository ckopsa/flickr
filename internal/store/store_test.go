package store

import (
	"path/filepath"
	"testing"
	"time"

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

// A file arrives once. The scan hands the object's own LastModified to the
// insert; a re-probe of the same object — a new etag, a new probe version, a
// new media_info — writes everything about the row EXCEPT when it got here.
func TestAddedAtIsStampedOnce(t *testing.T) {
	l := openTestLibrary(t)
	arrived := time.Date(2019, time.March, 4, 9, 30, 0, 0, time.UTC)
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "a.mkv", ETag: "e1", ProbeVersion: 4, AddedAt: arrived},
		{ObjectKey: "b.mkv", ETag: "e2", ProbeVersion: 4}, // the bucket said nothing
	}); err != nil {
		t.Fatal(err)
	}
	before, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	if !before[0].AddedAt.Equal(arrived) {
		t.Errorf("a.mkv added at %v, want %v", before[0].AddedAt, arrived)
	}
	if before[1].AddedAt.IsZero() {
		t.Error("b.mkv has no arrival time; an insert with none arrives now")
	}
	// Re-probe: same objects, new etags, and no arrival time offered.
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "a.mkv", ETag: "e9", ProbeVersion: 5,
			MediaInfo: &model.MediaInfo{Container: "mkv", VideoCodec: "h264"}},
		{ObjectKey: "b.mkv", ETag: "e8", ProbeVersion: 5, AddedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	for i := range after {
		if !after[i].AddedAt.Equal(before[i].AddedAt) {
			t.Errorf("%s arrived again: %v, was %v",
				after[i].ObjectKey, after[i].AddedAt, before[i].AddedAt)
		}
	}
	if after[0].ETag != "e9" {
		t.Errorf("the re-probe wrote nothing else either: etag %q", after[0].ETag)
	}
}

// The migration: a library written before there was an added_at column has
// no arrival times to recover — the bucket's LastModified was never kept —
// so the whole of it arrives at the moment of the migration, once.
func TestAddedAtMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	old, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE items (
			id INTEGER PRIMARY KEY,
			object_key TEXT NOT NULL UNIQUE,
			etag TEXT NOT NULL,
			size INTEGER NOT NULL,
			media_info TEXT,
			identity TEXT,
			identity_overridden INTEGER NOT NULL DEFAULT 0,
			probe_error TEXT NOT NULL DEFAULT '',
			probe_version INTEGER NOT NULL DEFAULT 0,
			identity_version INTEGER NOT NULL DEFAULT 0,
			enrichment TEXT,
			enrichment_identity TEXT,
			sidecar_sig TEXT NOT NULL DEFAULT '',
			updated_at REAL NOT NULL
		);
		INSERT INTO items (object_key, etag, size, updated_at) VALUES ('old.mkv', 'e1', 1, 0)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	before := time.Now()
	l, err := OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	items, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("migrated %d items, want 1", len(items))
	}
	if at := items[0].AddedAt; at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("old.mkv arrived at %v, want the migration time (~%v)", at, before)
	}
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

// A text place rides the row as a locator; a clock write clears it; a
// database from before the column migrates and reads NULL locators as nil.
func TestStatePlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	// Pre-locator schema, with a row, as an older server left it.
	old, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE playback_state (
		item_id INTEGER NOT NULL, client_id TEXT NOT NULL,
		position_seconds REAL NOT NULL, updated_at REAL NOT NULL,
		PRIMARY KEY (item_id, client_id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO playback_state VALUES (1, 'kid', 42, 1000)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPosition(1, "kid")
	if err != nil || p.PositionSeconds != 42 || p.Locator != nil {
		t.Fatalf("migrated row: %+v, %v", p, err)
	}
	if p, err := s.GetPosition(9, "kid"); err != nil || p.PositionSeconds != 0 || p.Locator != nil {
		t.Fatalf("absent row: %+v, %v", p, err)
	}

	loc := &model.Locator{CFI: "epubcfi(/6/14!/4/2/1:0)", Section: 7, Fraction: 0.34}
	if err := s.SetPlace(2, "kid", 0, loc); err != nil {
		t.Fatal(err)
	}
	p, err = s.GetPosition(2, "kid")
	if err != nil || p.Locator == nil || *p.Locator != *loc || p.PositionSeconds != 0 {
		t.Fatalf("placed row: %+v, %v", p, err)
	}
	all, err := s.PositionsFor("kid")
	if err != nil || len(all) != 2 {
		t.Fatalf("PositionsFor: %v, %v", all, err)
	}
	for _, q := range all {
		if (q.ItemID == 2) != (q.Locator != nil) {
			t.Errorf("locator on the wrong row: %+v", q)
		}
	}
	// A later clock write on the same row clears the place.
	if err := s.SetPosition(2, "kid", 12); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPosition(2, "kid"); p.Locator != nil || p.PositionSeconds != 12 {
		t.Errorf("after clock write: %+v", p)
	}
	// ClearPlace drops the row itself — forgetting the place, not moving it
	// to zero — and only for the profile that asked.
	if err := s.SetPosition(2, "adult", 99); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPlace(2, "kid"); err != nil {
		t.Fatal(err)
	}
	if all, err := s.PositionsFor("kid"); err != nil || len(all) != 1 || all[0].ItemID != 1 {
		t.Errorf("after ClearPlace: %v, %v", all, err)
	}
	if p, _ := s.GetPosition(2, "adult"); p.PositionSeconds != 99 {
		t.Errorf("another profile's place went with it: %+v", p)
	}
	// Clearing a row that is not there is not an error: the place is gone
	// either way, which is what the caller asked for.
	if err := s.ClearPlace(2, "kid"); err != nil {
		t.Errorf("clearing an absent row: %v", err)
	}
}

// My List, as the store keeps it: a set per profile, read back newest first,
// idempotent at both ends, and nobody else's.
func TestSavedList(t *testing.T) {
	s, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	keys := func(rows []Saved) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.WorkKey)
		}
		return out
	}
	if rows, err := s.SavedFor("chris"); err != nil || len(rows) != 0 {
		t.Fatalf("an empty list: %v, %v", rows, err)
	}
	for _, k := range []string{"movie:frozen", "show:the-office", "book:1984"} {
		if err := s.Save("chris", k); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save("kid", "movie:frozen"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.SavedFor("chris")
	if err != nil {
		t.Fatal(err)
	}
	// Newest first; the millisecond clock may tie all three, and then the key
	// orders them — either way the answer is the same twice.
	got := keys(rows)
	if len(got) != 3 {
		t.Fatalf("list = %v, want three entries", got)
	}
	if rows[0].SavedAt == 0 {
		t.Errorf("an entry with no time on it: %+v", rows[0])
	}
	again, err := s.SavedFor("chris")
	if err != nil || len(again) != 3 || keys(again)[0] != got[0] {
		t.Errorf("the list read twice: %v then %v (%v)", got, keys(again), err)
	}
	// Saving what is already there is not a second entry, and leaves the time
	// it was first put by alone.
	first := rows[len(rows)-1]
	if err := s.Save("chris", first.WorkKey); err != nil {
		t.Fatal(err)
	}
	rows = mustSaved(t, s, "chris")
	if len(rows) != 3 {
		t.Fatalf("after re-saving: %v", keys(rows))
	}
	for _, r := range rows {
		if r.WorkKey == first.WorkKey && r.SavedAt != first.SavedAt {
			t.Errorf("re-saving moved the entry: %+v, was %+v", r, first)
		}
	}
	// Taking one off takes one off, and only for the profile that asked.
	if err := s.Unsave("chris", "movie:frozen"); err != nil {
		t.Fatal(err)
	}
	if got := keys(mustSaved(t, s, "chris")); len(got) != 2 {
		t.Errorf("after unsaving: %v", got)
	}
	if got := keys(mustSaved(t, s, "kid")); len(got) != 1 || got[0] != "movie:frozen" {
		t.Errorf("another profile's list went with it: %v", got)
	}
	// Unsaving what is not there is not an error: it is off the list either
	// way, which is what the caller asked for.
	if err := s.Unsave("chris", "movie:frozen"); err != nil {
		t.Errorf("unsaving an absent entry: %v", err)
	}
}

func mustSaved(t *testing.T, s *State, profile string) []Saved {
	t.Helper()
	rows, err := s.SavedFor(profile)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A transcript is written once per file and replaced when the file under it
// changes; deleting the item takes its transcript with it.
func TestTranscriptRoundtrip(t *testing.T) {
	l := openTestLibrary(t)
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "a.mkv", ETag: "e1"},
		{ObjectKey: "b.mkv", ETag: "e2"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	a, b := items[0].ID, items[1].ID

	got, err := l.Transcript(a)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("a file nobody has transcribed has a transcript: %+v", got)
	}

	made := time.Date(2026, time.September, 5, 3, 15, 0, 0, time.UTC)
	for _, tr := range []Transcript{
		{ItemID: a, ETag: "e1", Language: "en", Model: "ggml-base.en.bin", GeneratedAt: made},
		{ItemID: b, ETag: "e2", Language: "de", Model: "ggml-base.en.bin", GeneratedAt: made},
	} {
		if err := l.SetTranscript(tr); err != nil {
			t.Fatal(err)
		}
	}
	got, err = l.Transcript(a)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ETag != "e1" || got.Language != "en" || got.Model != "ggml-base.en.bin" {
		t.Fatalf("transcript = %+v", got)
	}
	if !got.GeneratedAt.Equal(made) {
		t.Errorf("generated at %v, want %v", got.GeneratedAt, made)
	}

	// The file changed under it: the row is replaced, not doubled.
	again := made.Add(24 * time.Hour)
	if err := l.SetTranscript(Transcript{ItemID: a, ETag: "e9", Language: "fr",
		Model: "ggml-medium.bin", GeneratedAt: again}); err != nil {
		t.Fatal(err)
	}
	all, err := l.Transcripts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("%d transcript rows, want 2", len(all))
	}
	if all[a].ETag != "e9" || all[a].Language != "fr" || !all[a].GeneratedAt.Equal(again) {
		t.Errorf("re-transcribed row = %+v", all[a])
	}

	// The object is gone, and so is what was generated from it.
	if _, err := l.DeleteMissing(map[string]bool{"b.mkv": true}); err != nil {
		t.Fatal(err)
	}
	all, err = l.Transcripts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all[a]; ok {
		t.Error("a deleted item kept its transcript row")
	}
	if _, ok := all[b]; !ok {
		t.Error("a surviving item lost its transcript row")
	}
}

// The dialogue: written whole, matched by words, and gone with the file.
func TestCues(t *testing.T) {
	l := openTestLibrary(t)
	if err := l.UpsertBatch([]Item{
		{ObjectKey: "a.mkv", ETag: "e1"},
		{ObjectKey: "b.mkv", ETag: "e2"},
	}); err != nil {
		t.Fatal(err)
	}
	items, err := l.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	a, b := items[0].ID, items[1].ID

	if err := l.SetCues(a, []Cue{
		{ItemID: a, Start: 61, End: 64, Text: "He took the job in New York."},
		{ItemID: a, Start: 300, End: 302.5, Text: "Bears. Beets. Battlestar Galactica."},
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetCues(b, []Cue{
		{ItemID: b, Start: 10, End: 12, Text: "The job is yours."},
	}); err != nil {
		t.Fatal(err)
	}

	got, total, err := l.SearchCues("job", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("\"job\" matched %d lines (%d returned), want 2", total, len(got))
	}
	// A phrase is looked for the way it is said, and the last word is still
	// being typed.
	got, _, err = l.SearchCues("took the jo", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ItemID != a || got[0].Start != 61 || got[0].End != 64 {
		t.Fatalf("the phrase matched %+v", got)
	}
	// One file's own lines, which is what the in-page finder asks for.
	got, total, err = l.SearchCues("job", b, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(got) != 1 || got[0].ItemID != b {
		t.Fatalf("one file's lines = %+v (total %d)", got, total)
	}
	// A search says how many it matched even when it answers fewer.
	if got, total, err = l.SearchCues("the", 0, 1); err != nil || total != 2 || len(got) != 1 {
		t.Fatalf("\"the\" answered %d of %d lines, want 1 of 2 (%v)", len(got), total, err)
	}
	// Words nobody said, and a box with nothing but punctuation in it.
	for _, q := range []string{"parachute", "  ", `"`} {
		if got, total, err := l.SearchCues(q, 0, 20); err != nil || total != 0 || got != nil {
			t.Errorf("%q matched %+v (total %d, err %v)", q, got, total, err)
		}
	}

	// Transcribed again: the lines are replaced, not doubled.
	if err := l.SetCues(a, []Cue{{ItemID: a, Start: 5, End: 7, Text: "The job, again."}}); err != nil {
		t.Fatal(err)
	}
	if n, err := l.CueCount(a); err != nil || n != 1 {
		t.Fatalf("re-transcribed file has %d lines, want 1 (%v)", n, err)
	}
	if _, total, err := l.SearchCues("job", 0, 20); err != nil || total != 2 {
		t.Fatalf("\"job\" matched %d lines after a re-transcribe, want 2 (%v)", total, err)
	}

	// The object is gone, and so are the words it said.
	if _, err := l.DeleteMissing(map[string]bool{"b.mkv": true}); err != nil {
		t.Fatal(err)
	}
	if n, err := l.CueCount(a); err != nil || n != 0 {
		t.Fatalf("a deleted item kept %d lines (%v)", n, err)
	}
	if _, total, err := l.SearchCues("job", 0, 20); err != nil || total != 1 {
		t.Fatalf("\"job\" matched %d lines after a deletion, want 1 (%v)", total, err)
	}
}
