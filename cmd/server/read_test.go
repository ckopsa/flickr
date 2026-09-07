package main

// The reading session, as the handlers answer it (docs/hypermedia.md
// §Passages are session state, step 5).
//
// Same fixture library as every other document test, through the real
// routing table: two books, an EPUB with a titled spine and a PDF with a
// page count, and a profile who is seven sections into the first of them.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
)

// reading opens a book and answers its session document.
func reading(t *testing.T, h http.Handler, id int64, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["client_id"]; !ok {
		body["client_id"] = "chris"
	}
	w := post(t, h, "/api/items/"+strconv.FormatInt(id, 10)+"/read", body)
	if w.Code != http.StatusOK {
		t.Fatalf("POST read = %d: %s", w.Code, w.Body)
	}
	return decode(t, w)
}

// A book is read through a session too: the same envelope a play answers,
// in the units a book has — the bytes as a link, the spine as `sections`,
// and the profile's saved LOCATOR to resume from.
func TestReadOpensASession(t *testing.T) {
	_, h := fixtureServer(t)
	doc := reading(t, h, idHillHouse, nil)

	if doc["kind"] != "session" || doc["method"] != methodRead {
		t.Fatalf("kind = %v, method = %v", doc["kind"], doc["method"])
	}
	id, _ := doc["id"].(string)
	if id == "" || doc["self"] != "/api/sessions/"+id {
		t.Errorf("self = %v, id = %q", doc["self"], id)
	}
	if doc["format"] != "epub" || doc["etag"] != "e3" {
		t.Errorf("format = %v, etag = %v", doc["format"], doc["etag"])
	}
	if got := href(t, doc, "links", "book"); got != "/api/items/3/book" {
		t.Errorf("links.book = %q", got)
	}
	if got := href(t, doc, "links", "back"); got != itemHref(idHillHouse) {
		t.Errorf("links.back = %q", got)
	}
	if got := href(t, doc, "links", "work"); got == "" {
		t.Error("a book's session does not name its work")
	}
	// The contents, as the reader's TOC draws them: one entry per spine
	// item, in reading order, titled by the prober.
	secs, _ := doc["sections"].([]any)
	if len(secs) != 8 {
		t.Fatalf("sections = %v", doc["sections"])
	}
	first, _ := secs[0].(map[string]any)
	last, _ := secs[7].(map[string]any)
	if first["index"] != 1.0 || first["title"] != "Cover" ||
		last["index"] != 8.0 || last["title"] != "Afterword" {
		t.Errorf("sections run %v … %v", first, last)
	}
	if _, ok := doc["page_count"]; ok {
		t.Errorf("an EPUB has no page count: %v", doc["page_count"])
	}
	// Where chris left it — on the document, so the reader resumes from
	// what it was handed rather than asking a second address for it.
	loc, ok := doc["locator"].(map[string]any)
	if !ok {
		t.Fatalf("no saved locator: %v", doc["locator"])
	}
	if loc["cfi"] != "epubcfi(/6/14[ch07]!/4/2/1:0)" || loc["section"] != 7.0 || loc["fraction"] != 0.34 {
		t.Errorf("locator = %v", loc)
	}
	if doc["passage"] != nil {
		t.Errorf("an ordinary read has no passage: %v", doc["passage"])
	}
	for _, a := range []string{"progress", "stop"} {
		if !has(doc, "actions", a) {
			t.Errorf("no %s action", a)
		}
	}
	if has(doc, "actions", "keep_reading") || !has(doc, "unavailable", "keep_reading") {
		t.Error("keep_reading should be unavailable when no passage is on")
	}

	// The same document at its own address, through the session route every
	// other session answers on.
	w := get(t, h, "/api/sessions/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("GET session = %d: %s", w.Code, w.Body)
	}
	if again := decode(t, w); again["self"] != doc["self"] || again["format"] != doc["format"] {
		t.Errorf("GET /api/sessions/%s is a different document", id)
	}
}

// A reader nobody has met yet resumes from nowhere, and says so.
func TestReadWithoutASavedPlace(t *testing.T) {
	_, h := fixtureServer(t)
	doc := reading(t, h, idHillHouse, map[string]any{"client_id": "dana"})
	if doc["locator"] != nil {
		t.Errorf("dana has never read this book: %v", doc["locator"])
	}
}

// The passage rides the session, resolved: the two locators as the grammar
// spells them, and the sections they land in — the resolution the client
// used to do for itself.
func TestReadResolvesATextPassage(t *testing.T) {
	_, h := fixtureServer(t)
	doc := reading(t, h, idHillHouse, map[string]any{
		"passage": map[string]any{"from": "ch:3", "to": "ch:4"},
	})
	p, ok := doc["passage"].(map[string]any)
	if !ok {
		t.Fatalf("no passage on the session: %v", doc["passage"])
	}
	if p["from"] != "ch:3" || p["to"] != "ch:4" ||
		p["from_section"] != 3.0 || p["to_section"] != 4.0 || p["ends_at"] != "ch:4" {
		t.Errorf("passage = %v", p)
	}
	if !has(doc, "actions", "keep_reading") {
		t.Error("a passage session offers keep_reading")
	}

	// A percentage and a bare CFI are places too, and are spelled back the
	// way a link spells them.
	doc = reading(t, h, idHillHouse, map[string]any{
		"passage": map[string]any{"from": "pct:0.4", "to": "epubcfi(/6/14!/4/2)"},
	})
	p, _ = doc["passage"].(map[string]any)
	if p["from"] != "pct:0.4" || p["to"] != "cfi:epubcfi(/6/14!/4/2)" ||
		p["from_section"] != 4.0 || p["to_section"] != 7.0 {
		t.Errorf("passage = %v", p)
	}

	// A `to` before `from` is no bound, the way an `end` at or before `t` is
	// dropped; the same two sections ARE a passage, both bounds inclusive.
	doc = reading(t, h, idHillHouse, map[string]any{
		"passage": map[string]any{"from": "ch:4", "to": "ch:3"},
	})
	p, _ = doc["passage"].(map[string]any)
	if _, ok := p["to"]; ok {
		t.Errorf("a backwards bound should be dropped: %v", p)
	}
	doc = reading(t, h, idHillHouse, map[string]any{
		"passage": map[string]any{"from": "ch:3", "to": "ch:3"},
	})
	p, _ = doc["passage"].(map[string]any)
	if p["to"] != "ch:3" {
		t.Errorf("one section is a passage: %v", p)
	}

	// A clock says nothing about a book: `t` and `end` are not bounds a
	// reader can honour, and a passage that says nothing about the text is
	// no passage at all here.
	doc = reading(t, h, idHillHouse, map[string]any{
		"passage": map[string]any{"t": 142, "end": 854},
	})
	if doc["passage"] != nil {
		t.Errorf("a timed passage is not a reader's: %v", doc["passage"])
	}
	if !has(doc, "unavailable", "keep_reading") {
		t.Error("with no passage on, keep_reading is unavailable")
	}
}

// Rule 3 for a book, and its escape hatch: a locator write during a text
// passage is the same 409, with keep_reading as the remedy, and the place
// is saved the moment it is taken.
func TestLocatorProgressIsRefusedWhileAPassageIsOn(t *testing.T) {
	srv, h := fixtureServer(t)
	doc := reading(t, h, idHillHouse, map[string]any{
		"passage": map[string]any{"from": "ch:3", "to": "ch:4"},
	})
	progress := href(t, doc, "actions", "progress")
	keep := href(t, doc, "actions", "keep_reading")

	place := map[string]any{"locator": "epubcfi(/6/8[ch04]!/4/2/1:0)", "section": 4, "fraction": 0.5}
	w := post(t, h, progress, place)
	if w.Code != http.StatusConflict {
		t.Fatalf("locator progress during a passage = %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q", ct)
	}
	problem := decode(t, w)
	if problem["type"] != "passage-on" {
		t.Errorf("type = %v", problem["type"])
	}
	remedy, _ := problem["remedy"].(map[string]any)
	link, _ := remedy["link"].(map[string]any)
	if link["href"] != keep || link["title"] != "Keep reading" {
		t.Errorf("the remedy does not point at keep_reading: %v", remedy)
	}
	// The saved place did not move: chris is still where he was reading.
	pos, err := srv.state.GetPosition(idHillHouse, "chris")
	if err != nil || pos.Locator == nil || pos.Locator.Section != 7 {
		t.Errorf("the refused write moved the place to %+v (%v)", pos.Locator, err)
	}

	// The legacy route holds the same rule for a book.
	if w := post(t, h, "/api/progress", map[string]any{
		"item_id": idHillHouse, "client_id": "chris", "locator": "ch:4", "section": 4,
	}); w.Code != http.StatusConflict {
		t.Errorf("legacy locator write during a passage = %d: %s", w.Code, w.Body)
	}

	// Keep reading: the passage is gone, and the document says so.
	w = post(t, h, keep, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("keep_reading = %d: %s", w.Code, w.Body)
	}
	after := decode(t, w)
	if after["passage"] != nil {
		t.Errorf("keep_reading left a passage on: %v", after["passage"])
	}
	if has(after, "actions", "keep_reading") {
		t.Error("keep_reading is still offered after it was taken")
	}
	if after["method"] != methodRead {
		t.Errorf("keep_reading answered something other than the reading session: %v", after["method"])
	}

	w = post(t, h, progress, place)
	if w.Code != http.StatusOK {
		t.Fatalf("locator progress after keep_reading = %d: %s", w.Code, w.Body)
	}
	pos, err = srv.state.GetPosition(idHillHouse, "chris")
	if err != nil || pos.Locator == nil ||
		pos.Locator.CFI != "epubcfi(/6/8[ch04]!/4/2/1:0)" || pos.Locator.Section != 4 {
		t.Errorf("saved place = %+v (%v)", pos.Locator, err)
	}
}

// A PDF is a book with pages instead of sections: the page count comes back
// on the document, a page passage resolves to pages, and the place written
// is a page.
func TestReadAPDFCountsInPages(t *testing.T) {
	srv, h := fixtureServer(t)
	doc := reading(t, h, idFlatland, map[string]any{
		"passage": map[string]any{"from": "pg:213", "to": "pg:240"},
	})
	if doc["format"] != "pdf" || doc["page_count"] != 400.0 {
		t.Fatalf("format = %v, page_count = %v", doc["format"], doc["page_count"])
	}
	if _, ok := doc["sections"]; ok {
		t.Errorf("a PDF has no spine: %v", doc["sections"])
	}
	p, _ := doc["passage"].(map[string]any)
	if p["from_page"] != 213.0 || p["to_page"] != 240.0 || p["ends_at"] != "pg:240" {
		t.Errorf("passage = %v", p)
	}
	if _, ok := p["from_section"]; ok {
		t.Errorf("a page says nothing about sections: %v", p)
	}

	progress := href(t, doc, "actions", "progress")
	if w := post(t, h, progress, map[string]any{"page": 240, "fraction": 0.6}); w.Code != http.StatusConflict {
		t.Fatalf("page progress during a passage = %d: %s", w.Code, w.Body)
	}
	if w := post(t, h, href(t, doc, "actions", "keep_reading"), nil); w.Code != http.StatusOK {
		t.Fatalf("keep_reading = %d: %s", w.Code, w.Body)
	}
	if w := post(t, h, progress, map[string]any{"page": 240, "fraction": 0.6}); w.Code != http.StatusOK {
		t.Fatalf("page progress after keep_reading = %d: %s", w.Code, w.Body)
	}
	pos, err := srv.state.GetPosition(idFlatland, "chris")
	if err != nil || pos.Locator == nil || pos.Locator.Page != 240 || pos.Locator.Fraction != 0.6 {
		t.Errorf("saved place = %+v (%v)", pos.Locator, err)
	}
}

// A film is not a book to open: the refusal says so in the kind's own words
// and points at the action this item does afford.
func TestReadRefusesWhatIsNotABook(t *testing.T) {
	_, h := fixtureServer(t)
	w := post(t, h, "/api/items/4/read", map[string]any{"client_id": "chris"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("read a film = %d: %s", w.Code, w.Body)
	}
	problem := decode(t, w)
	if problem["type"] != "not-a-book" || problem["detail"] != "a film is played, not read" {
		t.Errorf("problem = %v", problem)
	}
	remedy, _ := problem["remedy"].(map[string]any)
	link, _ := remedy["link"].(map[string]any)
	if link["href"] != "/api/items/4/play" {
		t.Errorf("remedy = %v", remedy)
	}

	if w := post(t, h, "/api/items/9999/read", nil); w.Code != http.StatusNotFound {
		t.Errorf("read a missing item = %d: %s", w.Code, w.Body)
	}
}

// Stopping a reading session is the same DELETE every session takes.
func TestStopEndsAReadingSession(t *testing.T) {
	// fixturePlayer, not fixtureServer: the stop route goes through the
	// transcode manager on its way to the row, and a book has no transcode
	// for it to find — but the manager still has to be there.
	_, h := fixturePlayer(t)
	doc := reading(t, h, idHillHouse, nil)
	id, _ := doc["id"].(string)
	r := httptest.NewRequest(http.MethodDelete, href(t, doc, "actions", "stop"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("stop = %d: %s", w.Code, w.Body)
	}
	if w := get(t, h, "/api/sessions/"+id); w.Code != http.StatusNotFound {
		t.Errorf("the session outlived its stop: %d", w.Code)
	}
}

// ── the goldens ─────────────────────────────────────────────────────────

// A session's id is minted and its start is a clock reading, so both are
// pinned before the document is compared: everything else about it is the
// library's and has to hold still.
var startedAtRe = regexp.MustCompile(`"started_at":"[^"]*"`)

func readGolden(t *testing.T, name string, doc map[string]any, body []byte) {
	t.Helper()
	id, _ := doc["id"].(string)
	if id == "" {
		t.Fatalf("%s: the session has no id", name)
	}
	body = bytes.ReplaceAll(body, []byte(id), []byte("5e5510f00d00"))
	body = startedAtRe.ReplaceAll(body, []byte(`"started_at":"2026-09-07T09:00:00Z"`))
	golden(t, name, body)
}

func TestReadSessionGolden(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct {
		name string
		id   int64
		body map[string]any
	}{
		// The EPUB, opened on a passage: a titled spine, the saved locator,
		// and the two bounds resolved to sections.
		{"session-read", idHillHouse, map[string]any{
			"client_id": "chris",
			"passage":   map[string]any{"from": "ch:3", "to": "ch:4"},
		}},
		// The PDF, opened plainly: a page count where the spine was, and no
		// passage — so keep_reading is an `unavailable` with its reason.
		{"session-read-pdf", idFlatland, map[string]any{"client_id": "chris"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := post(t, h, "/api/items/"+strconv.FormatInt(tc.id, 10)+"/read", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("POST read = %d: %s", w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q", ct)
			}
			readGolden(t, tc.name, decode(t, w), w.Body.Bytes())
		})
	}
}
