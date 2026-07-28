package works

import (
	"reflect"
	"strings"
	"testing"

	"flickr/internal/model"
	"flickr/internal/store"
)

func movie(id int64, key, title string, year int) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity: &model.Identity{Kind: "movie", Title: title, Year: year}}
}

func episode(id int64, key, show string, season, ep int) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity: &model.Identity{Kind: "episode", Title: show, Season: season, Episode: ep}}
}

func withDuration(it store.Item, dur float64) store.Item {
	it.MediaInfo = &model.MediaInfo{DurationSeconds: dur}
	return it
}

func withEnrichment(it store.Item, e model.Enrichment) store.Item {
	it.Enrichment = &e
	return it
}

func keys(ws []Work) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Key
	}
	return out
}

func TestBuildGrouping(t *testing.T) {
	items := []store.Item{
		movie(1, "Movies/Frozen (2013)/Frozen.mkv", "Frozen", 2013),
		episode(2, "Shows/Ninjago/Season 2/e05.mkv", "Ninjago", 2, 5),
		episode(3, "Shows/Ninjago/Season 1/e01.mkv", "ninjago", 1, 1), // case-insensitive grouping
		{ID: 4, ObjectKey: "misc/holiday_clip.mp4", Identity: &model.Identity{Kind: "unknown", Title: "holiday_clip"}},
		{ID: 5, ObjectKey: "misc/no_identity.mp4"}, // nil identity
	}
	ws := Build(items)
	got := keys(ws)
	// Title order: Frozen, holiday_clip.mp4, Ninjago, no_identity.mp4.
	want := []string{"movie:frozen-2013", "file:4", "show:ninjago", "file:5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	show := ws[2]
	if show.Kind != "show" || !strings.EqualFold(show.Title, "Ninjago") || show.EpisodeCount != 2 || show.ItemCount != 2 {
		t.Errorf("show work: %+v", show)
	}
	// Episodes sorted by season/episode: S01E01 before S02E05.
	if show.Items[0].ID != 3 || show.Items[1].ID != 2 {
		t.Errorf("episode order: %d, %d", show.Items[0].ID, show.Items[1].ID)
	}
	// No poster anywhere: representative is the lowest S/E episode.
	if show.RepresentativeItemID != 3 {
		t.Errorf("show representative = %d, want 3", show.RepresentativeItemID)
	}

	mov := ws[0]
	if mov.Kind != "movie" || mov.Year != 2013 || mov.EpisodeCount != 0 || mov.ItemCount != 1 || mov.RepresentativeItemID != 1 {
		t.Errorf("movie work: %+v", mov)
	}
	if mov.Genres == nil {
		t.Error("genres must serialize as [], not null")
	}

	file := ws[1]
	if file.Kind != "file" || file.Title != "holiday_clip.mp4" || file.ItemCount != 1 {
		t.Errorf("file work: %+v", file)
	}
}

func TestBuildTMDBKeys(t *testing.T) {
	items := []store.Item{
		withEnrichment(movie(1, "m/Frozen.mkv", "Frozen", 2013),
			model.Enrichment{TMDBID: 109445, Title: "Frozen", Year: 2013, Overview: "ice", Genres: []string{"Animation"}, HasPoster: true}),
		// Only ONE episode enriched — it still names the whole show, and the
		// top-level enrichment fields (the show's) become the work's.
		episode(2, "s/Ninjago/S1/e01.mkv", "Ninjago", 1, 1),
		withEnrichment(episode(3, "s/Ninjago/S1/e02.mkv", "Ninjago", 1, 2),
			model.Enrichment{TMDBID: 40075, Title: "Ninjago: Masters of Spinjitzu", Overview: "lego", HasPoster: true}),
	}
	ws := Build(items)
	if len(ws) != 2 {
		t.Fatalf("works = %v", keys(ws))
	}
	m, s := ws[0], ws[1]
	if m.Key != "tmdb:109445" || m.Overview != "ice" || !reflect.DeepEqual(m.Genres, []string{"Animation"}) {
		t.Errorf("movie: %+v", m)
	}
	if s.Key != "tmdb:40075" || s.Title != "Ninjago: Masters of Spinjitzu" || s.Overview != "lego" {
		t.Errorf("show: %+v", s)
	}
	// Representative: first enriched member WITH a poster, not the lowest episode.
	if s.RepresentativeItemID != 3 {
		t.Errorf("show representative = %d, want enriched episode 3", s.RepresentativeItemID)
	}
}

func TestBuildDuplicateMovieMergesAndCrossKindCollision(t *testing.T) {
	items := []store.Item{
		// Two copies of the same film → same key → one work, two items.
		withEnrichment(movie(1, "m/Frozen.mkv", "Frozen", 2013), model.Enrichment{TMDBID: 7, Title: "Frozen", Year: 2013}),
		withEnrichment(movie(2, "m/Frozen-4k.mkv", "Frozen", 2013), model.Enrichment{TMDBID: 7, Title: "Frozen", Year: 2013}),
		// A show whose TMDB id numerically collides with the movie's: the
		// later work is demoted to its slug key instead of clobbering.
		withEnrichment(episode(3, "s/Collide/e1.mkv", "Collide", 1, 1), model.Enrichment{TMDBID: 7, Title: "Collide"}),
	}
	ws := Build(items)
	got := keys(ws)
	want := []string{"show:collide", "tmdb:7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if ws[1].ItemCount != 2 || ws[1].Kind != "movie" {
		t.Errorf("merged movie: %+v", ws[1])
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Frozen", "frozen"},
		{"Mister Rogers' Neighborhood", "mister-rogers-neighborhood"},
		{"  What's Up, Doc?  ", "what-s-up-doc"},
		{"WALL·E", "wall-e"},
	} {
		if got := slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestByKey(t *testing.T) {
	ws := Build([]store.Item{movie(1, "m/a.mkv", "A", 0)})
	if ByKey(ws)["movie:a"] == nil {
		t.Fatalf("lookup failed: %v", keys(ws))
	}
}

func pos(itemID int64, sec, updated float64) store.Position {
	return store.Position{ItemID: itemID, PositionSeconds: sec, UpdatedAt: updated}
}

func TestWorkProgressMovie(t *testing.T) {
	ws := Build([]store.Item{withDuration(movie(1, "m/f.mkv", "Frozen", 2013), 6000)})
	w := &ws[0]

	p, ok := WorkProgress(w, map[int64]store.Position{1: pos(1, 2592, 100)})
	if !ok || p.Status != "active" || p.ProgressText != "43:12" || p.Fraction != 2592.0/6000 || p.UpdatedAt != 100 {
		t.Errorf("mid-movie: %+v ok=%v", p, ok)
	}

	p, _ = WorkProgress(w, map[int64]store.Position{1: pos(1, 5400, 101)}) // exactly 90%
	if p.Status != "finished" || p.ProgressText != "1:30:00" {
		t.Errorf("finished movie: %+v", p)
	}

	if _, ok := WorkProgress(w, map[int64]store.Position{}); ok {
		t.Error("no positions must yield ok=false")
	}
}

func TestWorkProgressShow(t *testing.T) {
	// Four 1000s episodes: S01E01, S01E02, S02E05 (furthest), S03E01.
	ws := Build([]store.Item{
		withDuration(episode(1, "s/n/e1.mkv", "N", 1, 1), 1000),
		withDuration(episode(2, "s/n/e2.mkv", "N", 1, 2), 1000),
		withDuration(episode(3, "s/n/e3.mkv", "N", 2, 5), 1000),
		withDuration(episode(4, "s/n/e4.mkv", "N", 3, 1), 1000),
	})
	w := &ws[0]

	// E1 watched (950≥900), E2 untouched, S02E05 partial at 750s.
	p, ok := WorkProgress(w, map[int64]store.Position{
		1: pos(1, 950, 10),
		3: pos(3, 750, 30),
	})
	if !ok {
		t.Fatal("expected progress")
	}
	want := 1.0/4 + (750.0/1000)/4 // one watched + furthest partial
	if p.Fraction != want || p.Status != "active" || p.UpdatedAt != 30 {
		t.Errorf("show progress: %+v, want fraction %v", p, want)
	}
	if p.ProgressText != "S02E05 · 12:30" {
		t.Errorf("progress_text = %q", p.ProgressText)
	}

	// Furthest episode itself watched: no partial double-count.
	p, _ = WorkProgress(w, map[int64]store.Position{3: pos(3, 999, 5)})
	if p.Fraction != 0.25 || p.ProgressText != "S02E05 · 16:39" {
		t.Errorf("watched-furthest: %+v", p)
	}

	// All four watched → finished.
	p, _ = WorkProgress(w, map[int64]store.Position{
		1: pos(1, 950, 1), 2: pos(2, 950, 2), 3: pos(3, 950, 3), 4: pos(4, 950, 4),
	})
	if p.Fraction != 1 || p.Status != "finished" {
		t.Errorf("all watched: %+v", p)
	}
}

func TestContinueList(t *testing.T) {
	frozen := withDuration(movie(1, "m/Frozen.mkv", "Frozen", 2013), 6000)
	e1 := withDuration(episode(2, "s/n/e1.mkv", "Ninjago", 1, 1), 1000)
	e2 := withEnrichment(withDuration(episode(3, "s/n/e2.mkv", "Ninjago", 1, 2), 1000),
		model.Enrichment{TMDBID: 40075, Title: "Ninjago", EpisodeTitle: "Home"})
	done := withDuration(movie(4, "m/Done.mkv", "Done", 0), 1000)
	blip := withDuration(movie(5, "m/Blip.mkv", "Blip", 0), 1000)
	ws := Build([]store.Item{frozen, e1, e2, done, blip})

	entries := ContinueList(ws, []store.Position{
		pos(1, 2592, 100),
		pos(2, 300, 50),  // older episode of the show
		pos(3, 120, 200), // most recent episode → the show's single entry
		pos(4, 950, 300), // ≥90% → finished, excluded
		pos(5, 3, 400),   // <5s → noise, excluded
		pos(99, 60, 500), // deleted item → skipped
	}, 20)

	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	// Most-recent first: the show's e2 (200), then Frozen (100).
	if entries[0].ItemID != 3 || entries[0].WorkKey != "tmdb:40075" || entries[0].Title != "Ninjago" {
		t.Errorf("entry 0: %+v", entries[0])
	}
	if entries[0].Label != "S01E02 · Home" {
		t.Errorf("episode label = %q", entries[0].Label)
	}
	if entries[1].ItemID != 1 || entries[1].Label != "Frozen.mkv" || entries[1].DurationSeconds != 6000 {
		t.Errorf("entry 1: %+v", entries[1])
	}

	if got := ContinueList(ws, []store.Position{pos(1, 100, 1), pos(4, 100, 2)}, 1); len(got) != 1 || got[0].ItemID != 4 {
		t.Errorf("max cap: %+v", got)
	}
}

// A finished episode (≥90%) advances the show's entry to the next episode
// ("up next") instead of dropping the show from the list.
func TestContinueListUpNext(t *testing.T) {
	e1 := withDuration(episode(1, "s/n/e1.mkv", "Ninjago", 1, 1), 1000)
	e2 := withEnrichment(withDuration(episode(2, "s/n/e2.mkv", "Ninjago", 1, 2), 1000),
		model.Enrichment{TMDBID: 40075, Title: "Ninjago", EpisodeTitle: "Home"})
	e3 := withDuration(episode(3, "s/n/e3.mkv", "Ninjago", 1, 3), 1000)
	done := withDuration(movie(4, "m/Done.mkv", "Done", 0), 1000)
	ws := Build([]store.Item{e1, e2, e3, done})

	for _, tc := range []struct {
		name      string
		positions []store.Position
		wantLen   int
		wantItem  int64
		wantPos   float64
		wantLabel string
	}{
		{"mid-episode unchanged",
			[]store.Position{pos(1, 300, 10)}, 1, 1, 300, "S01E01 · e1.mkv"},
		{"finished mid-season advances to next at 0",
			[]store.Position{pos(1, 950, 10)}, 1, 2, 0, "S01E02 · Home"},
		{"finished finale excluded",
			[]store.Position{pos(3, 950, 10)}, 0, 0, 0, ""},
		{"finished movie still excluded",
			[]store.Position{pos(4, 950, 10)}, 0, 0, 0, ""},
		{"next's own existing progress wins",
			[]store.Position{pos(1, 950, 20), pos(2, 300, 5)}, 1, 2, 300, "S01E02 · Home"},
		{"next's finished progress does not resurrect: start at 0",
			[]store.Position{pos(1, 950, 20), pos(2, 950, 5)}, 1, 2, 0, "S01E02 · Home"},
	} {
		got := ContinueList(ws, tc.positions, 20)
		if len(got) != tc.wantLen {
			t.Errorf("%s: got %d entries: %+v", tc.name, len(got), got)
			continue
		}
		if tc.wantLen == 0 {
			continue
		}
		e := got[0]
		if e.ItemID != tc.wantItem || e.PositionSeconds != tc.wantPos || e.Label != tc.wantLabel {
			t.Errorf("%s: entry %+v, want item %d pos %v label %q", tc.name, e, tc.wantItem, tc.wantPos, tc.wantLabel)
		}
		// The advanced entry keeps the show's work key and duration of the entry item.
		if e.WorkKey != "tmdb:40075" || e.DurationSeconds != 1000 {
			t.Errorf("%s: work key/duration: %+v", tc.name, e)
		}
	}

	// Recency anchor: the finished episode's UpdatedAt sorts the advanced
	// entry, so a just-finished show outranks an older mid-movie position.
	frozen := withDuration(movie(5, "m/Frozen.mkv", "Frozen", 2013), 6000)
	ws2 := Build([]store.Item{e1, e2, e3, frozen})
	got := ContinueList(ws2, []store.Position{pos(5, 2000, 50), pos(1, 950, 100)}, 20)
	if len(got) != 2 || got[0].ItemID != 2 || got[0].UpdatedAt != 100 || got[1].ItemID != 5 {
		t.Errorf("recency: %+v", got)
	}
}

func TestCursorRoundtrip(t *testing.T) {
	c := Cursor{Lib: 1042, State: 388}
	if c.String() != "l1042.s388" {
		t.Fatalf("String() = %q", c.String())
	}
	back, err := ParseCursor("l1042.s388")
	if err != nil || back != c {
		t.Fatalf("parse: %v %v", back, err)
	}
	for _, bad := range []string{"", "l1042", "1042.388", "l10.s", "lx.s3", "l1.s2.s3", "l-1.s2", "l+1.s2", "s3.l1"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) should fail", bad)
		}
	}
}

func TestFeed(t *testing.T) {
	frozen := withDuration(movie(1, "m/Frozen.mkv", "Frozen", 2013), 6000)
	frozen.Seq = 10
	e1 := withDuration(episode(2, "s/n/e1.mkv", "Ninjago", 1, 1), 1000)
	e1.Seq = 20
	ws := Build([]store.Item{frozen, e1})

	positions := []store.Position{
		{ItemID: 1, ClientID: "kid", PositionSeconds: 3000, UpdatedAt: 100, Seq: 5},
		{ItemID: 1, ClientID: "dad", PositionSeconds: 5900, UpdatedAt: 101, Seq: 6},
	}

	// No cursor → everything, audiences included.
	all := Feed(ws, positions, nil, 0)
	if len(all) != 2 {
		t.Fatalf("full feed: %d works", len(all))
	}
	fz := all[0]
	if fz.Key != "movie:frozen-2013" || len(fz.Audiences) != 2 {
		t.Fatalf("frozen feed work: %+v", fz)
	}
	// Sorted by name: dad (finished at 5900/6000), then kid (active).
	if fz.Audiences[0].Name != "dad" || fz.Audiences[0].Status != "finished" ||
		fz.Audiences[1].Name != "kid" || fz.Audiences[1].Status != "active" || fz.Audiences[1].ProgressText != "50:00" {
		t.Errorf("audiences: %+v", fz.Audiences)
	}
	if len(all[1].Audiences) != 0 {
		t.Errorf("ninjago audiences should be empty, got %+v", all[1].Audiences)
	}

	// Cursor beyond all seqs → nothing changed.
	if got := Feed(ws, positions, &Cursor{Lib: 20, State: 6}, 0); len(got) != 0 {
		t.Errorf("quiet feed: %+v", got)
	}
	// Library change only: e1.Seq(20) > 15 → just the show.
	if got := Feed(ws, positions, &Cursor{Lib: 15, State: 6}, 0); len(got) != 1 || got[0].Kind != "show" {
		t.Errorf("lib-change feed: %+v", got)
	}
	// Playback change only: seq 6 > 5 → just Frozen.
	if got := Feed(ws, positions, &Cursor{Lib: 20, State: 5}, 0); len(got) != 1 || got[0].Key != "movie:frozen-2013" {
		t.Errorf("state-change feed: %+v", got)
	}
	// A deletion after the cursor forces a full resync.
	if got := Feed(ws, positions, &Cursor{Lib: 20, State: 6}, 21); len(got) != 2 {
		t.Errorf("deletion resync: %d works", len(got))
	}
}
