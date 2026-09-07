package main

// The search document — GET /api/search?q=…
//
// The library document's tiles carry a `search` field and the grid filters
// them against it on every keystroke. That finds a title and nothing else:
// an episode's name is not on a tile, nor is a track's, so "beach games"
// found The Office only if you already knew the show. This is the other
// half — one substring over everything the library holds, answered as a
// document of GROUPS the way the library answers bands, so a screen draws
// headed rows and does no grouping of its own.
//
// A hit is either a WORK (a show, a film, a record) as the very tile the
// grid already draws, or one MEMBER — an episode, a track, a part, a book —
// as a small envelope of its own: where it lives, what to call it, and the
// ids a hash is spelled from. Nothing here offers an action: a result is a
// place to go, and what may be done there is that document's answer.

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// searchMax is how many hits one group carries. A search that matches half
// the library is a search worth narrowing, and the group says how many it
// matched either way.
const searchMax = 20

// searchBands is the groups a search answers and the order they are read
// in, top to bottom — works first, because a title is what most searches
// are for, then the artists whose shelf is a title of a kind, then the
// members a title would never have found — and last the DIALOGUE, which is
// not a thing in the library at all but a moment inside one.
var searchBands = []band{
	{Key: "works", Title: "Titles"},
	{Key: "artists", Title: "Artists"},
	{Key: "episodes", Title: "Episodes"},
	{Key: "tracks", Title: "Tracks"},
	{Key: "parts", Title: "Parts"},
	{Key: "books", Title: "Books"},
	lineBand,
}

// lineBand is the dialogue row. It is named apart because it is the one
// group searchLibrary does not fill: the words are in an index, not in the
// works, so searchEnvelope adds it — under this heading, in this place.
var lineBand = band{Key: "lines", Title: "Dialogue"}

// searchGroup is one headed row of results: how many matched, and the first
// searchMax of them.
type searchGroup struct {
	Key   string            `json:"key"`
	Title string            `json:"title"`
	Count int               `json:"count"`
	Items []*hyper.Envelope `json:"items"`
}

// searchHref is one query's own address.
func searchHref(q string) string { return "/api/search?q=" + url.QueryEscape(q) }

// searchLibrary is the whole of the matching, and it is a pure function of
// the query and the library — no request, no store — so its table test
// drives it directly, the way resolveRoute's does.
//
// A work is matched on what is said about the TITLE (its own, its author's,
// its synopsis) and a member on what is said about the FILE (its own title,
// the label its work gives it, its synopsis, its author), so "beach" finds
// the episode and "karma" the track without either flooding the other's
// row. An ARTIST is matched on their NAME alone — their records answer for
// themselves a row above — and is answered as the very tile the library
// draws, so a hit opens the shelf. Case is folded once, here.
func searchLibrary(q string, ws []works.Work) []searchGroup {
	q = strings.ToLower(strings.TrimSpace(q))
	hits := map[string][]*hyper.Envelope{}
	count := map[string]int{}
	add := func(group string, en *hyper.Envelope) {
		count[group]++
		if len(hits[group]) < searchMax {
			hits[group] = append(hits[group], en)
		}
	}
	if q != "" {
		as := works.Artists(ws)
		for i := range as {
			if matchesQuery(q, as[i].Name) {
				add("artists", artistTile(&as[i]))
			}
		}
		for i := range ws {
			wk := &ws[i]
			// A book IS its file, so it is answered once, among the books;
			// every other work is a title of its own.
			if wk.Medium != model.MediumText &&
				matchesQuery(q, wk.Title, wk.Author, workOverview(wk)) {
				add("works", workTile(wk))
			}
			for _, it := range wk.Items {
				group := searchGroupOf(it)
				if group == "" {
					continue // a film, a featurette: its work speaks for it
				}
				if !matchesQuery(q, works.ItemTitle(it), works.ItemLabel(it),
					itemOverview(it), itemAuthor(it)) {
					continue
				}
				add(group, searchHit(it, wk))
			}
		}
	}
	out := make([]searchGroup, 0, len(searchBands))
	for _, b := range searchBands {
		if count[b.Key] == 0 {
			continue // a row with nothing in it is not a heading
		}
		out = append(out, searchGroup{Key: b.Key, Title: b.Title,
			Count: count[b.Key], Items: hits[b.Key]})
	}
	return out
}

// matchesQuery is the match itself: a substring of any of these, the query
// already folded.
func matchesQuery(q string, parts ...string) bool {
	for _, p := range parts {
		if p != "" && strings.Contains(strings.ToLower(p), q) {
			return true
		}
	}
	return false
}

// searchGroupOf is the row one member is answered in, or "" when it has no
// row of its own — a film and a featurette are found through their work.
func searchGroupOf(it store.Item) string {
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		return "books"
	}
	switch identityKind(it) {
	case "episode":
		return "episodes"
	case "track":
		return "tracks"
	case "audiobook_part":
		return "parts"
	}
	return ""
}

// searchHit is one member as a result: its own address, what to call it, the
// work it belongs to — and `item_id`, which is what `#/item/<id>` is spelled
// with. The work's title rides as the subtitle, which is also the title
// `#/show/<title>` takes, so a screen has every id a hash needs without
// composing an address.
func searchHit(it store.Item, wk *works.Work) *hyper.Envelope {
	title := works.ItemTitle(it)
	href := itemHref(it.ID)
	doc := hyper.Doc(href, "item", title).
		Field("item_id", it.ID).
		Field("label", works.ItemLabel(it))
	if wk != nil {
		doc.Field("work_key", wk.Key)
		// A book IS its work, so its work's title under its own would say the
		// same thing twice; every other member is one part of something.
		if wk.Title != title {
			doc.Field("subtitle", wk.Title)
		}
	}
	doc.Link("self", href, title)
	if wk != nil {
		doc.Link("work", workHref(wk.Key), wk.Title)
	}
	// The row's picture is the one the resume shelf draws for the same file:
	// its own still or cover, and its work's when this file has none.
	if art := continueArtwork(it, wk); art != "" {
		doc.Link("artwork", art, "")
	}
	return doc
}

// workOverview is what the WORK is about, in TMDB's words for the title —
// never one episode's synopsis, which belongs to the episode.
func workOverview(wk *works.Work) string {
	if rep := memberByID(wk, wk.RepresentativeItemID); rep != nil && rep.Enrichment != nil {
		return rep.Enrichment.Overview
	}
	return ""
}

// itemAuthor is who the path said made this one file: a book's author, a
// record's artist.
func itemAuthor(it store.Item) string {
	if it.Identity == nil {
		return ""
	}
	return it.Identity.Author
}

// ── the dialogue ────────────────────────────────────────────────────────
//
// What was SAID, which no title carries: the transcription stage parses
// every generated WebVTT into cues (store.SetCues) and this is the read.
// A hit is one line — the words, the file that says them, and the seconds
// they are said between — answered as the same member envelope a search
// result is, plus a PASSAGE: the scene around the line, which the client
// spells as `#/item/<id>?t=…&end=…` and the player then plays and stops.
//
// cueLead is how much of the scene comes with the line, either side of it.
// A line landed on exactly starts mid-breath; two seconds is the run-up a
// person needs to hear what is being answered.
const cueLead = 2.0

// linesHref is one file's dialogue search, as an address.
func linesHref(id int64) string { return itemHref(id) + "/lines" }

// lineHits is the cues as documents, against the works this profile may
// see: a cue whose file is not in them (a kid's shelf, a deleted item) is
// not a result, which is how the dialogue keeps the same fences as the
// shelves.
func lineHits(cues []store.Cue, ws []works.Work) []*hyper.Envelope {
	byItem := works.ByItem(ws)
	out := make([]*hyper.Envelope, 0, len(cues))
	for _, c := range cues {
		it := itemByID(ws, c.ItemID)
		if it == nil {
			continue
		}
		doc := searchHit(*it, byItem[c.ItemID])
		doc.Field("start", c.Start).
			Field("end", c.End).
			Field("text", c.Text).
			// The scene, not the instant: `t` and `end` are what the hash
			// carries and what the player stops at.
			Field("passage", map[string]float64{
				"t": math.Max(0, c.Start-cueLead), "end": c.End + cueLead,
			})
		out = append(out, doc)
	}
	return out
}

// searchLines is the dialogue half of a search: the whole library when
// itemID is 0, one file's own lines otherwise. A library that cannot be
// asked (a bare test server) has no dialogue, which is an answer.
func (s *server) searchLines(q string, itemID int64) ([]store.Cue, int) {
	if s.library == nil || strings.TrimSpace(q) == "" {
		return nil, 0
	}
	cues, total, err := s.library.SearchCues(q, itemID, searchMax)
	if err != nil {
		log.Printf("dialogue search %q: %v", q, err)
		return nil, 0
	}
	return cues, total
}

// hasLines says whether one item has dialogue to search — what decides
// whether its document offers the finder at all.
func (s *server) hasLines(id int64) bool {
	if s.library == nil {
		return false
	}
	n, err := s.library.CueCount(id)
	if err != nil {
		log.Printf("cue count for item %d: %v", id, err)
		return false
	}
	return n > 0
}

// searchEnvelope is the document, apart from the request that asked for it:
// the route resolver answers a #/search/<q> hash with the very same one.
func (s *server) searchEnvelope(q string, ws []works.Work) *hyper.Envelope {
	q = strings.TrimSpace(q)
	groups := searchLibrary(q, ws)
	if cues, total := s.searchLines(q, 0); total > 0 {
		if items := lineHits(cues, ws); len(items) > 0 {
			groups = append(groups, searchGroup{Key: lineBand.Key,
				Title: lineBand.Title, Count: total, Items: items})
		}
	}
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	return hyper.Doc(searchHref(q), "search", "Search").
		Field("query", q).
		Field("count", n).
		Field("groups", groups).
		Link("root", "/api/", "flickr").
		Link("library", "/api/library", "Library")
}

// handleSearch is GET /api/search?q=… — the groups, in their order. An empty
// query is not a refusal: it is a search that has found nothing yet, which
// is what a cleared box means.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ws, err := s.worksFor(r)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	hyper.WriteDoc(w, http.StatusOK, s.searchEnvelope(r.URL.Query().Get("q"), ws))
}

// handleItemLines is GET /api/items/{id}/lines?q=… — the same hits, inside
// one file, for the finder on its page. It is the item's own `lines`
// relation; a file with no transcript never offers it, and asking anyway is
// an empty answer rather than a refusal.
func (s *server) handleItemLines(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("follow an item's link rather than composing its address",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	ws, err := s.worksFor(r)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cues, total := s.searchLines(q, id)
	items := lineHits(cues, ws)
	doc := hyper.Doc(linesHref(id)+"?q="+url.QueryEscape(q), "lines", "Find in dialogue").
		Field("query", q).
		Field("count", total).
		Field("items", items).
		Link("item", itemHref(id), "")
	hyper.WriteDoc(w, http.StatusOK, doc)
}
