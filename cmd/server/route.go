package main

// The route document — docs/hypermedia.md, step 2 of its order of work:
// GET /api/-/route?hash=<hash> answers what a hash MEANS. The hash stays
// human ("#/show/The%20Office?ep=S03E22&t=142&end=854", README "Deep links
// and passages") and every link minted so far keeps working, but the browser
// no longer reads it: it splits the hash off the URL, asks here, and renders
// the answer.
//
//	{ "self", "kind": "route",
//	  "view": "library" | "work" | "artist" | "item" | "search",
//	  "document": <the envelope to render>,
//	  "passage": <resolved, or null>,
//	  "autoplay": <does arriving start it> }
//
// The resolution is the part that used to be manners: an episode code
// against the show's members, a text locator against the book's spine, an
// unknown title into a refusal with a remedy. resolveRoute below is a pure
// function of the hash and the library — no request, no store — so its table
// test drives it directly and this handler stays the thin wrapper it is.

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/passage"
	"flickr/internal/store"
	"flickr/internal/works"
)

// resolvedPassage is a passage with every spelling resolved to something the
// player and the reader can act on without knowing the grammar: item ids,
// and the spine section a text locator lands in. `ep` and an episode-code
// `until` never reach here — they are what the resolution consumed.
type resolvedPassage struct {
	T           *float64 `json:"t,omitempty"`
	End         *float64 `json:"end,omitempty"`
	Until       *int64   `json:"until,omitempty"`
	From        string   `json:"from,omitempty"`
	To          string   `json:"to,omitempty"`
	FromSection int      `json:"from_section,omitempty"`
	ToSection   int      `json:"to_section,omitempty"`
	FromPage    int      `json:"from_page,omitempty"`
	ToPage      int      `json:"to_page,omitempty"`
}

// routeTarget is what a hash names, before any document is built: the pure
// resolver's whole answer. Exactly one of Work, Artist, Item and Query is
// set, unless Problem is — a refusal names nothing.
type routeTarget struct {
	View     string // "library" | "work" | "artist" | "item" | "search"
	Work     *works.Work
	Artist   string
	Item     *store.Item
	Query    string
	Passage  *resolvedPassage
	Autoplay bool
	Problem  *hyper.Problem
}

var itemPathRe = regexp.MustCompile(`^#/item/(\d+)$`)

// resolveRoute reads one hash against the library. Every spelling README
// "Deep links and passages" documents is answered here; anything else is the
// library, which is what the browser's own router did with an address it did
// not know.
func resolveRoute(hash string, ws []works.Work) routeTarget {
	path, query := passage.Split(hash)
	p := passage.Parse(query)

	switch {
	case strings.HasPrefix(path, "#/show/"):
		title, err := url.PathUnescape(strings.TrimPrefix(path, "#/show/"))
		if err != nil {
			return badHash(hash, err)
		}
		wk := showByTitle(ws, title)
		if wk == nil {
			return refuse(hyper.Refuse(http.StatusNotFound, "no-such-show",
				"No such show", fmt.Sprintf("the library holds no show called %q", title)).
				WithRemedy("open the library and follow a show from there",
					&hyper.Link{Href: "/api/library", Title: "Library"}))
		}
		if p == nil || (p.Ep == "" && p.UntilEp == "") {
			// A bare show route is the episode list; a query that says nothing
			// about episodes says nothing this view can act on.
			return routeTarget{View: "work", Work: wk}
		}
		// The show form: resolve the episode code against the show's members
		// and hand back the item route it means. The browser replaces the hash
		// with that item link — the show link was the minter's spelling.
		member, resolved, err := passage.ResolveShow(p, membersOf(wk))
		if err != nil {
			return refuse(hyper.Refuse(http.StatusNotFound, "no-such-episode",
				"No such episode", err.Error()).
				WithRemedy("the show's own page lists the episodes it has",
					&hyper.Link{Href: workHref(wk.Key), Title: wk.Title}))
		}
		it := memberByID(wk, member.ID)
		return itemTarget(it, resolved)

	case strings.HasPrefix(path, "#/artist/"):
		name, err := url.PathUnescape(strings.TrimPrefix(path, "#/artist/"))
		if err != nil {
			return badHash(hash, err)
		}
		artist := artistByName(ws, name)
		if artist == "" {
			return refuse(hyper.Refuse(http.StatusNotFound, "no-such-artist",
				"No such artist", fmt.Sprintf("the library holds no artist called %q", name)).
				WithRemedy("open the library and follow an artist from there",
					&hyper.Link{Href: "/api/library", Title: "Library"}))
		}
		return routeTarget{View: "artist", Artist: artist}

	case strings.HasPrefix(path, "#/search/"):
		// A search is a place, not a gesture: the words are in the address,
		// so a result page can be sent to somebody. A query nothing matches
		// is not a refusal — it is an answer with empty hands.
		q, err := url.PathUnescape(strings.TrimPrefix(path, "#/search/"))
		if err != nil {
			return badHash(hash, err)
		}
		return routeTarget{View: "search", Query: q}

	case itemPathRe.MatchString(path):
		id, _ := strconv.ParseInt(itemPathRe.FindStringSubmatch(path)[1], 10, 64)
		it := itemByID(ws, id)
		if it == nil {
			return refuse(hyper.Refuse(http.StatusNotFound, "no-such-item",
				"No such item", fmt.Sprintf("the library holds no item %d", id)).
				WithRemedy("scan the library, or open it and follow an item from there",
					&hyper.Link{Href: "/api/library", Title: "Library"}))
		}
		return itemTarget(it, p)
	}
	return routeTarget{View: "library"}
}

// itemTarget is the item view with its passage resolved against that item —
// a book's from/to also carrying the spine section each locator lands in, so
// the reader is handed a place rather than a spelling.
func itemTarget(it *store.Item, p *passage.Passage) routeTarget {
	t := routeTarget{View: "item", Item: it}
	if p == nil {
		return t
	}
	if !p.IsTimed() && !p.IsText() {
		return t // the query named only an episode, and that is now the item
	}
	rp := &resolvedPassage{T: p.T, End: p.End, Until: p.Until, From: p.From, To: p.To}
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		from, to := passage.ParseLocator(p.From), passage.ParseLocator(p.To)
		if n := sectionCount(it); n > 0 {
			rp.FromSection, rp.ToSection = from.Section(n), to.Section(n)
		}
		// A PDF counts in pages where an EPUB counts in sections, and the
		// reading session is opened from this answer: the route resolves
		// whichever unit this book has (read.go does the same for its own
		// passage, since a session can be opened without a route).
		if n := pageCount(*it); n > 0 {
			rp.FromPage, rp.ToPage = from.Page(n), to.Page(n)
		}
	}
	t.Passage = rp
	// A passage route is not browsed to, it is arrived at: the player starts
	// at `t` and the reader opens at `from`, without a second gesture.
	t.Autoplay = true
	return t
}

func refuse(p hyper.Problem) routeTarget {
	return routeTarget{View: "library", Problem: &p}
}

func badHash(hash string, err error) routeTarget {
	return refuse(hyper.Refuse(http.StatusBadRequest, "bad-hash",
		"That is not an address this library can read",
		fmt.Sprintf("%q could not be decoded: %v", hash, err)).
		WithRemedy("follow a link rather than composing an address",
			&hyper.Link{Href: "/api/library", Title: "Library"}))
}

// showByTitle finds the show a "#/show/<title>" hash names. The links out
// there were minted from the title the show's own FILES spell, which is not
// always the title TMDB gives the work, so both are matched — case
// insensitively, the way the projection groups episodes in the first place.
func showByTitle(ws []works.Work, title string) *works.Work {
	if title == "" {
		return nil
	}
	for i := range ws {
		wk := &ws[i]
		if wk.Kind != "show" {
			continue
		}
		if strings.EqualFold(wk.Title, title) {
			return wk
		}
		for _, it := range wk.Items {
			if it.Identity != nil && strings.EqualFold(it.Identity.Title, title) {
				return wk
			}
		}
	}
	return nil
}

// artistByName is the artist's own spelling of a name matched case
// insensitively, or "" when nobody in the library goes by it.
func artistByName(ws []works.Work, name string) string {
	if name == "" {
		return ""
	}
	for _, a := range works.Artists(ws) {
		if strings.EqualFold(a.Name, name) {
			return a.Name
		}
	}
	return ""
}

func itemByID(ws []works.Work, id int64) *store.Item {
	for i := range ws {
		for j := range ws[i].Items {
			if ws[i].Items[j].ID == id {
				return &ws[i].Items[j]
			}
		}
	}
	return nil
}

// membersOf is a show's members as the grammar reads them: ids and the
// numbers their identity carries, nothing else.
func membersOf(wk *works.Work) []passage.Member {
	out := make([]passage.Member, 0, len(wk.Items))
	for _, it := range wk.Items {
		m := passage.Member{ID: it.ID}
		if it.Identity != nil {
			m.Kind, m.Season, m.Episode = it.Identity.Kind, it.Identity.Season, it.Identity.Episode
		}
		out = append(out, m)
	}
	return out
}

// sectionCount is how many spine sections a text item has: what the probe
// counted, or the section list itself for a book probed before that count
// existed.
func sectionCount(it *store.Item) int {
	if it.MediaInfo == nil {
		return 0
	}
	if it.MediaInfo.Sections > 0 {
		return it.MediaInfo.Sections
	}
	return len(it.MediaInfo.Chapters)
}

// libraryEnvelope is the library view's answer: the shelf's address and a
// link to it, not the shelf itself. GET /api/library serves the whole grid
// now (hyper 3, library.go), and the client keeps ONE copy of it rather than
// a fresh one on every route — so what a route answer owes the library view
// is where to look, which is this. Inlining the document is hyper 6's, when
// the kernel renders every view from the answer alone.
func libraryEnvelope() *hyper.Envelope {
	return hyper.Doc("/api/library", "library", "Library").
		Link("library", "/api/library", "Library")
}

// artistEnvelope is the same for GET /api/artists/{name}: the shelf is
// served, and the answer names it.
func artistEnvelope(name string) *hyper.Envelope {
	href := artistHref(name)
	return hyper.Doc(href, "artist", name).Link("artist", href, name)
}

// handleRouteDoc is GET /api/-/route?hash=<hash>.
func (s *server) handleRouteDoc(w http.ResponseWriter, r *http.Request) {
	hash := r.URL.Query().Get("hash")
	ws, err := s.worksFor(r)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	target := resolveRoute(hash, ws)
	if target.Problem != nil {
		hyper.WriteProblem(w, *target.Problem)
		return
	}
	profile := profileOf(r)
	positions, err := s.positionsFor(profile)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	saved, err := s.savedSet(profile)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	var doc *hyper.Envelope
	switch target.View {
	case "work":
		doc = s.workEnvelope(target.Work, ws, profile, positions, saved)
	case "artist":
		doc = artistEnvelope(target.Artist)
	case "item":
		doc = s.itemEnvelope(*target.Item, works.ByItem(ws)[target.Item.ID], true, positions)
	case "search":
		doc = s.searchEnvelope(target.Query, ws)
	default:
		doc = libraryEnvelope()
	}
	hyper.WriteDoc(w, http.StatusOK,
		hyper.Doc("/api/-/route?hash="+url.QueryEscape(hash), "route", "").
			Field("view", target.View).
			Field("document", doc).
			Field("passage", target.Passage).
			Field("autoplay", target.Autoplay))
}
