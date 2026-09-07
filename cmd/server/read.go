package main

// The reading session (docs/hypermedia.md, step 5 of its order of work).
//
// A book is read through a session too. Until now `read` was a link to the
// bytes — the client fetched /api/items/{id}/book, guessed /api/progress for
// the place, and held the text passage's rules itself. That made the reader
// the one client in the house with rules of its own, and a rule that lives
// only in a client is manners, not a rule.
//
// POST /api/items/{id}/read opens a session the same shape a play opens: the
// same registry (session.go), the same progress action, the same 409 while a
// passage is on. What differs is the unit. A book has no clock, so where a
// play carries `url` and a decision trace this carries `links.book` (the
// bytes), the TOC as `sections` (or `page_count` for a PDF), the profile's
// saved LOCATOR to resume from, and a passage whose bounds are locators —
// resolved here to the section indexes or the pages they land on, so the
// reader is handed a place rather than a spelling. The escape hatch is
// `keep_reading`: keep_watching in the words of the medium, and the very
// same handler.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/passage"
	"flickr/internal/scanner"
	"flickr/internal/store"
	"flickr/internal/works"
)

// methodRead is a reading session's `method`, where a play's is the
// decision's ("direct" or "transcode"). It is what tells the one registry,
// the one progress action and the one reaper which kind of sitting a row is.
const methodRead = "read"

// readIdleTimeout is how long a reading session outlives its last action.
// A play is reaped after sessionIdleTimeout because a client that has gone
// quiet for five minutes has gone; a reader that has gone quiet has been
// reading the page. Nothing is being transcoded, so the only cost of the
// longer grace is one small row in memory.
const readIdleTimeout = 4 * time.Hour

// readInput is the body POST /api/items/{id}/read takes: who is reading, and
// the passage they arrived under, if any. Both are optional — a bare POST
// opens the book for an anonymous reader at the top.
type readInput struct {
	ClientID string        `json:"client_id"`
	Passage  *passageInput `json:"passage"`
}

// readInputSketch is what the `read` action publishes as its input.
func readInputSketch() map[string]string {
	return map[string]string{"client_id": "string?", "passage": "passage?"}
}

// handleRead is POST /api/items/{id}/read — the item document's `read`
// action. It opens the book on a session and answers that session's
// document; the bytes are `links.book` on it, not this route.
func (s *server) handleRead(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("follow an item's read action rather than composing its address",
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
	if item.MediaInfo.MediumOrVideo() != model.MediumText {
		// The mirror of the item document's `unavailable.read`, said as a
		// refusal: the kind's own word for what this is, and play as the
		// remedy, which is the action it does afford.
		hyper.WriteProblem(w, hyper.Refuse(http.StatusUnsupportedMediaType, "not-a-book",
			"This is not something to read", notReadable(identityKind(*item))).
			WithRemedy("play it instead",
				&hyper.Link{Href: itemHref(id) + "/play", Title: "▶ " + itemTitle(*item)}))
		return
	}
	var in readInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-body",
			"That is not a read", err.Error()).
			WithRemedy("send the fields this action's input names, or nothing at all", nil))
		return
	}
	client := strings.TrimSpace(in.ClientID)
	if client == "" {
		client = profileOf(r)
	}
	row := playSession{
		ID: newSessionID(), ItemID: id, ClientID: client,
		Method: methodRead, URL: itemHref(id) + "/book",
		Passage:   textPassage(resolvePassage(in.Passage, id)),
		StartedAt: time.Now(),
	}
	if ws, err := s.buildWorks(); err == nil {
		if wk := works.ByItem(ws)[id]; wk != nil {
			row.WorkKey = wk.Key
		}
	}
	s.plays.put(row)
	s.writeSession(w, r, row, http.StatusOK)
}

// ── the document ────────────────────────────────────────────────────────

// bookSection is one entry of a book's contents: the 1-based spine position
// a `ch:` locator counts by, and the label the reader's TOC shows. The prober
// wrote them (media_info.chapters, one per reading-order document); a book
// probed before that list existed still says how many there are, and those
// are numbered rather than named.
type bookSection struct {
	Index int    `json:"index"`
	Title string `json:"title"`
}

func bookSections(it store.Item) []bookSection {
	mi := it.MediaInfo
	if mi == nil {
		return nil
	}
	out := make([]bookSection, 0, len(mi.Chapters))
	for i, ch := range mi.Chapters {
		title := ch.Title
		if title == "" {
			title = fmt.Sprintf("Section %d", i+1)
		}
		out = append(out, bookSection{Index: i + 1, Title: title})
	}
	for i := len(out); i < mi.Sections; i++ {
		out = append(out, bookSection{Index: i + 1, Title: fmt.Sprintf("Section %d", i+1)})
	}
	return out
}

// bookFormat is which reader pane opens this book. The client asks nothing
// about the file name for it: a PDF is read page by page, everything else is
// an EPUB, and the server is the one that already knows which by extension.
func bookFormat(it store.Item) string {
	if scanner.IsPDF(it.ObjectKey) {
		return "pdf"
	}
	return "epub"
}

func pageCount(it store.Item) int {
	if it.MediaInfo == nil {
		return 0
	}
	return it.MediaInfo.PageCount
}

// readPassage is a text passage as the reading session carries it: the two
// locators as the grammar spells them, and — this is the resolution the
// client used to do — the section or the page each one lands on in THIS
// book. `ends_at` is the bound the reader's own clock watches for, the
// locator counterpart of a play session's `ends_at` in seconds.
type readPassage struct {
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	FromSection int    `json:"from_section,omitempty"`
	ToSection   int    `json:"to_section,omitempty"`
	FromPage    int    `json:"from_page,omitempty"`
	ToPage      int    `json:"to_page,omitempty"`
	EndsAt      string `json:"ends_at,omitempty"`
}

// textPassage is the passage a READING session keeps: the text half of one,
// canonically spelled, or nothing at all. A book has no clock, so a `t` or an
// `end` that rode in on the same link says nothing here and is dropped rather
// than kept as a bound nobody can honour — and a passage that says nothing
// about the text is no passage for a reader, which is what makes the 409 mean
// what it says.
//
// A `to` BEFORE `from` is no bound, the way an `end` at or before `t` is
// dropped: judged only where the two are comparable without opening the book.
// Two locators on the same section (or the same page) are a passage, not an
// empty one — both bounds are inclusive.
func textPassage(p *sessionPassage) *sessionPassage {
	if p == nil {
		return nil
	}
	from, to := passage.ParseLocator(p.From), passage.ParseLocator(p.To)
	if from != nil && to != nil && from.Kind == to.Kind {
		if (to.Kind == "ch" || to.Kind == "pg") && to.N < from.N ||
			to.Kind == "pct" && to.F < from.F {
			to = nil
		}
	}
	out := &sessionPassage{From: from.String(), To: to.String()}
	if !out.text() {
		return nil
	}
	return out
}

// readPassageOf resolves the session's passage against this book.
func readPassageOf(p *sessionPassage, it store.Item) *readPassage {
	if p == nil {
		return nil
	}
	rp := &readPassage{From: p.From, To: p.To, EndsAt: p.To}
	from, to := passage.ParseLocator(p.From), passage.ParseLocator(p.To)
	if n := sectionCount(&it); n > 0 {
		rp.FromSection, rp.ToSection = from.Section(n), to.Section(n)
	}
	if n := pageCount(it); n > 0 {
		rp.FromPage, rp.ToPage = from.Page(n), to.Page(n)
	}
	return rp
}

// savedLocator is where this profile left the book, or nil for a reader this
// book has never seen. It is on the document so the reader resumes from what
// it was handed rather than fetching a second address for it.
func (s *server) savedLocator(row playSession) *model.Locator {
	if row.ClientID == "" {
		return nil
	}
	pos, err := s.state.GetPosition(row.ItemID, row.ClientID)
	if err != nil {
		return nil
	}
	return pos.Locator
}

// readEnvelope is one reading as a document — the session document's text
// half, answered by the read action, by GET /api/sessions/{id} and by
// keep_reading alike.
func (s *server) readEnvelope(row playSession, item store.Item, wk *works.Work) *hyper.Envelope {
	base := "/api/sessions/" + row.ID
	doc := hyper.Doc(base, "session", itemTitle(item)).
		Field("id", row.ID).
		Field("item_id", row.ItemID)
	if row.ClientID != "" {
		doc.Field("profile", row.ClientID)
	}
	doc.Field("method", methodRead).
		Field("format", bookFormat(item)).
		// The bytes' etag: the reader caches what it works out about a book
		// (an EPUB's generated locations) against it, and a re-scanned file
		// is a different book to that cache.
		Field("etag", item.ETag).
		Field("started_at", row.StartedAt.UTC().Format(time.RFC3339))
	if secs := bookSections(item); len(secs) > 0 {
		doc.Field("sections", secs)
	}
	if n := pageCount(item); n > 0 {
		doc.Field("page_count", n)
	}
	// locator and passage are written even when empty, for the reason the
	// play document writes its passage: a client that reads `null` knows the
	// answer, where a missing field only means it is reading an older server.
	doc.Field("locator", s.savedLocator(row)).
		Field("passage", readPassageOf(row.Passage, item))

	doc.Field("display", s.bookDisplay(item))

	doc.Link("item", itemHref(item.ID), itemTitle(item))
	doc.Link("back", itemHref(item.ID), itemTitle(item))
	doc.Link("book", itemHref(item.ID)+"/book", "")
	if wk != nil {
		doc.Link("work", workHref(wk.Key), wk.Title)
	}

	doc.Action("progress", hyper.Action{
		Method: "POST", Href: base + "/progress",
		Input: sessionProgressInput(item), Label: "Save the place",
	})
	if row.Passage != nil {
		doc.Action("keep_reading", hyper.Action{
			Method: "POST", Href: base + "/keep_reading", Label: keepLabel(item),
		})
	} else {
		doc.Unavailable("keep_reading",
			"no passage is on — this is ordinary reading, and the place is being saved")
	}
	doc.Action("set_display", hyper.Action{
		Method: "POST", Href: base + "/display",
		Input: map[string]string{"images": "themed | printed"},
		Label: "Pictures on the dark page",
	})
	doc.Action("stop", hyper.Action{
		Method: "DELETE", Href: base, Label: "Close the book",
	})
	return doc
}

// bookDisplay is how this book's pictures are shown on a dark page, the
// session's `display` field: {images: themed|printed, chosen: bool}.
// `themed` recolours them with the page — a diagram reads on dark, a photo
// turns to a negative; `printed` leaves them as printed. The default is the
// format's: an EPUB's figures stay printed, because its text is themed on
// its own and a wrong guess on a photo is worse than a bright diagram; a
// PDF is one picture of a page, so themed is the only dark page it has.
// A choice made for the book (SetBookDisplay) stands over the default.
type bookDisplay struct {
	Images string `json:"images"`
	Chosen bool   `json:"chosen"`
}

func (s *server) bookDisplay(item store.Item) bookDisplay {
	if s.state != nil {
		if v, err := s.state.BookDisplay(item.ID); err == nil && v != "" {
			return bookDisplay{Images: v, Chosen: true}
		}
	}
	if bookFormat(item) == "pdf" {
		return bookDisplay{Images: "themed"}
	}
	return bookDisplay{Images: "printed"}
}

// handleSetDisplay records how this book's pictures are shown, for the book
// — every profile and every device — and answers the session.
func (s *server) handleSetDisplay(w http.ResponseWriter, r *http.Request) {
	row, ok := s.session(w, r)
	if !ok {
		return
	}
	if row.Method != methodRead {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "not-a-book",
			"Only a book has a page to show pictures on", "this session is playing, not reading").
			WithRemedy("open a book with its read action", nil))
		return
	}
	var in struct {
		Images string `json:"images"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-input", "The body is not JSON", err.Error()))
		return
	}
	if in.Images != "themed" && in.Images != "printed" {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusUnprocessableEntity, "no-such-display",
			"Pictures are themed or printed", fmt.Sprintf("%q is neither", in.Images)).
			WithRemedy("send images: themed (recoloured with the dark page) or printed (as printed)", nil))
		return
	}
	if err := s.state.SetBookDisplay(row.ItemID, in.Images); err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusInternalServerError, "display-not-saved", "Could not save the choice", err.Error()))
		return
	}
	s.writeSession(w, r, row, http.StatusOK)
}
