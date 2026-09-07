package main

// The library and the artist shelf as documents — docs/hypermedia.md, step 3
// of its order of work.
//
// The grid's ORDER lives here now, in one Go function (libraryOrder): tv,
// movies, music, audiobooks, books — the bands a home screen draws as headed
// rows, named once in libraryBands. It used to live in the browser,
// which regrouped /api/items into shows, albums and artists on every render
// — a second copy of works.Build and works.Artists, in another language,
// drifting. The browser now draws what this document lists, in the order it
// lists it, and its search and genre chips only FILTER those tiles.
//
// A tile is a small envelope: where the thing lives (self, links.self), what
// to draw (title, subtitle, tech, links.artwork) and what a chip or a search
// box matches (genres, search). Everything a screen would otherwise have to
// derive — the season count, the "3 albums", whether the picture is a poster
// or a cover — is decided here.

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// artistHref is one artist's own address. The name is a path segment, so a
// slash inside it ("AC/DC") is escaped rather than read as one.
func artistHref(name string) string { return "/api/artists/" + url.PathEscape(name) }

// maxGenreFacets is how many genre chips the row offers. The grid showed the
// twelve commonest and no more; the count is the document's now.
const maxGenreFacets = 12

// ── the order ───────────────────────────────────────────────────────────

// shelf is one tile's subject: a work, or an artist whose shelf of albums
// stands in for them. Exactly one of the two is set. Band is the row it
// belongs to — the key of one of libraryBands.
type shelf struct {
	Work   *works.Work
	Artist *works.Artist
	Band   string
}

// band is one row of the library: the key its tiles carry and the heading a
// screen draws over them.
type band struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// libraryBands is the rows the library has and the order they are read in,
// top to bottom. The home screen used to draw one wall of tiles; it draws
// these rows now, and this is the only place their order is decided.
var libraryBands = []band{
	{Key: "tv", Title: "TV"},
	{Key: "movies", Title: "Movies"},
	{Key: "music", Title: "Music"},
	{Key: "audiobooks", Title: "Audiobooks"},
	{Key: "books", Title: "Books"},
}

// libraryOrder is THE order of the library grid, and the only place it is
// decided:
//
//	tv, movies, music, audiobooks, books
//
// Within each band the order is the one it arrives in — works.Build sorts by
// title, works.Artists by name, and an artist's shelf leads the music ahead
// of the records nobody filed — so the bands are the only new rule.
//
// An album filed under an artist is NOT a tile of its own: its artist's
// shelf stands for it, which is what the browser did by hand. An album
// nobody filed under an artist keeps its tile, among the music. A file work
// — anything the path could not place — lands in the band its medium belongs
// to, so no file is ever invisible.
func libraryOrder(ws []works.Work, as []works.Artist) []shelf {
	shelved := map[string]bool{} // album keys that sit on an artist's shelf
	for i := range as {
		for _, al := range as[i].Albums {
			shelved[al.Key] = true
		}
	}
	var shows, films, music, audiobooks, books []shelf
	for i := range as {
		music = append(music, shelf{Artist: &as[i], Band: "music"})
	}
	for i := range ws {
		w := &ws[i]
		if shelved[w.Key] {
			continue
		}
		switch {
		case w.Kind == "show":
			shows = append(shows, shelf{Work: w, Band: "tv"})
		case w.Kind == "book":
			books = append(books, shelf{Work: w, Band: "books"})
		case w.Kind == "audiobook":
			audiobooks = append(audiobooks, shelf{Work: w, Band: "audiobooks"})
		case w.Kind == "album", w.Medium == model.MediumAudio:
			music = append(music, shelf{Work: w, Band: "music"})
		case w.Medium == model.MediumText:
			books = append(books, shelf{Work: w, Band: "books"})
		default: // a film, and any other video
			films = append(films, shelf{Work: w, Band: "movies"})
		}
	}
	out := make([]shelf, 0, len(shows)+len(films)+len(music)+len(audiobooks)+len(books))
	out = append(out, shows...)
	out = append(out, films...)
	out = append(out, music...)
	out = append(out, audiobooks...)
	return append(out, books...)
}

// bandsOf is the rows this library actually has, in libraryBands' order. A
// band nothing landed in is left out, so a screen never draws a heading over
// nothing.
func bandsOf(order []shelf) []band {
	has := map[string]bool{}
	for _, sh := range order {
		has[sh.Band] = true
	}
	out := make([]band, 0, len(libraryBands))
	for _, b := range libraryBands {
		if has[b.Key] {
			out = append(out, b)
		}
	}
	return out
}

// ── the tiles ───────────────────────────────────────────────────────────

// workTile is one work as the GRID draws it: with the author in the subtitle,
// because a grid says nothing else about who made a thing.
func workTile(w *works.Work) *hyper.Envelope {
	return tileFor(w, tileSubtitle(w))
}

// shelfTile is the same work on its artist's own shelf, where the name at
// the top of the page has already said who: the subtitle keeps the year and
// the track count and drops the author.
func shelfTile(w *works.Work) *hyper.Envelope {
	return tileFor(w, joinDot(year(w.Year), memberCount(w)))
}

// tileFor is one work as a tile. `item_id` is the member a tap opens — the
// film itself, the first episode, track one — for the screens that address
// an item rather than the work.
func tileFor(w *works.Work, subtitle string) *hyper.Envelope {
	t := hyper.Doc(workHref(w.Key), "work", w.Title).
		Field("work_kind", w.Kind).
		Field("medium", w.Medium)
	if w.Author != "" {
		t.Field("author", w.Author)
	}
	if w.Year != 0 {
		t.Field("year", w.Year)
	}
	if subtitle != "" {
		t.Field("subtitle", subtitle)
	}
	t.Field("tech", tileTech(w))
	if len(w.Genres) > 0 {
		t.Field("genres", w.Genres)
	}
	t.Field("item_id", w.Items[0].ID)
	t.Field("search", searchText(w.Title, w.Author, path.Base(w.Items[0].ObjectKey)))
	t.Link("self", workHref(w.Key), w.Title)
	t.Link("artwork", artworkHref(w.RepresentativeItemID, w.Medium), "")
	// The wide picture, where there is one: a row that leads with a hero
	// rather than a grid of tiles has it on the tile it already draws.
	if rep := memberByID(w, w.RepresentativeItemID); rep != nil &&
		rep.Enrichment != nil && rep.Enrichment.HasBackdrop {
		t.Link("backdrop", itemHref(rep.ID)+"/backdrop", "")
	}
	return t
}

// tileOf is one shelf as a tile, whichever of the two subjects it has, with
// the band it belongs to on it: a row draws the tiles that name it, and the
// "recently added" row carries the band along, so a tile out of its row
// still knows where it came from.
func tileOf(sh shelf) *hyper.Envelope {
	var t *hyper.Envelope
	if sh.Artist != nil {
		t = artistTile(sh.Artist)
	} else {
		t = workTile(sh.Work)
	}
	if sh.Band != "" {
		t.Field("band", sh.Band)
	}
	return t
}

// artistTile is one artist's shelf as the grid draws it: the name, how many
// records are behind it, and the years they span. Its `self` is the artist
// document, not a work — the tile opens a shelf, not a record.
func artistTile(a *works.Artist) *hyper.Envelope {
	titles := make([]string, 0, len(a.Albums))
	for _, al := range a.Albums {
		titles = append(titles, al.Title)
	}
	return hyper.Doc(artistHref(a.Name), "artist", a.Name).
		Field("medium", model.MediumAudio).
		Field("album_count", a.AlbumCount).
		Field("subtitle", plural(a.AlbumCount, "album")).
		Field("tech", joinDot("artist", yearSpan(a))).
		Field("search", searchText(append([]string{a.Name}, titles...)...)).
		Link("self", artistHref(a.Name), a.Name).
		Link("artwork", artworkHref(a.RepresentativeItemID, model.MediumAudio), "")
}

// artworkHref is the picture a tile draws, decided here rather than in the
// browser: an audio work's or a book's OWN cover (extracted from the file),
// the TMDB poster for anything with a picture of its own to show. The
// address is the work's representative member — the one member with a
// poster, or the sorted head.
func artworkHref(itemID int64, medium string) string {
	switch medium {
	case model.MediumAudio, model.MediumText:
		return itemHref(itemID) + "/cover"
	}
	return itemHref(itemID) + "/poster"
}

// tileSubtitle is the line under the title: what this thing is made of.
// A show counts its seasons and episodes, a record its tracks, and
// everything authored says who by.
func tileSubtitle(w *works.Work) string {
	switch w.Kind {
	case "show":
		var parts []string
		if n := seasonCount(w); n > 0 {
			parts = append(parts, plural(n, "season"))
		}
		if w.EpisodeCount > 0 {
			parts = append(parts, plural(w.EpisodeCount, "episode"))
		}
		if w.ExtraCount > 0 {
			parts = append(parts, plural(w.ExtraCount, "extra"))
		}
		return strings.Join(parts, " · ")
	case "audiobook", "album":
		return joinDot(w.Author, year(w.Year), memberCount(w))
	}
	// A film's year; a book's author and year.
	return joinDot(w.Author, year(w.Year))
}

// memberCount is how many parts or tracks a record is in, and nothing at all
// when it is in one — "1 track" is a count nobody asked for.
func memberCount(w *works.Work) string {
	n := len(w.Items)
	if n < 2 {
		return ""
	}
	if w.Kind == "album" {
		return plural(n, "track")
	}
	return plural(n, "part")
}

// tileTech is the small grey line: what the file IS. It is the tile's text
// fallback too — when there is no artwork at all the browser draws the title
// and this line in the poster's place.
func tileTech(w *works.Work) string {
	first := w.Items[0]
	if w.Medium == model.MediumAudio {
		var total float64
		for _, it := range w.Items {
			if it.MediaInfo != nil {
				total += it.MediaInfo.DurationSeconds
			}
		}
		kind := w.Kind
		if kind == "file" {
			kind = model.MediumAudio
		}
		codec := ""
		if first.MediaInfo != nil {
			codec = first.MediaInfo.AudioCodec
		}
		clock := ""
		if total > 0 {
			clock = works.Clock(total)
		}
		return joinDot(kind, codec, clock)
	}
	if w.Kind == "show" {
		return resLine(first.MediaInfo) // one episode stands for the run
	}
	return techLine(first)
}

// seasonCount counts NUMBERED seasons only: a show of unnumbered files knows
// how many episodes it has, not how many seasons.
func seasonCount(w *works.Work) int {
	seen := map[int]bool{}
	for _, it := range w.Items {
		if identityKind(it) == "episode" && it.Identity.Season > 0 {
			seen[it.Identity.Season] = true
		}
	}
	return len(seen)
}

// yearSpan is an artist's shelf in years: "1997", or "1993–1997" when the
// records are spread. Albums arrive in year order with the year-less last.
func yearSpan(a *works.Artist) string {
	var years []int
	for _, al := range a.Albums {
		if al.Year != 0 {
			years = append(years, al.Year)
		}
	}
	if len(years) == 0 {
		return ""
	}
	if first, last := years[0], years[len(years)-1]; first != last {
		return fmt.Sprintf("%d–%d", first, last)
	}
	return fmt.Sprintf("%d", years[0])
}

// resLine is a file's shape in a phrase: the picture's size and codecs, or —
// where there is no picture — what an audio or a text file can say instead.
func resLine(mi *model.MediaInfo) string {
	if mi == nil {
		return "probe failed"
	}
	switch mi.MediumOrVideo() {
	case model.MediumAudio:
		if mi.AudioChannels > 0 {
			return fmt.Sprintf("%s %dch", mi.AudioCodec, mi.AudioChannels)
		}
		return mi.AudioCodec
	case model.MediumVideo:
		s := fmt.Sprintf("%dx%d %s/%s", mi.Width, mi.Height, mi.VideoCodec, mi.AudioCodec)
		if mi.HDR != "" {
			s += " " + strings.ToUpper(mi.HDR)
		}
		return s
	default:
		return mi.MediumOrVideo() // text: no picture to size either
	}
}

// techLine is resLine plus how big the file is and how many places it has.
func techLine(it store.Item) string {
	s := resLine(it.MediaInfo) + " · " + fileSize(it.Size)
	if mi := it.MediaInfo; mi != nil {
		if n := len(mi.Chapters); n > 0 {
			s += fmt.Sprintf(" · %d ch", n)
		}
		if mi.PageCount > 0 {
			s += fmt.Sprintf(" · %d p.", mi.PageCount)
		}
	}
	return s
}

func fileSize(n int64) string {
	if n < 1e9 {
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.2f GB", float64(n)/1e9)
}

// searchText is what the grid's search box matches this tile against, folded
// once here rather than on every keystroke: the title, who made it, and the
// file's own name (a film often being searched for by the name on disk).
func searchText(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return strings.Join(out, " ")
}

func joinDot(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func year(y int) string {
	if y == 0 {
		return ""
	}
	return fmt.Sprintf("%d", y)
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// ── recently added ──────────────────────────────────────────────────────

// recentlyAddedTiles is how many tiles the "Recently added" row holds.
const recentlyAddedTiles = 12

// shelfAddedAt is when the newest file behind a tile arrived: a show is as
// new as its newest episode, and an artist's shelf is as new as their newest
// record — a new album puts the artist back at the front, which is what a
// person means by "that's new".
func shelfAddedAt(sh shelf, byKey map[string]*works.Work) time.Time {
	var newest time.Time
	add := func(w *works.Work) {
		if w == nil {
			return
		}
		for _, it := range w.Items {
			if it.AddedAt.After(newest) {
				newest = it.AddedAt
			}
		}
	}
	add(sh.Work)
	if sh.Artist != nil {
		for _, al := range sh.Artist.Albums {
			add(byKey[al.Key])
		}
	}
	return newest
}

// recentlyAdded is the grid's own shelves re-ordered by arrival — newest
// first, at most n — and it is a SELECTION of the same tiles, not a second
// kind of thing: what is new is a question about the library's order, and
// the library document is where the order is decided.
//
// A shelf whose files predate the arrival column carries no time at all and
// is left out: a "recently added" row is worth having short, and not worth
// having wrong. Ties keep the grid's order, so the row is deterministic.
func recentlyAdded(order []shelf, byKey map[string]*works.Work, n int) []shelf {
	type dated struct {
		sh shelf
		at time.Time
	}
	var known []dated
	for _, sh := range order {
		if at := shelfAddedAt(sh, byKey); !at.IsZero() {
			known = append(known, dated{sh, at})
		}
	}
	sort.SliceStable(known, func(i, j int) bool { return known[i].at.After(known[j].at) })
	if len(known) > n {
		known = known[:n]
	}
	out := make([]shelf, 0, len(known))
	for _, d := range known {
		out = append(out, d.sh)
	}
	return out
}

// ── the facets ──────────────────────────────────────────────────────────

// facet is one value a filter offers and how many tiles carry it. It is a
// LIST, not an object: the order is the document's (commonest first, ties by
// name), and a chip row renders it top to bottom without sorting anything.
type facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// genreFacet counts genres over TILES — the unit the chip filters — and
// keeps the commonest few. Only TMDB-enriched works carry genres, so a
// library of music and books answers with an empty list and the chip row
// stays away.
func genreFacet(order []shelf) []facet {
	count := map[string]int{}
	for _, s := range order {
		if s.Work == nil {
			continue
		}
		for _, g := range s.Work.Genres {
			if g != "" {
				count[g]++
			}
		}
	}
	out := make([]facet, 0, len(count))
	for g, n := range count {
		out = append(out, facet{Value: g, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > maxGenreFacets {
		out = out[:maxGenreFacets]
	}
	return out
}

// ── the documents ───────────────────────────────────────────────────────

// handleLibrary is GET /api/library — every tile the home screen draws, in
// the order it draws them, the rows it draws them under (`bands`, each tile
// naming its own), and the facets its chips offer.
//
// The profile is carried where the grid honours one, which today is nowhere:
// the same shelf is shown to everybody, and where a person stands in a work
// is the work document's answer. The document names who asked all the same,
// so the resume link below it can.
func (s *server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	order := libraryOrder(ws, works.Artists(ws))
	tiles := make([]*hyper.Envelope, 0, len(order))
	for _, sh := range order {
		tiles = append(tiles, tileOf(sh))
	}
	byKey := works.ByKey(ws)
	recent := make([]*hyper.Envelope, 0, recentlyAddedTiles)
	for _, sh := range recentlyAdded(order, byKey, recentlyAddedTiles) {
		recent = append(recent, tileOf(sh))
	}

	profile := profileOf(r)
	doc := hyper.Doc("/api/library", "library", "Library")
	if profile != "" {
		doc.Field("profile", profile)
	}
	cont := "/api/continue"
	if profile != "" {
		cont += "?client_id=" + url.QueryEscape(profile)
	}
	doc.Field("count", len(tiles)).
		Field("bands", bandsOf(order)).
		Field("items", tiles).
		Field("recently_added", recent).
		Field("facets", map[string][]facet{"genre": genreFacet(order)}).
		Link("root", "/api/", "flickr").
		Link("continue", cont, "Continue watching").
		Link("artists", "/api/artists", "Artists")
	hyper.WriteDoc(w, http.StatusOK, doc)
}

// handleArtistDoc is GET /api/artists/{name} — one artist's shelf: the
// albums in year order, each as the same tile the grid draws, and the
// picture that stands for the artist.
//
// The name is matched the way works.Artists shelves it, case-insensitively,
// and the document answers in the artist's OWN spelling — the one the first
// record spells — whatever spelling asked.
func (s *server) handleArtistDoc(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	shelves := works.Artists(ws)
	var found *works.Artist
	for i := range shelves {
		if strings.EqualFold(shelves[i].Name, name) {
			found = &shelves[i]
			break
		}
	}
	if found == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-artist",
			"No such artist", fmt.Sprintf("no album in the library is filed under %q", name)).
			WithRemedy("open the library and follow an artist's shelf from there",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	byKey := works.ByKey(ws)
	albums := make([]*hyper.Envelope, 0, len(found.Albums))
	tracks := 0
	for _, al := range found.Albums {
		tracks += al.TrackCount
		if wk := byKey[al.Key]; wk != nil {
			albums = append(albums, shelfTile(wk))
		}
	}
	doc := hyper.Doc(artistHref(found.Name), "artist", found.Name).
		Field("name", found.Name).
		Field("album_count", found.AlbumCount).
		Field("track_count", tracks).
		Field("albums", albums).
		Link("artwork", artworkHref(found.RepresentativeItemID, model.MediumAudio), "").
		Link("library", "/api/library", "Library")
	hyper.WriteDoc(w, http.StatusOK, doc)
}
