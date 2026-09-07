package main

// The hypermedia documents — docs/hypermedia.md, step 1 of its order of
// work: the root, one item and one work. The server says what exists and
// what may be done to it; the browser renders what it is told.
//
// These three sit BESIDE the existing routes, which are untouched: nothing
// here changes a JSON shape anything already reads. Every relation they
// publish now names a document that is served — /api/library came with
// hyper 3 (library.go), and /api/-/passage with hyper 4 (passage.go), in
// both its forms: an item and two times, and the work-and-two-places form
// the work document's action points at.

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// ── addresses ───────────────────────────────────────────────────────────

func itemHref(id int64) string { return "/api/items/" + strconv.FormatInt(id, 10) }

// workHref is a work key's own address. url.PathEscape leaves ":" alone (it
// is legal in a path segment) and every key carries one — "show:the-office",
// "tmdb:2316" — so the colon is escaped by hand: it is the spelling
// docs/hypermedia.md uses, and both spellings decode to the same segment, so
// the route reads either.
func workHref(key string) string {
	return "/api/works/" + strings.ReplaceAll(url.PathEscape(key), ":", "%3A")
}

// profileOf is who is asking: the client_id the app already sends as a query
// parameter (it is the profile NAME — playback state is keyed by it), or a
// cookie of the same name for a caller that would rather not spell it on
// every URL. "" when nobody said.
func profileOf(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("client_id")); v != "" {
		return v
	}
	if c, err := r.Cookie("client_id"); err == nil {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

// ── the root ────────────────────────────────────────────────────────────

// handleRootDoc is GET /api/ — the one address the client knows by heart.
// Everything else it reaches by following a relation from here.
func (s *server) handleRootDoc(w http.ResponseWriter, r *http.Request) {
	profile := profileOf(r)
	doc := hyper.Doc("/api/", "root", "flickr")
	if profile != "" {
		doc.Field("profile", profile)
	}
	// The resume list is per profile and refuses without one; when we know
	// who is asking, the link carries it, so following the relation is the
	// whole of what the client has to do.
	cont := "/api/continue"
	if profile != "" {
		cont += "?client_id=" + url.QueryEscape(profile)
	}
	doc.
		// The shelf every other document's remedy points back to.
		Link("library", "/api/library", "Library").
		Link("continue", cont, "Continue watching").
		Link("artists", "/api/artists", "Artists").
		Link("works", "/api/works", "Works").
		Link("items", "/api/items", "Items").
		Link("scan", "/api/scan", "Scan status").
		Link("system", "/api/system", "System").
		Link("profiles", "/api/users", "Profiles").
		Action("scan", hyper.Action{
			Method: "POST", Href: "/api/scan", Label: "Scan the library",
		}).
		// The client knows /api/ by heart and nothing else, so the address
		// that turns a hash into a document has to be named here: without it
		// the one URL the kernel composes would be two.
		Action("route", hyper.Action{
			Method: "GET", Href: "/api/-/route",
			Input: map[string]string{"hash": "string"},
			Label: "Open an address",
		}).
		Action("create_profile", hyper.Action{
			Method: "POST", Href: "/api/users",
			Input: map[string]string{"name": "string"},
			Label: "Create a profile",
		})
	hyper.WriteDoc(w, http.StatusOK, doc)
}

// ── one item ────────────────────────────────────────────────────────────

// handleItemDoc is GET /api/items/{id} — the route the API never had. Until
// now the only read that carried an item's media_info was the work's flat
// member list, which is why the waymark side had to fetch a whole work to
// learn one item's chapters.
func (s *server) handleItemDoc(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("follow an item's link rather than composing its address",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	if item == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-item",
			"No such item", fmt.Sprintf("the library holds no item %d", id)).
			WithRemedy("scan the library, or open it and follow an item from there",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	positions, err := s.positionsFor(profileOf(r))
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	hyper.WriteDoc(w, http.StatusOK, s.itemEnvelope(*item, works.ByItem(ws)[id], true, positions))
}

// subtitle is one subtitle track as a document lists it: what the probe
// found, plus the address of its WebVTT. A bitmap track (PGS, VobSub) has no
// href — it cannot be converted, and the route refuses it — so a screen that
// only offers what carries an href is right by construction.
type subtitle struct {
	Ordinal   int    `json:"ordinal"`
	Language  string `json:"language,omitempty"`
	Title     string `json:"title,omitempty"`
	Codec     string `json:"codec"`
	Supported bool   `json:"supported"`
	External  bool   `json:"external,omitempty"`
	Href      string `json:"href,omitempty"`
}

func subtitlesOf(it store.Item) []subtitle {
	if it.MediaInfo == nil || len(it.MediaInfo.Subtitles) == 0 {
		return nil
	}
	out := make([]subtitle, 0, len(it.MediaInfo.Subtitles))
	for _, tr := range it.MediaInfo.Subtitles {
		s := subtitle{
			Ordinal: tr.Ordinal, Language: tr.Language, Title: tr.Title,
			Codec: tr.Codec, Supported: tr.Supported, External: tr.External,
		}
		if tr.Supported {
			s.Href = fmt.Sprintf("%s/subtitles/%d.vtt", itemHref(it.ID), tr.Ordinal)
		}
		out = append(out, s)
	}
	return out
}

// itemEnvelope is one item as a document.
//
// full says whether this is the item's OWN document (GET /api/items/{id}) or
// a member inside a work's. A member carries what a list needs — its place
// in the work, its chapters or sections, its artwork, and the actions a
// person takes on a row — while the full document adds media_info, the
// subtitle tracks, the neighbours either side of it, and the three
// curatorial actions (identity, reprobe, enrich) that belong on one item's
// own page and would be noise repeated down a twenty-episode list.
func (s *server) itemEnvelope(it store.Item, wk *works.Work, full bool, positions map[int64]store.Position) *hyper.Envelope {
	medium := it.MediaInfo.MediumOrVideo() // nil media info reads as video
	base := itemHref(it.ID)
	doc := hyper.Doc(base, "item", itemTitle(it)).
		Field("id", it.ID)
	if full {
		// The whole key, and the name at the end of it: no title is a file
		// name any more, so the name a person searched their shelf for — or
		// typed at an ffprobe — is said here, once, on the item's own page.
		doc.Field("object_key", it.ObjectKey).
			Field("file_name", path.Base(it.ObjectKey))
	}
	doc.Field("medium", medium).
		Field("label", works.ItemLabel(it))
	// Bonus material is a member of its work but not a step in it: the flag
	// is what a list needs to keep it out of the run and put it under its own
	// heading, and it used to be read off identity.kind in the browser.
	if identityKind(it) == "extra" {
		doc.Field("extra", true)
	}
	if it.MediaInfo != nil && it.MediaInfo.DurationSeconds > 0 {
		doc.Field("duration_seconds", it.MediaInfo.DurationSeconds)
	}
	// The two lines a screen writes under a title, decided here rather than
	// composed from four half-fields in the browser: `tech` is what the FILE
	// is (the same phrase a tile carries, for this one member), `overview` is
	// what the thing is ABOUT — the episode's own synopsis where there is
	// one, the film's or the book's otherwise. `year` is the parenthesis a
	// title takes, and an episode or a featurette takes none.
	doc.Field("tech", techLine(it))
	if ov := itemOverview(it); ov != "" {
		doc.Field("overview", ov)
	}
	if y := itemYear(it); y != 0 {
		doc.Field("year", y)
	}
	if full {
		if it.Identity != nil {
			doc.Field("identity", it.Identity)
		}
		if it.Enrichment != nil {
			doc.Field("enrichment", it.Enrichment)
		}
		if it.MediaInfo != nil {
			doc.Field("media_info", it.MediaInfo)
		}
		if subs := subtitlesOf(it); len(subs) > 0 {
			doc.Field("subtitles", subs)
		}
		if it.ProbeError != "" {
			doc.Field("probe_error", it.ProbeError)
		}
	} else if mi := it.MediaInfo; mi != nil {
		// A member says where its own places are without the whole probe:
		// chapter marks for a clock, the spine length or the page count for
		// a text (a book has no clock, so that is all a section can be).
		if len(mi.Chapters) > 0 {
			doc.Field("chapters", mi.Chapters)
		}
		if mi.Sections > 0 {
			doc.Field("sections", mi.Sections)
		}
		if mi.PageCount > 0 {
			doc.Field("page_count", mi.PageCount)
		}
	}
	// Where this profile left off, on the item's own document. The browser
	// used to ask GET /api/progress for it by hand; the place belongs to the
	// thing it is a place in, and a client that follows a link to an item is
	// told where it stands in the same breath.
	if r := resumeOf(it, positions); r != nil {
		doc.Field("resume", r)
	}

	if wk != nil {
		doc.Link("work", workHref(wk.Key), wk.Title)
		if full {
			// The neighbours are the work's own order — the one "up next"
			// walks, bonus material aside.
			if prev, next := works.Neighbours(wk, it.ID); prev != nil || next != nil {
				if prev != nil {
					doc.Link("prev", itemHref(prev.ID), itemTitle(*prev))
				}
				if next != nil {
					doc.Link("next", itemHref(next.ID), itemTitle(*next))
				}
			}
		}
	}
	artworkLinks(doc, it, medium)

	if medium == model.MediumText {
		// A book is READ through a session, the way a film is played through
		// one (read.go): the action opens the sitting, and the bytes are
		// `links.book` on the document it answers.
		doc.Action("read", hyper.Action{
			Method: "POST", Href: base + "/read",
			Input: readInputSketch(), Label: "Read",
		})
		doc.Unavailable("play", "a book is read, not played")
	} else {
		if it.MediaInfo == nil {
			doc.Unavailable("play", unprobed(it))
		} else {
			doc.Action("play", hyper.Action{
				Method: "POST", Href: base + "/play",
				Input: playInput(), Label: "▶ Play",
			})
		}
		doc.Unavailable("read", notReadable(identityKind(it)))
	}
	doc.Action("progress", hyper.Action{
		Method: "POST", Href: "/api/progress",
		Input: progressInputSketch(it), Label: "Save the place",
	})

	if full {
		doc.Action("identity", hyper.Action{
			Method: "POST", Href: base + "/identity",
			Input: identityInputSketch(), Label: "Fix the identity",
		})
		doc.Action("reprobe", hyper.Action{
			Method: "POST", Href: base + "/reprobe", Label: "Probe this file again",
		})
		switch {
		case s.enricher == nil:
			doc.Unavailable("enrich", "TMDB enrichment is off (TMDB_API_KEY is not set)")
		case !enrichable(it):
			doc.Unavailable("enrich", enrichRefusal(it))
		default:
			doc.Action("enrich", hyper.Action{
				Method: "POST", Href: base + "/enrich", Label: "Look this up on TMDB",
			})
		}
	}
	return doc
}

// resume is where a profile left off in one item: the clock for anything
// with one, the locator for a book. Absent when there is no place worth
// coming back to — under five seconds is a misclick, and the last ten
// seconds of a film are the credits, not a place.
type resume struct {
	PositionSeconds float64        `json:"position_seconds,omitempty"`
	Locator         *model.Locator `json:"locator,omitempty"`
}

func resumeOf(it store.Item, positions map[int64]store.Position) *resume {
	p, ok := positions[it.ID]
	if !ok {
		return nil
	}
	if p.Locator != nil {
		if p.Locator.Fraction <= 0 && p.Locator.Section <= 1 && p.Locator.Page <= 1 {
			return nil
		}
		return &resume{PositionSeconds: p.PositionSeconds, Locator: p.Locator}
	}
	if p.PositionSeconds <= 5 {
		return nil
	}
	if it.MediaInfo != nil && it.MediaInfo.DurationSeconds > 0 &&
		p.PositionSeconds >= it.MediaInfo.DurationSeconds-10 {
		return nil
	}
	return &resume{PositionSeconds: p.PositionSeconds}
}

// itemOverview is what this one file is about, in the words whoever wrote
// them: an episode's own synopsis, and the work's for everything else.
func itemOverview(it store.Item) string {
	e := it.Enrichment
	if e == nil {
		return ""
	}
	if e.EpisodeOverview != "" {
		return e.EpisodeOverview
	}
	return e.Overview
}

// itemYear is the year a title is shown with. An episode's year is the
// show's and is said on the show's page; a featurette has none of its own.
func itemYear(it store.Item) int {
	switch identityKind(it) {
	case "episode", "extra":
		return 0
	}
	if it.Enrichment != nil && it.Enrichment.Year != 0 {
		return it.Enrichment.Year
	}
	if it.Identity != nil {
		return it.Identity.Year
	}
	return 0
}

// artworkLinks names every picture and every byte-stream this item has, by
// relation: a TMDB poster and episode still when the enrichment fetched
// them, the item's OWN cover for audio and text (extracted on first ask and
// falling back to the poster route), the book itself for a reader, and the
// scrub-preview index for a video long enough to have been given one.
func artworkLinks(doc *hyper.Envelope, it store.Item, medium string) {
	base := itemHref(it.ID)
	if it.Enrichment != nil && it.Enrichment.HasPoster {
		doc.Link("poster", base+"/poster", "")
	}
	if it.Enrichment != nil && it.Enrichment.HasStill {
		doc.Link("still", base+"/still", "")
	}
	switch medium {
	case model.MediumAudio, model.MediumText:
		doc.Link("cover", base+"/cover", "")
	}
	if medium == model.MediumText {
		doc.Link("book", base+"/book", "")
	}
	if medium == model.MediumVideo && it.MediaInfo != nil &&
		it.MediaInfo.DurationSeconds > trickplayMinSeconds {
		doc.Link("trickplay", base+"/trickplay.json", "")
	}
}

// itemTitle is what a screen calls this one file: the episode's own title
// when TMDB knows it, a track's own name, the film's or the book's title —
// and never the file's name, which reaches the document as `file_name`
// instead. The rule is works.ItemTitle's, beside the label rule it shares
// its fallback with; this document carries both.
func itemTitle(it store.Item) string { return works.ItemTitle(it) }

// playInput is the sketch every play action publishes. `passage` is the
// object play takes (t, end, until, from, to — passage.go): the server
// resolves it onto the session and owns its rules from there.
func playInput() map[string]string {
	return map[string]string{"seek_seconds": "number?", "passage": "passage?"}
}

// progressInputSketch pre-fills the item id: /api/progress takes the item in
// its body, and a document that knows which item it is should not make the
// caller work that out again. The rest is the unit the medium counts in.
func progressInputSketch(it store.Item) map[string]string {
	in := map[string]string{
		"item_id":   strconv.FormatInt(it.ID, 10),
		"client_id": "string",
	}
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		in["position_seconds"] = "number?"
		in["locator"] = "string?"
		in["fraction"] = "number?"
		in["section"] = "number?"
		in["page"] = "number?"
		return in
	}
	in["position_seconds"] = "number"
	return in
}

func identityInputSketch() map[string]string {
	return map[string]string{
		"kind": "string", "title": "string", "author": "string?",
		"year": "number?", "season": "number?", "episode": "number?",
		"part": "number?", "track_title": "string?",
	}
}

// notReadable is why `read` is not on offer, in the words of what this is —
// the kind's word, not the medium's, so an album is not told it is an
// audiobook. It takes either an identity kind (an item) or a work kind.
func notReadable(kind string) string {
	switch kind {
	case "audiobook", "audiobook_part":
		return "an audiobook is played, not read"
	case "album", "track":
		return "an album is played, not read"
	case "show", "episode":
		return "an episode is played, not read"
	}
	return "a film is played, not read"
}

// identityKind is what the path said this file is, or "" when it said
// nothing.
func identityKind(it store.Item) string {
	if it.Identity == nil {
		return ""
	}
	return it.Identity.Kind
}

// unprobed is why `play` is not on offer when there is no media info: the
// decision engine has nothing to decide about. `reprobe` is still offered,
// which is the remedy.
func unprobed(it store.Item) string {
	if it.ProbeError != "" {
		return "the probe could not read this file, so there is nothing to play: " + it.ProbeError
	}
	return "this file has not been probed yet, so there is nothing to play"
}

// enrichable mirrors the enricher's own kind filter: TMDB is asked about
// films and shows, and nothing else in the library has an answer there.
func enrichable(it store.Item) bool {
	return it.Identity != nil && (it.Identity.Kind == "movie" || it.Identity.Kind == "episode")
}

func enrichRefusal(it store.Item) string {
	kind := "unidentified file"
	if it.Identity != nil && it.Identity.Kind != "" {
		kind = strings.ReplaceAll(it.Identity.Kind, "_", " ")
	}
	return fmt.Sprintf("TMDB is asked about films and shows; this is %s %s", article(kind), kind)
}

func article(noun string) string {
	if noun == "" {
		return "a"
	}
	if strings.ContainsRune("aeiou", rune(noun[0])) {
		return "an"
	}
	return "a"
}

// ── one work ────────────────────────────────────────────────────────────

// place is one place a work offers a passage: the TOKEN a passage's from/to
// takes, spelled in the passage grammar, and the LABEL a chip shows in its
// stead. The two are separate on purpose — a token must read back through
// the grammar, and "1:19:00 — The Cellar" would not.
//
// The spellings are the grammar's own (README "Deep links and passages", and
// dayplan10.passage on the waymark side, which reads these): a show's
// episodes are "S02E05 0:00", the first second of each; a film's, an
// audiobook's and an album's chapter marks are times, "1:19:00" past the
// hour and "4:30" under it; a book's sections are "ch. 7", counting the
// spine the way flickr's own ch:<n> locator counts it.
type place struct {
	Token string `json:"token"`
	Label string `json:"label"`
}

// placesOf is every place the work offers, in the work's own order,
// duplicate tokens dropped. Empty for a work whose medium the grammar has no
// words for, and for a PDF — whose places are its pages, and four hundred of
// them are not a picker.
func placesOf(wk *works.Work) []place {
	out := []place{}
	seen := map[string]bool{}
	add := func(token, label string) {
		if token == "" || seen[token] {
			return
		}
		seen[token] = true
		out = append(out, place{Token: token, Label: label})
	}

	switch wk.Kind {
	case "show":
		for _, it := range wk.Items {
			id := it.Identity
			if id == nil || id.Kind != "episode" || id.Episode == 0 {
				continue // bonus material, and episodes nobody numbered
			}
			add(fmt.Sprintf("S%02dE%02d 0:00", id.Season, id.Episode), works.ItemLabel(it))
		}
	case "movie", "audiobook", "album":
		if it := chapterSource(wk); it != nil {
			for i, ch := range it.MediaInfo.Chapters {
				label := ch.Title
				if label == "" {
					label = fmt.Sprintf("Chapter %02d", i+1)
				}
				add(works.Clock(ch.StartSeconds), label)
			}
		}
	case "book":
		// A text item's chapters ARE its spine: one entry per reading-order
		// document, its title the section's TOC label.
		if it := chapterSource(wk); it != nil {
			for i, ch := range it.MediaInfo.Chapters {
				label := ch.Title
				if label == "" {
					label = fmt.Sprintf("Section %d", i+1)
				}
				add(fmt.Sprintf("ch. %d", i+1), label)
			}
			break
		}
		// Probed before the section list existed: the spine length alone
		// still says how many places there are.
		for i := 1; i <= sectionsOf(wk); i++ {
			add(fmt.Sprintf("ch. %d", i), fmt.Sprintf("Section %d", i))
		}
	}
	return out
}

// chapterSource is the member whose chapter marks stand for the work's: the
// first real member that has any. A multi-file work counts its time inside
// one file, so a work's time tokens are that file's — which for a film, a
// one-sitting audiobook or a book is the only file there is.
func chapterSource(wk *works.Work) *store.Item {
	for i := range wk.Items {
		it := &wk.Items[i]
		if identityKind(*it) == "extra" {
			continue
		}
		if it.MediaInfo != nil && len(it.MediaInfo.Chapters) > 0 {
			return it
		}
	}
	return nil
}

func sectionsOf(wk *works.Work) int {
	for _, it := range wk.Items {
		if it.MediaInfo != nil && it.MediaInfo.Sections > 0 {
			return it.MediaInfo.Sections
		}
	}
	return 0
}

// progress is one profile's standing in the work, as the document says it:
// the fraction, the words, and where they would pick up.
type progress struct {
	Status   string      `json:"status"`
	Fraction float64     `json:"fraction"`
	Text     string      `json:"text,omitempty"`
	Next     *hyper.Link `json:"next,omitempty"`
}

// handleWorkDoc is GET /api/works/{key} — the work as a document: its own
// fields, its members as item envelopes, the places it offers a passage, and
// the asking profile's standing in it.
func (s *server) handleWorkDoc(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	wk := works.ByKey(ws)[key]
	if wk == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-work",
			"No such work", fmt.Sprintf("the library holds no work keyed %q", key)).
			WithRemedy("open the library and follow a work from there",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	profile := profileOf(r)
	positions, err := s.positionsFor(profile)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	hyper.WriteDoc(w, http.StatusOK, s.workEnvelope(wk, profile, positions))
}

// workEnvelope is that document, apart from the request that asked for it:
// the route resolver (route.go) answers a #/show/<title> hash with the very
// same document, and one work is one document however it was reached.
func (s *server) workEnvelope(wk *works.Work, profile string, positions map[int64]store.Position) *hyper.Envelope {
	doc := hyper.Doc(workHref(wk.Key), "work", wk.Title)
	// The work's own kind ("show", "album", "book") is not the DOCUMENT's
	// kind, which is always "work". The envelope's name wins, so the work's
	// goes in front of its other fields under one of its own; everything
	// else works.Work publishes is embedded as it stands, so a field added
	// there appears here without this handler being touched.
	doc.Field("work_kind", wk.Kind).Embed(wk)
	if profile != "" {
		doc.Field("profile", profile)
	}
	if pr, ok := works.WorkProgress(wk, positions); ok {
		p := progress{Status: pr.Status, Fraction: pr.Fraction, Text: pr.ProgressText}
		// Where they would pick up. Only a work with more than one member to
		// walk has one: a film or a book is where it is, and naming it as
		// its own next member would say nothing.
		if next := nextUnfinished(wk, positions); next != nil && walkedMembers(wk) > 1 {
			p.Next = &hyper.Link{Href: itemHref(next.ID), Title: works.ItemLabel(*next)}
		}
		doc.Field("progress", p)
	}
	doc.Field("places", placesOf(wk))

	members := make([]*hyper.Envelope, 0, len(wk.Items))
	for _, it := range wk.Items {
		members = append(members, s.itemEnvelope(it, wk, false, positions))
	}
	doc.Field("members", members)

	doc.Link("items", workHref(wk.Key)+"/items", "")
	rep := memberByID(wk, wk.RepresentativeItemID)
	if rep != nil {
		if rep.Enrichment != nil && rep.Enrichment.HasPoster {
			doc.Link("poster", itemHref(rep.ID)+"/poster", "")
		}
		if wk.Medium != model.MediumVideo {
			doc.Link("cover", itemHref(rep.ID)+"/cover", "")
		}
	}

	// Where the work starts: its representative — the film, the first
	// episode, track one — unless this profile has been here before, when it
	// picks up at the first member they have not finished.
	target, label := rep, "▶ Play"
	if anyPosition(wk, positions) {
		if next := nextUnfinished(wk, positions); next != nil {
			target, label = next, "▶ Resume"
		}
	}
	switch {
	case wk.Medium == model.MediumText:
		if target != nil {
			doc.Action("read", hyper.Action{
				Method: "POST", Href: itemHref(target.ID) + "/read",
				Input: readInputSketch(), Label: "Read",
			})
		}
		doc.Unavailable("play", "a book is read, not played")
	default:
		if target != nil && target.MediaInfo != nil {
			doc.Action("play", hyper.Action{
				Method: "POST", Href: itemHref(target.ID) + "/play",
				Input: playInput(), Label: label,
			})
		} else if target != nil {
			doc.Unavailable("play", unprobed(*target))
		}
		doc.Unavailable("read", notReadable(wk.Kind))
	}
	// GET /api/-/passage mints a link from a from/to pair spelled in the
	// grammar `places` publishes, and answers with the link and its
	// sentence. The work is where a person picks the two places, which is
	// why the action is named here: the two tokens are handed back exactly
	// as this document spelled them.
	doc.Action("passage", hyper.Action{
		Method: "GET",
		Href:   "/api/-/passage?work=" + url.QueryEscape(wk.Key),
		Input:  map[string]string{"from": "place?", "to": "place?"},
		Label:  "Copy a passage link",
	})

	return doc
}

// positionsFor is one profile's playback rows, by item id; nil for an
// anonymous caller, which every derivation below reads as "nothing known".
func (s *server) positionsFor(profile string) (map[int64]store.Position, error) {
	if profile == "" {
		return nil, nil
	}
	ps, err := s.state.PositionsFor(profile)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]store.Position, len(ps))
	for _, p := range ps {
		m[p.ItemID] = p
	}
	return m, nil
}

func memberByID(wk *works.Work, id int64) *store.Item {
	for i := range wk.Items {
		if wk.Items[i].ID == id {
			return &wk.Items[i]
		}
	}
	if len(wk.Items) > 0 {
		return &wk.Items[0]
	}
	return nil
}

// walkedMembers counts the members the work is moved through — bonus
// material is attached to a work, not part of the sequence.
func walkedMembers(wk *works.Work) int {
	n := 0
	for _, it := range wk.Items {
		if identityKind(it) != "extra" {
			n++
		}
	}
	return n
}

func anyPosition(wk *works.Work, positions map[int64]store.Position) bool {
	for _, it := range wk.Items {
		if _, ok := positions[it.ID]; ok {
			return true
		}
	}
	return false
}

// nextUnfinished is the first member, in the work's own order and bonus
// material aside, that this profile has not finished. nil when they have
// been all the way through — a finished work starts again at the top.
func nextUnfinished(wk *works.Work, positions map[int64]store.Position) *store.Item {
	for i := range wk.Items {
		it := &wk.Items[i]
		if identityKind(*it) == "extra" {
			continue
		}
		p, seen := positions[it.ID]
		if !seen || !works.Finished(*it, p) {
			return it
		}
	}
	return nil
}

// serverProblem is the last-resort refusal: something below the handler
// failed, and the caller is told so in the same envelope as every other
// answer rather than in a bare 500.
func serverProblem(err error) hyper.Problem {
	return hyper.Refuse(http.StatusInternalServerError, "server-error",
		"The server could not answer that", err.Error()).
		WithRemedy("try again; if it keeps failing the server log has the rest", nil)
}
