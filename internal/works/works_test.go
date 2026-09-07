package works

import (
	"encoding/json"
	"fmt"
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

func extra(id int64, key, title string, season, ep int) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity: &model.Identity{Kind: "extra", Title: title, Season: season, Episode: ep}}
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

// Bonus material must never earn a work — and so never a tile — of its own:
// it joins the show or film it belongs to, sorted last, counted separately.
func TestBuildAttachesExtras(t *testing.T) {
	items := []store.Item{
		movie(1, "Movies/Frozen (2013)/Frozen.mkv", "Frozen", 2013),
		extra(2, "Movies/Frozen (2013)/Extras/Sing-Along.mkv", "Frozen", 0, 0),
		episode(3, "Shows/The Office/Season 1/S01E01.mkv", "The Office", 1, 1),
		// Carries S01E01 in its name, but it is NOT the pilot.
		extra(4, "Shows/The Office/Featurettes/S01E01 Deleted Scenes.mkv", "the office", 1, 1),
		// Bonus material for a work that isn't in the library: still one work.
		extra(5, "Shows/Lost/Extras/a.mkv", "Lost", 0, 0),
		extra(6, "Shows/Lost/Extras/b.mkv", "Lost", 0, 0),
	}
	ws := Build(items)
	if got, want := keys(ws), []string{"movie:frozen-2013", "show:lost", "show:the-office"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	frozen := ws[0]
	if frozen.ItemCount != 2 || frozen.ExtraCount != 1 || frozen.EpisodeCount != 0 {
		t.Errorf("movie work: %+v", frozen)
	}
	if frozen.Items[0].ID != 1 || frozen.RepresentativeItemID != 1 {
		t.Errorf("the film itself must lead its work: %+v", frozen.Items)
	}

	office := ws[2]
	if office.EpisodeCount != 1 || office.ExtraCount != 1 || office.ItemCount != 2 {
		t.Errorf("show work: %+v", office)
	}
	if office.Items[0].ID != 3 || office.Items[1].ID != 4 {
		t.Errorf("extras sort after episodes, whatever their numbering: %+v", office.Items)
	}

	lost := ws[1]
	if lost.ExtraCount != 2 || lost.EpisodeCount != 0 || lost.ItemCount != 2 {
		t.Errorf("orphan extras group into one work: %+v", lost)
	}
}

// Unnumbered episodes (episode 0 — disc rips, title-named files) sort after
// their season's numbered episodes, by key, so a ripped block stays in order.
func TestBuildOrdersUnnumberedEpisodesLast(t *testing.T) {
	items := []store.Item{
		episode(1, "Shows/MASH/Season 6/B9_t02.mkv", "MASH", 6, 0),
		episode(2, "Shows/MASH/Season 6/S06E01.mkv", "MASH", 6, 1),
		episode(3, "Shows/MASH/Season 6/B9_t01.mkv", "MASH", 6, 0),
		episode(4, "Shows/MASH/Season 7/S07E01.mkv", "MASH", 7, 1),
	}
	got := []int64{}
	for _, it := range Build(items)[0].Items {
		got = append(got, it.ID)
	}
	if want := []int64{2, 3, 1, 4}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v (S06E01, then unnumbered by key, then S07E01)", got, want)
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

// Bonus material is not part of a show's arc: it counts toward no fraction,
// anchors no "furthest episode", and finishing the finale never advances into
// a featurette.
func TestExtrasStayOutOfShowProgress(t *testing.T) {
	ws := Build([]store.Item{
		withDuration(episode(1, "s/o/e1.mkv", "O", 1, 1), 1000),
		withDuration(episode(2, "s/o/e2.mkv", "O", 1, 2), 1000),
		withDuration(extra(3, "s/o/Extras/x.mkv", "O", 1, 2), 1000),
	})
	w := &ws[0]

	// Both episodes watched: finished, even though the featurette is untouched.
	p, ok := WorkProgress(w, map[int64]store.Position{1: pos(1, 950, 1), 2: pos(2, 950, 2)})
	if !ok || p.Fraction != 1 || p.Status != "finished" {
		t.Errorf("episodes only: %+v", p)
	}

	// A watched episode plus a watched extra must not exceed 1 or re-anchor
	// the progress text on the extra.
	p, _ = WorkProgress(w, map[int64]store.Position{2: pos(2, 950, 2), 3: pos(3, 950, 9)})
	if p.Fraction != 0.5 || p.ProgressText != "S01E02 · 15:50" {
		t.Errorf("with extra watched: %+v", p)
	}

	// Only the extra has a position: reported as a plain in-progress item,
	// not as a place in the show.
	p, ok = WorkProgress(w, map[int64]store.Position{3: pos(3, 500, 9)})
	if !ok || p.Status != "active" || p.Fraction != 0.5 || p.ProgressText != "8:20" {
		t.Errorf("extra only: %+v", p)
	}

	// Finishing the finale does not advance into the featurette.
	entries := ContinueList(ws, []store.Position{pos(2, 990, 3)}, 20)
	if len(entries) != 0 {
		t.Errorf("finale finished: %+v, want no resume entry", entries)
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

func part(id int64, key, author, title string, part int) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity: &model.Identity{Kind: "audiobook_part", Author: author, Title: title, Part: part}}
}

func track(id int64, key, artist, album string, n int) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity: &model.Identity{Kind: "track", Author: artist, Title: album, Part: n}}
}

// titledTrack is a track from an identity that knows the track's own name;
// the album's year rides on it the way the path grammar puts it there.
func titledTrack(id int64, key, artist, album string, year, n int, name string) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity: &model.Identity{Kind: "track", Author: artist, Title: album, Year: year, Part: n, TrackTitle: name}}
}

func book(id int64, key, author, title string, year int) store.Item {
	return store.Item{ID: id, ObjectKey: key,
		Identity:  &model.Identity{Kind: "book", Author: author, Title: title, Year: year},
		MediaInfo: &model.MediaInfo{Medium: model.MediumText, Container: "epub", Sections: 12}}
}

// The audio and text kinds project the way shows and movies do: parts and
// tracks group under their (author, title) into one work each, ordered by
// part number; a book is a work of one item. Each work knows its medium and
// author, and the slug key keeps two authors' same-named works apart.
func TestBuildAudioAndTextWorks(t *testing.T) {
	items := []store.Item{
		part(1, "Audiobooks/Frank Herbert/Dune/02.m4b", "Frank Herbert", "Dune", 2),
		part(2, "Audiobooks/Frank Herbert/Dune/03.m4b", "frank herbert", "dune", 3), // case-insensitive grouping
		part(3, "Audiobooks/Frank Herbert/Dune/01.m4b", "Frank Herbert", "Dune", 1),
		track(4, "Music/Radiohead/OK Computer/02 Paranoid Android.flac", "Radiohead", "OK Computer", 2),
		track(5, "Music/Radiohead/OK Computer/01 Airbag.flac", "Radiohead", "OK Computer", 1),
		book(6, "Books/Frank Herbert/Dune (1965).epub", "Frank Herbert", "Dune", 1965),
		// Same title as the audiobook, different author: a different work.
		part(7, "Audiobooks/Someone Else/Dune.m4b", "Someone Else", "Dune", 0),
		// A movie called Dune too: kinds never share a key.
		movie(8, "Movies/Dune (2021)/Dune.mkv", "Dune", 2021),
	}
	ws := Build(items)
	got := keys(ws)
	// Title order, then key: the four works called "Dune" sort by key.
	want := []string{
		"audiobook:frank-herbert-dune", "audiobook:someone-else-dune", "book:frank-herbert-dune-1965",
		"movie:dune-2021", "album:radiohead-ok-computer",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}

	ab := ws[0]
	if ab.Kind != "audiobook" || ab.Medium != "audio" || ab.Author != "Frank Herbert" || ab.Title != "Dune" {
		t.Errorf("audiobook work: %+v", ab)
	}
	if ab.PartCount != 3 || ab.ItemCount != 3 || ab.TrackCount != 0 || ab.EpisodeCount != 0 {
		t.Errorf("audiobook counts: %+v", ab)
	}
	// Parts in part order, whatever the input order; the first part is the
	// representative.
	if ab.Items[0].ID != 3 || ab.Items[1].ID != 1 || ab.Items[2].ID != 2 || ab.RepresentativeItemID != 3 {
		t.Errorf("part order: %d %d %d rep=%d", ab.Items[0].ID, ab.Items[1].ID, ab.Items[2].ID, ab.RepresentativeItemID)
	}

	album := ws[4]
	if album.Kind != "album" || album.Medium != "audio" || album.Author != "Radiohead" || album.Title != "OK Computer" {
		t.Errorf("album work: %+v", album)
	}
	if album.TrackCount != 2 || album.ItemCount != 2 || album.PartCount != 0 || album.Items[0].ID != 5 || album.RepresentativeItemID != 5 {
		t.Errorf("album members: %+v", album)
	}

	bk := ws[2]
	if bk.Kind != "book" || bk.Medium != "text" || bk.Author != "Frank Herbert" || bk.Year != 1965 || bk.ItemCount != 1 || bk.RepresentativeItemID != 6 {
		t.Errorf("book work: %+v", bk)
	}
	if bk.Genres == nil {
		t.Error("genres must serialize as [], not null")
	}

	// Video works have no author and say so; the single-file audiobook is a
	// work of one part.
	if mv := ws[3]; mv.Medium != "video" || mv.Author != "" {
		t.Errorf("movie work: %+v", mv)
	}
	if single := ws[1]; single.PartCount != 1 || single.Medium != "audio" {
		t.Errorf("single-file audiobook: %+v", single)
	}
}

// A file work — an item the path could not place — takes its own medium, so
// an unidentified .mp3 does not claim to be a video; stored JSON without a
// medium (every item before the field existed) still reads as video.
func TestBuildFileWorkMedium(t *testing.T) {
	ws := Build([]store.Item{
		{ID: 1, ObjectKey: "misc/a.mp3", Identity: &model.Identity{Kind: "unknown", Title: "a"},
			MediaInfo: &model.MediaInfo{Medium: model.MediumAudio}},
		{ID: 2, ObjectKey: "misc/b.mkv", Identity: &model.Identity{Kind: "unknown", Title: "b"},
			MediaInfo: &model.MediaInfo{}},
		{ID: 3, ObjectKey: "misc/c.mkv"}, // probe failed: no media info at all
	})
	if ws[0].Medium != "audio" || ws[1].Medium != "video" || ws[2].Medium != "video" {
		t.Errorf("file media: %s %s %s", ws[0].Medium, ws[1].Medium, ws[2].Medium)
	}
}

// Audiobook progress is time over the whole book: positions on earlier parts
// count those parts as heard in full, the furthest part contributes its own
// position, and the text names the place.
func TestWorkProgressAudiobook(t *testing.T) {
	ws := Build([]store.Item{
		withDuration(part(1, "a/d/01.m4b", "Frank Herbert", "Dune", 1), 3600),
		withDuration(part(2, "a/d/02.m4b", "Frank Herbert", "Dune", 2), 3600),
		withDuration(part(3, "a/d/03.m4b", "Frank Herbert", "Dune", 3), 1800),
	})
	w := &ws[0]

	// Part 1 barely started, part 2 (the furthest) at 41:10: part 1 counts
	// whole, part 2 adds its position — 6070 of 9000 seconds.
	p, ok := WorkProgress(w, map[int64]store.Position{1: pos(1, 60, 10), 2: pos(2, 2470, 20)})
	if !ok {
		t.Fatal("expected progress")
	}
	if want := (3600 + 2470.0) / 9000; p.Fraction != want || p.Status != "active" || p.UpdatedAt != 20 {
		t.Errorf("audiobook progress: %+v, want fraction %v", p, want)
	}
	if p.ProgressText != "part 2 of 3 · 41:10" {
		t.Errorf("progress_text = %q", p.ProgressText)
	}

	// Only the last part touched, near its end: the first two count as heard.
	p, _ = WorkProgress(w, map[int64]store.Position{3: pos(3, 1700, 5)})
	if want := (3600 + 3600 + 1700.0) / 9000; p.Fraction != want || p.Status != "finished" || p.ProgressText != "part 3 of 3 · 28:20" {
		t.Errorf("near the end: %+v, want fraction %v", p, want)
	}

	// A position past a part's duration is clamped to that part.
	p, _ = WorkProgress(w, map[int64]store.Position{1: pos(1, 9999, 5)})
	if p.Fraction != 3600.0/9000 {
		t.Errorf("clamped: %+v", p)
	}

	// The album variant names tracks.
	al := Build([]store.Item{
		withDuration(track(1, "m/r/01.flac", "Radiohead", "OK Computer", 1), 300),
		withDuration(track(2, "m/r/02.flac", "Radiohead", "OK Computer", 2), 300),
	})
	p, _ = WorkProgress(&al[0], map[int64]store.Position{1: pos(1, 100, 1)})
	if p.ProgressText != "track 1 of 2 · 1:40" || p.Fraction != 100.0/600 {
		t.Errorf("album: %+v", p)
	}

	// A single-file audiobook reads like a movie: plain clock, own fraction.
	single := Build([]store.Item{withDuration(part(9, "a/x/Book.m4b", "A", "Book", 0), 1000)})
	p, _ = WorkProgress(&single[0], map[int64]store.Position{9: pos(9, 250, 1)})
	if p.Fraction != 0.25 || p.ProgressText != "4:10" {
		t.Errorf("single file: %+v", p)
	}

	// A chaptered single file names the chapter the position falls in and
	// puts the position over the whole: 1:19:22 into an 11:30:00 book whose
	// seventh chapter starts at 1:00:00 and eighth at 1:20:00.
	m4b := withDuration(part(11, "a/x/Long.m4b", "A", "Long", 0), 41400)
	for i := 0; i < 12; i++ {
		m4b.MediaInfo.Chapters = append(m4b.MediaInfo.Chapters, model.Chapter{StartSeconds: float64(i) * 600, Title: fmt.Sprintf("Chapter %d", i+1)})
	}
	m4b.MediaInfo.Chapters = append(m4b.MediaInfo.Chapters[:7], model.Chapter{StartSeconds: 4800, Title: "Chapter 8"})
	chaptered := Build([]store.Item{m4b})
	p, _ = WorkProgress(&chaptered[0], map[int64]store.Position{11: pos(11, 4762, 1)})
	if p.ProgressText != "ch. 7 · 1:19:22 / 11:30:00" || p.Fraction != 4762.0/41400 {
		t.Errorf("chaptered m4b: %+v", p)
	}
	// Ahead of every marker is still chapter 1; the last marker holds to the end.
	if got := chapterAt(m4b.MediaInfo.Chapters, 0); got != 1 {
		t.Errorf("chapterAt(0) = %d", got)
	}
	if got := chapterAt(m4b.MediaInfo.Chapters, 41000); got != 8 {
		t.Errorf("chapterAt(end) = %d", got)
	}
	if got := chapterAt(nil, 100); got != 0 {
		t.Errorf("chapterAt(no chapters) = %d", got)
	}
	// A chaptered PART of a multi-file set still reads by part: the set is
	// the unit the listener moves through.
	first := m4b
	first.Identity = &model.Identity{Kind: "audiobook_part", Author: "A", Title: "Long", Part: 1}
	set := Build([]store.Item{first, withDuration(part(12, "a/x/02.m4b", "A", "Long", 2), 600)})
	p, _ = WorkProgress(&set[0], map[int64]store.Position{11: pos(11, 4762, 1)})
	if p.ProgressText != "part 1 of 2 · 1:19:22" {
		t.Errorf("chaptered part of a set: %+v", p)
	}

	// A book: a position is acknowledged, but this generation has no locator
	// to turn it into a fraction or a place.
	bk := Build([]store.Item{book(10, "b/a/T.epub", "A", "T", 0)})
	p, ok = WorkProgress(&bk[0], map[int64]store.Position{10: pos(10, 30, 7)})
	if !ok || p.Status != "active" || p.Fraction != 0 || p.ProgressText != "" || p.UpdatedAt != 7 {
		t.Errorf("book: %+v ok=%v", p, ok)
	}
}

// The resume list labels parts and tracks by number, and a finished part
// advances to the next one the way a finished episode does.
func TestContinueListAudio(t *testing.T) {
	ws := Build([]store.Item{
		withDuration(part(1, "a/d/01.m4b", "Frank Herbert", "Dune", 1), 1000),
		withDuration(part(2, "a/d/02.m4b", "Frank Herbert", "Dune", 2), 1000),
		withDuration(part(3, "a/d/Epilogue.m4b", "Frank Herbert", "Dune", 0), 1000),
		withDuration(track(4, "m/r/07 Karma Police.flac", "Radiohead", "OK Computer", 7), 300),
		book(5, "b/a/T.epub", "A", "T", 0),
	})
	entries := ContinueList(ws, []store.Position{pos(1, 400, 10), pos(4, 60, 20)}, 20)
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	if entries[0].ItemID != 4 || entries[0].Label != "Track 7" || entries[0].WorkKey != "album:radiohead-ok-computer" {
		t.Errorf("track entry: %+v", entries[0])
	}
	if entries[1].ItemID != 1 || entries[1].Label != "Part 1" || entries[1].Title != "Dune" {
		t.Errorf("part entry: %+v", entries[1])
	}

	// Finished part 1 → up next is part 2 at 0; finished part 2 → the
	// unnumbered epilogue, labelled by its basename.
	if got := ContinueList(ws, []store.Position{pos(1, 950, 10)}, 20); len(got) != 1 || got[0].ItemID != 2 || got[0].PositionSeconds != 0 || got[0].Label != "Part 2" {
		t.Errorf("up next after part 1: %+v", got)
	}
	if got := ContinueList(ws, []store.Position{pos(2, 950, 10)}, 20); len(got) != 1 || got[0].ItemID != 3 || got[0].Label != "Epilogue.m4b" {
		t.Errorf("up next after part 2: %+v", got)
	}
	// Finished the last part: the book is done.
	if got := ContinueList(ws, []store.Position{pos(3, 950, 10)}, 20); len(got) != 0 {
		t.Errorf("finished audiobook: %+v", got)
	}
	// A book with a position lists by its basename, with no duration.
	if got := ContinueList(ws, []store.Position{pos(5, 30, 10)}, 20); len(got) != 1 || got[0].Label != "T.epub" || got[0].DurationSeconds != 0 {
		t.Errorf("book entry: %+v", got)
	}
}

// FeedWork embeds Work, so kind, medium and author ride into /api/feed/media
// (and /api/works) without any wiring of their own — pinned here so a later
// change to either struct cannot silently drop them.
func TestFeedWorkCarriesMedium(t *testing.T) {
	ws := Build([]store.Item{
		part(1, "a/d/01.m4b", "Frank Herbert", "Dune", 1),
		movie(2, "m/f.mkv", "Frozen", 2013),
	})
	fw := Feed(ws, nil, nil, 0)
	raw, err := json.Marshal(fw)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 {
		t.Fatalf("feed: %s", raw)
	}
	ab, mv := decoded[0], decoded[1]
	if ab["kind"] != "audiobook" || ab["medium"] != "audio" || ab["author"] != "Frank Herbert" || ab["part_count"] != 1.0 {
		t.Errorf("audiobook feed work: %v", ab)
	}
	if mv["kind"] != "movie" || mv["medium"] != "video" {
		t.Errorf("movie feed work: %v", mv)
	}
	if _, has := mv["author"]; has {
		t.Errorf("a video work must omit author, got %v", mv["author"])
	}
	if _, has := mv["part_count"]; has {
		t.Errorf("a video work must omit part_count, got %v", mv["part_count"])
	}
}

// Artists shelves the album works by artist: name order, albums in year order
// (the year-less last), the name spelled as the first album spells it, the
// first album's representative as the picture. Audiobooks and artist-less
// albums are not on any shelf.
func TestArtists(t *testing.T) {
	ws := Build([]store.Item{
		titledTrack(1, "Music/Radiohead/In Rainbows (2007)/01 15 Step.flac", "Radiohead", "In Rainbows", 2007, 1, "15 Step"),
		titledTrack(2, "Music/Radiohead/OK Computer (1997)/01 Airbag.flac", "Radiohead", "OK Computer", 1997, 1, "Airbag"),
		titledTrack(3, "Music/Radiohead/OK Computer (1997)/02 Paranoid Android.flac", "Radiohead", "OK Computer", 1997, 2, "Paranoid Android"),
		// Case differs across albums: one artist, spelled as the earliest album spells it.
		titledTrack(4, "Music/radiohead/Kid A (2000)/01 Everything in Its Right Place.flac", "radiohead", "Kid A", 2000, 1, "Everything in Its Right Place"),
		// A year-less album sorts after the dated ones.
		titledTrack(5, "Music/Radiohead/B-Sides/01 Talk Show Host.flac", "Radiohead", "B-Sides", 0, 1, "Talk Show Host"),
		titledTrack(6, "Music/Bjork/Homogenic (1997)/01 Hunter.flac", "Bjork", "Homogenic", 1997, 1, "Hunter"),
		// Not music, and music with nobody to file it under.
		part(7, "Audiobooks/Frank Herbert/Dune/01.m4b", "Frank Herbert", "Dune", 1),
		track(8, "Music/Orphan.mp3", "", "Orphan", 0),
	})
	got := Artists(ws)
	if len(got) != 2 || got[0].Name != "Bjork" || got[1].Name != "Radiohead" {
		t.Fatalf("artists: %+v", got)
	}
	rh := got[1]
	if rh.AlbumCount != 4 || len(rh.Albums) != 4 || rh.RepresentativeItemID != 2 {
		t.Errorf("radiohead shelf: %+v", rh)
	}
	var titles []string
	for _, a := range rh.Albums {
		titles = append(titles, fmt.Sprintf("%s/%d/%d", a.Title, a.Year, a.TrackCount))
	}
	want := []string{"OK Computer/1997/2", "Kid A/2000/1", "In Rainbows/2007/1", "B-Sides/0/1"}
	if !reflect.DeepEqual(titles, want) {
		t.Errorf("album order = %v, want %v", titles, want)
	}
	if rh.Albums[0].Key != "album:radiohead-ok-computer-1997" || rh.Albums[0].RepresentativeItemID != 2 {
		t.Errorf("album entry: %+v", rh.Albums[0])
	}
	if bj := got[0]; bj.AlbumCount != 1 || bj.RepresentativeItemID != 6 {
		t.Errorf("bjork shelf: %+v", bj)
	}

	// No music at all: an empty list, not null.
	raw, err := json.Marshal(Artists(Build([]store.Item{movie(1, "m/f.mkv", "Frozen", 2013)})))
	if err != nil || string(raw) != "[]" {
		t.Errorf("no artists: %s %v", raw, err)
	}
}

// A track's label carries its title once the identity knows it; without a
// number the title stands alone; an identity from before track titles keeps
// the bare number.
func TestItemLabelTrack(t *testing.T) {
	for _, tc := range []struct {
		it   store.Item
		want string
	}{
		{titledTrack(1, "m/r/07 Karma Police.flac", "Radiohead", "OK Computer", 1997, 7, "Karma Police"), "Track 7 · Karma Police"},
		{titledTrack(2, "m/r/Hidden Track.flac", "Radiohead", "OK Computer", 1997, 0, "Hidden Track"), "Hidden Track"},
		{track(3, "m/r/07 Karma Police.flac", "Radiohead", "OK Computer", 7), "Track 7"},
		{track(4, "m/r/Untitled.flac", "Radiohead", "OK Computer", 0), "Untitled.flac"},
	} {
		if got := itemLabel(tc.it); got != tc.want {
			t.Errorf("itemLabel(%s) = %q, want %q", tc.it.ObjectKey, got, tc.want)
		}
	}
}
