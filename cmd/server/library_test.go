package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// wk is one work in the order table: only the fields the ordering reads.
func wk(key, kind, medium, author string) works.Work {
	return works.Work{Key: key, Kind: kind, Medium: medium, Title: key, Author: author,
		RepresentativeItemID: 1}
}

// The order of the library is one function's answer, and this is its table:
// the bands (tv, movies, music, audiobooks, books), what sits in each, and
// the one work that gets no tile at all — an album its artist's shelf
// already stands for. Each want is "band/subject", because the band a shelf
// lands in is now part of the answer: the home screen draws a row per band.
func TestLibraryOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		works []works.Work
		want  []string
	}{
		{
			name: "one of everything, in band order",
			works: []works.Work{
				wk("album:radiohead-ok-computer", "album", model.MediumAudio, "Radiohead"),
				wk("audiobook:dune", "audiobook", model.MediumAudio, "Frank Herbert"),
				wk("book:1984", "book", model.MediumText, "George Orwell"),
				wk("movie:frozen", "movie", model.MediumVideo, ""),
				wk("show:the-office", "show", model.MediumVideo, ""),
			},
			// The album is not here: its artist's shelf is.
			want: []string{"tv/show:the-office", "movies/movie:frozen", "music/Radiohead",
				"audiobooks/audiobook:dune", "books/book:1984"},
		},
		{
			name: "the works' own order holds inside a band",
			works: []works.Work{
				wk("movie:arrival", "movie", model.MediumVideo, ""),
				wk("movie:frozen", "movie", model.MediumVideo, ""),
				wk("show:atlanta", "show", model.MediumVideo, ""),
				wk("show:the-office", "show", model.MediumVideo, ""),
			},
			want: []string{"tv/show:atlanta", "tv/show:the-office",
				"movies/movie:arrival", "movies/movie:frozen"},
		},
		{
			name: "an album nobody filed under an artist keeps its own tile",
			works: []works.Work{
				wk("album:untitled", "album", model.MediumAudio, ""),
				wk("album:kid-a", "album", model.MediumAudio, "Radiohead"),
			},
			want: []string{"music/Radiohead", "music/album:untitled"},
		},
		{
			name: "a file the path could not place lands by its medium",
			works: []works.Work{
				wk("file:1", "file", model.MediumAudio, ""),
				wk("file:2", "file", model.MediumText, ""),
				wk("file:3", "file", model.MediumVideo, ""),
				wk("show:atlanta", "show", model.MediumVideo, ""),
			},
			want: []string{"tv/show:atlanta", "movies/file:3", "music/file:1", "books/file:2"},
		},
		{name: "an empty library is an empty grid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, sh := range libraryOrder(tc.works, works.Artists(tc.works)) {
				if sh.Artist != nil {
					got = append(got, sh.Band+"/"+sh.Artist.Name)
					continue
				}
				got = append(got, sh.Band+"/"+sh.Work.Key)
			}
			if strings.Join(got, ", ") != strings.Join(tc.want, ", ") {
				t.Errorf("order = [%s], want [%s]", strings.Join(got, ", "), strings.Join(tc.want, ", "))
			}
		})
	}
}

// arrivedWork is one work in the recently-added table: the same work the
// order table builds, with member files that arrived on the given days of
// one January. Day 0 is a file with no arrival time at all — a row written
// before the library kept one.
func arrivedWork(key, kind, medium, author string, days ...int) works.Work {
	w := wk(key, kind, medium, author)
	for i, d := range days {
		it := store.Item{ID: int64(i + 1)}
		if d > 0 {
			it.AddedAt = time.Date(2026, time.January, d, 12, 0, 0, 0, time.UTC)
		}
		w.Items = append(w.Items, it)
	}
	return w
}

// "Recently added" is a re-ordering of the grid's own shelves, and this is
// its table: what a work's arrival is (its newest file), what an artist's
// shelf's arrival is (their newest record), how many the row holds, and what
// happens to a thing that has no arrival time.
func TestRecentlyAdded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		n     int
		works []works.Work
		want  []string
	}{
		{
			name: "newest first, whatever band the grid put them in",
			n:    12,
			works: []works.Work{
				arrivedWork("book:1984", "book", model.MediumText, "George Orwell", 5),
				arrivedWork("movie:frozen", "movie", model.MediumVideo, "", 20),
				arrivedWork("show:atlanta", "show", model.MediumVideo, "", 11),
			},
			want: []string{"movie:frozen", "show:atlanta", "book:1984"},
		},
		{
			name: "a show is as new as its newest episode",
			n:    12,
			works: []works.Work{
				arrivedWork("movie:frozen", "movie", model.MediumVideo, "", 10),
				// An old first season, a new one just added.
				arrivedWork("show:atlanta", "show", model.MediumVideo, "", 3, 22),
			},
			want: []string{"show:atlanta", "movie:frozen"},
		},
		{
			name: "an artist's shelf arrives with their newest record",
			n:    12,
			works: []works.Work{
				arrivedWork("album:radiohead-pablo-honey", "album", model.MediumAudio, "Radiohead", 2),
				arrivedWork("album:radiohead-kid-a", "album", model.MediumAudio, "Radiohead", 25),
				arrivedWork("movie:frozen", "movie", model.MediumVideo, "", 10),
			},
			want: []string{"Radiohead", "movie:frozen"},
		},
		{
			name: "the row is capped, and the cut is at the far end",
			n:    2,
			works: []works.Work{
				arrivedWork("movie:arrival", "movie", model.MediumVideo, "", 1),
				arrivedWork("movie:frozen", "movie", model.MediumVideo, "", 30),
				arrivedWork("show:atlanta", "show", model.MediumVideo, "", 15),
			},
			want: []string{"movie:frozen", "show:atlanta"},
		},
		{
			name: "a work with no arrival time is not recently added",
			n:    12,
			works: []works.Work{
				arrivedWork("movie:arrival", "movie", model.MediumVideo, "", 0),
				arrivedWork("movie:frozen", "movie", model.MediumVideo, "", 4),
			},
			want: []string{"movie:frozen"},
		},
		{
			name: "same day: the grid's own order breaks the tie",
			n:    12,
			works: []works.Work{
				arrivedWork("movie:arrival", "movie", model.MediumVideo, "", 7),
				arrivedWork("movie:frozen", "movie", model.MediumVideo, "", 7),
				arrivedWork("show:atlanta", "show", model.MediumVideo, "", 7),
			},
			// Shows band before films, and films in their own order.
			want: []string{"show:atlanta", "movie:arrival", "movie:frozen"},
		},
		{name: "an empty library has nothing new in it", n: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := libraryOrder(tc.works, works.Artists(tc.works))
			var got []string
			for _, sh := range recentlyAdded(order, works.ByKey(tc.works), tc.n) {
				if sh.Artist != nil {
					got = append(got, sh.Artist.Name)
					continue
				}
				got = append(got, sh.Work.Key)
			}
			if strings.Join(got, ", ") != strings.Join(tc.want, ", ") {
				t.Errorf("recently added = [%s], want [%s]",
					strings.Join(got, ", "), strings.Join(tc.want, ", "))
			}
		})
	}
}

// tile is a library tile as a test reads it.
type tile struct {
	Self     string   `json:"self"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	WorkKind string   `json:"work_kind"`
	Subtitle string   `json:"subtitle"`
	Tech     string   `json:"tech"`
	Genres   []string `json:"genres"`
	ItemID   int64    `json:"item_id"`
	Search   string   `json:"search"`
	Band     string   `json:"band"`
	Links    map[string]struct {
		Href string `json:"href"`
	} `json:"links"`
}

type libraryDoc struct {
	Count int `json:"count"`
	Bands []struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"bands"`
	Items         []tile `json:"items"`
	RecentlyAdded []tile `json:"recently_added"`
	Facets        map[string][]struct {
		Value string `json:"value"`
		Count int    `json:"count"`
	} `json:"facets"`
}

func library(t *testing.T, h http.Handler, target string) libraryDoc {
	t.Helper()
	w := get(t, h, target)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body)
	}
	var doc libraryDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET %s: %v (%s)", target, err, w.Body)
	}
	return doc
}

// The document names its rows and every tile names the row it sits in, so a
// screen can draw headed sections without grouping anything itself. The
// bands come in the document's own order, and only the ones that hold
// something are offered.
func TestLibraryBands(t *testing.T) {
	_, h := fixtureServer(t)
	doc := library(t, h, "/api/library?client_id=chris")

	var keys, titles []string
	for _, b := range doc.Bands {
		keys = append(keys, b.Key)
		titles = append(titles, b.Title)
	}
	if want := "tv, movies, music, audiobooks, books"; strings.Join(keys, ", ") != want {
		t.Errorf("bands = [%s], want [%s]", strings.Join(keys, ", "), want)
	}
	for i, tl := range titles {
		if tl == "" {
			t.Errorf("band %q has no heading", keys[i])
		}
	}

	// Every tile lands in a band the document offers, and the tiles come in
	// band order: a screen renders them section by section without sorting.
	offered := map[string]int{}
	for i, k := range keys {
		offered[k] = i
	}
	last := -1
	for _, tl := range doc.Items {
		at, ok := offered[tl.Band]
		if !ok {
			t.Fatalf("%q is in band %q, which the document does not offer", tl.Title, tl.Band)
		}
		if at < last {
			t.Errorf("%q (band %q) comes after a later band's tile", tl.Title, tl.Band)
		}
		last = at
	}

	// A tile drawn out of its row still says which row it belongs to.
	for _, tl := range doc.RecentlyAdded {
		if tl.Band == "" {
			t.Errorf("the recently-added tile %q names no band", tl.Title)
		}
	}

	byTitle := map[string]tile{}
	for _, tl := range doc.Items {
		byTitle[tl.Title] = tl
	}
	for title, want := range map[string]string{
		"The Office": "tv", "Frozen": "movies", "Radiohead": "music",
		"Dune": "audiobooks", "Flatland": "books",
	} {
		if got := byTitle[title].Band; got != want {
			t.Errorf("%q is in band %q, want %q", title, got, want)
		}
	}
}

// Every tile says where it goes and what to draw, and the artist's shelf
// stands in for the records behind it.
func TestLibraryTiles(t *testing.T) {
	_, h := fixtureServer(t)
	doc := library(t, h, "/api/library?client_id=chris")
	if doc.Count != len(doc.Items) {
		t.Errorf("count = %d, %d items", doc.Count, len(doc.Items))
	}
	byTitle := map[string]tile{}
	for _, it := range doc.Items {
		byTitle[it.Title] = it
		if it.Links["self"].Href != it.Self {
			t.Errorf("%q: links.self = %q, self = %q", it.Title, it.Links["self"].Href, it.Self)
		}
		if it.Links["artwork"].Href == "" {
			t.Errorf("%q has no artwork link", it.Title)
		}
		if it.Search == "" || it.Search != strings.ToLower(it.Search) {
			t.Errorf("%q: search = %q", it.Title, it.Search)
		}
	}
	for _, gone := range []string{"OK Computer", "Pablo Honey", "The Bends"} {
		if _, ok := byTitle[gone]; ok {
			t.Errorf("%q has a tile of its own; its artist's shelf stands for it", gone)
		}
	}

	office := byTitle["The Office"]
	if office.WorkKind != "show" || office.Self != "/api/works/tmdb%3A2316" {
		t.Errorf("the show tile = %+v", office)
	}
	if office.Subtitle != "1 season · 2 episodes · 1 extra" {
		t.Errorf("show subtitle = %q", office.Subtitle)
	}
	if office.Links["artwork"].Href != "/api/items/8/poster" {
		t.Errorf("a show's picture is its poster: %q", office.Links["artwork"].Href)
	}

	radiohead := byTitle["Radiohead"]
	if radiohead.Kind != "artist" || radiohead.Self != "/api/artists/Radiohead" {
		t.Errorf("the artist tile = %+v", radiohead)
	}
	if radiohead.Subtitle != "3 albums" || radiohead.Tech != "artist · 1993–1997" {
		t.Errorf("artist tile = %q / %q", radiohead.Subtitle, radiohead.Tech)
	}
	if radiohead.Links["artwork"].Href != "/api/items/10/cover" {
		t.Errorf("an artist's picture is a record's own cover: %q", radiohead.Links["artwork"].Href)
	}
	if !strings.Contains(radiohead.Search, "ok computer") {
		t.Errorf("a shelf is searched by the records on it: %q", radiohead.Search)
	}

	dune := byTitle["Dune"]
	if dune.Subtitle != "Frank Herbert · 1965 · 2 parts" || dune.ItemID != idDunePart1 {
		t.Errorf("the audiobook tile = %+v", dune)
	}
	if dune.Links["artwork"].Href != "/api/items/1/cover" {
		t.Errorf("an audiobook's picture is its own cover: %q", dune.Links["artwork"].Href)
	}
}

// The library document carries what is new as the SAME tiles the grid draws
// — a row a client can render with the renderer it already has — newest
// first, over the fixture's own arrival dates.
func TestLibraryRecentlyAdded(t *testing.T) {
	_, h := fixtureServer(t)
	doc := library(t, h, "/api/library?client_id=chris")
	var got []string
	for _, tl := range doc.RecentlyAdded {
		got = append(got, tl.Title)
	}
	want := []string{"Radiohead", "Frozen", "The Office", "Dune",
		"The Haunting of Hill House", "Flatland"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("recently added = [%s], want [%s]",
			strings.Join(got, ", "), strings.Join(want, ", "))
	}
	// The same envelope, not a thinner one: a tile is a tile wherever it is
	// drawn, so the row needs no renderer of its own.
	byTitle := map[string]tile{}
	for _, tl := range doc.Items {
		byTitle[tl.Title] = tl
	}
	for _, tl := range doc.RecentlyAdded {
		if grid := byTitle[tl.Title]; grid.Self != tl.Self || grid.Tech != tl.Tech ||
			grid.Subtitle != tl.Subtitle || grid.ItemID != tl.ItemID {
			t.Errorf("%q: the recently-added tile differs from the grid's", tl.Title)
		}
	}
}

// The chips are the document's: the commonest genres over the tiles that
// carry them, in the order the row draws them.
func TestLibraryGenreFacet(t *testing.T) {
	_, h := fixtureServer(t)
	doc := library(t, h, "/api/library")
	got := map[string]int{}
	var order []string
	for _, f := range doc.Facets["genre"] {
		got[f.Value] = f.Count
		order = append(order, f.Value)
	}
	for _, g := range []string{"Comedy", "Animation", "Family", "Adventure"} {
		if got[g] != 1 {
			t.Errorf("genre %q counted %d, want 1 (one work carries it)", g, got[g])
		}
	}
	if strings.Join(order, ",") != "Adventure,Animation,Comedy,Family" {
		t.Errorf("facet order = %v; ties are broken by name", order)
	}
}

// An artist's shelf: the albums in year order with the year-less last, each
// a tile with its own cover, and the name spelled the way the shelf spells
// it however it was asked for.
func TestArtistDocument(t *testing.T) {
	_, h := fixtureServer(t)
	var doc struct {
		Self       string `json:"self"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		AlbumCount int    `json:"album_count"`
		Albums     []tile `json:"albums"`
		Links      map[string]struct {
			Href string `json:"href"`
		} `json:"links"`
	}
	w := get(t, h, "/api/artists/radiohead") // asked for in lower case
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/artists/radiohead = %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "artist" || doc.Name != "Radiohead" || doc.Self != "/api/artists/Radiohead" {
		t.Errorf("the shelf answers in its own spelling: %+v", doc)
	}
	if doc.AlbumCount != 3 || len(doc.Albums) != 3 {
		t.Fatalf("album_count = %d, %d albums", doc.AlbumCount, len(doc.Albums))
	}
	var titles []string
	for _, al := range doc.Albums {
		titles = append(titles, al.Title)
		if al.Links["artwork"].Href == "" || !strings.HasSuffix(al.Links["artwork"].Href, "/cover") {
			t.Errorf("%q: artwork = %q, want a cover", al.Title, al.Links["artwork"].Href)
		}
	}
	if strings.Join(titles, ", ") != "Pablo Honey, OK Computer, The Bends" {
		t.Errorf("albums = %v; year order, the year-less last", titles)
	}
	// A tile opens at the record's first track, wherever the record sits.
	if doc.Albums[0].ItemID != idYou || doc.Albums[2].ItemID != idTelex {
		t.Errorf("albums open at items %d and %d", doc.Albums[0].ItemID, doc.Albums[2].ItemID)
	}
	// On the shelf the name at the top has already said who.
	if doc.Albums[1].Subtitle != "1997 · 2 tracks" {
		t.Errorf("shelf subtitle = %q", doc.Albums[1].Subtitle)
	}
	if doc.Links["artwork"].Href != "/api/items/10/cover" {
		t.Errorf("the shelf's picture = %q", doc.Links["artwork"].Href)
	}
}

// A name no record is filed under is a refusal with somewhere to go.
func TestUnknownArtistIsAProblem(t *testing.T) {
	_, h := fixtureServer(t)
	w := get(t, h, "/api/artists/Nobody")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /api/artists/Nobody = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q", ct)
	}
	var p struct {
		Type   string `json:"type"`
		Remedy *struct {
			Text string `json:"text"`
			Link *struct {
				Href string `json:"href"`
			} `json:"link"`
		} `json:"remedy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "no-such-artist" {
		t.Errorf("type = %q", p.Type)
	}
	if p.Remedy == nil || p.Remedy.Link == nil || p.Remedy.Link.Href != "/api/library" {
		t.Errorf("remedy = %+v; the library is where to go instead", p.Remedy)
	}
}

// The flat lists the feed and the tools read are untouched by any of this.
func TestArtistListStillFlat(t *testing.T) {
	_, h := fixtureServer(t)
	var list []works.Artist
	if err := json.Unmarshal(get(t, h, "/api/artists").Body.Bytes(), &list); err != nil {
		t.Fatalf("/api/artists: %v", err)
	}
	if len(list) != 1 || list[0].Name != "Radiohead" || list[0].AlbumCount != 3 {
		t.Errorf("/api/artists = %+v", list)
	}
}
