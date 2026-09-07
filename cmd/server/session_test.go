package main

// The session document and the three rules of a passage, as the handlers
// answer them (docs/hypermedia.md §Passages are session state).
//
// Everything here runs over the same fixture library hyper_test.go builds,
// through the real routing table. No MinIO: the direct-play branch presigns
// through the server's own seam, which the fixture fills with a stub, so a
// play is a play without a byte of media anywhere.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flickr/internal/model"
	"flickr/internal/pipeline"
)

// directPlayCaps read the show's episodes as they are (mkv/h264/aac, 720p,
// stereo): the decision is direct play, so no ffmpeg is started.
func directPlayCaps() model.ClientCapabilities {
	return model.ClientCapabilities{
		SchemaVersion: model.CapabilitySchemaVersion,
		Containers:    []string{"mkv"},
		VideoCodecs:   []string{"h264"},
		AudioCodecs:   []string{"aac", "ac3"},
		MaxWidth:      1920, MaxHeight: 1080, MaxAudioChannels: 6,
	}
}

// fixturePlayer is the fixture library with the storage presigner stubbed:
// a direct play needs a URL to hand back, not bytes.
func fixturePlayer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	srv, h := fixtureServer(t)
	srv.presign = func(_ context.Context, objectKey string) (string, error) {
		return "https://storage.test/" + objectKey + "?signed", nil
	}
	// A stop goes through the transcode manager on its way to the row, and
	// every play here direct-plays, so this one never starts an ffmpeg.
	srv.sessions = pipeline.NewSessionManager(t.TempDir(), nil)
	return srv, h
}

// play plays one item and answers its session document.
func play(t *testing.T, h http.Handler, id int64, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["capabilities"]; !ok {
		body["capabilities"] = directPlayCaps()
	}
	if _, ok := body["client_id"]; !ok {
		body["client_id"] = "chris"
	}
	w := post(t, h, fmt.Sprintf("/api/items/%d/play", id), body)
	if w.Code != http.StatusOK {
		t.Fatalf("POST play = %d: %s", w.Code, w.Body)
	}
	return decode(t, w)
}

// playing is the pair of them: a fresh server, playing one item.
func playing(t *testing.T, id int64, body map[string]any) (*server, http.Handler, map[string]any) {
	t.Helper()
	srv, h := fixturePlayer(t)
	return srv, h, play(t, h, id, body)
}

func post(t *testing.T, h http.Handler, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, target, &buf)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, w.Body)
	}
	return m
}

// href digs actions.<name>.href (or links.<rel>.href) out of a document.
func href(t *testing.T, doc map[string]any, group, name string) string {
	t.Helper()
	g, _ := doc[group].(map[string]any)
	entry, ok := g[name].(map[string]any)
	if !ok {
		t.Fatalf("document has no %s.%s\n%v", group, name, doc)
	}
	h, _ := entry["href"].(string)
	if h == "" {
		t.Fatalf("%s.%s has no href", group, name)
	}
	return h
}

func has(doc map[string]any, group, name string) bool {
	g, _ := doc[group].(map[string]any)
	_, ok := g[name]
	return ok
}

// The play session as a golden, so the renderers on the other side of the
// wire (web/render_test.mjs) draw the player chrome from the same bytes the
// handler writes. The id is minted and the start is a clock reading, so both
// are pinned the way a reading session's are (read_test.go).
func TestPlaySessionGolden(t *testing.T) {
	// Beach Games under the passage a deep link opens it with: `t` seeds the
	// seek, `end` is the bound the client's clock stops at, and `until` makes
	// it a run — so the document carries keep_watching, next and the marks
	// all at once, which is the whole of the player chrome.
	_, h := fixturePlayer(t)
	w := post(t, h, fmt.Sprintf("/api/items/%d/play", idBeach), map[string]any{
		"capabilities": directPlayCaps(), "client_id": "chris",
		"passage": map[string]any{"t": 142, "end": 854, "until": idTheJob},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST play = %d: %s", w.Code, w.Body)
	}
	readGolden(t, "session-play", decode(t, w), w.Body.Bytes())
}

// A play is a session: the document says what was decided, where the bytes
// are, and what may be done next — for a DIRECT play too, which until now
// had no id at all.
func TestPlayAnswersTheSessionDocument(t *testing.T) {
	_, h, doc := playing(t, idBeach, nil)
	if doc["kind"] != "session" {
		t.Fatalf("kind = %v", doc["kind"])
	}
	id, _ := doc["id"].(string)
	if id == "" || doc["self"] != "/api/sessions/"+id {
		t.Errorf("self = %v, id = %q", doc["self"], id)
	}
	if doc["method"] != string(model.DirectPlay) {
		t.Errorf("method = %v", doc["method"])
	}
	if url, _ := doc["url"].(string); !strings.HasPrefix(url, "https://storage.test/") {
		t.Errorf("url = %v", doc["url"])
	}
	if doc["passage"] != nil {
		t.Errorf("an ordinary play has no passage: %v", doc["passage"])
	}
	if _, ok := doc["decision"].(map[string]any); !ok {
		t.Error("the document carries no decision trace")
	}
	if got := href(t, doc, "links", "item"); got != itemHref(idBeach) {
		t.Errorf("links.item = %q", got)
	}
	if got := href(t, doc, "links", "back"); got != itemHref(idBeach) {
		t.Errorf("links.back = %q", got)
	}
	// The Office's next episode: the work walks, so the run has somewhere
	// to go and the document says where.
	if got := href(t, doc, "links", "next"); got != itemHref(idTheJob) {
		t.Errorf("links.next = %q", got)
	}
	for _, a := range []string{"progress", "stop", "next", "mark_in", "mark_out"} {
		if !has(doc, "actions", a) {
			t.Errorf("no %s action", a)
		}
	}
	// Nothing is marked yet, so there is no link to copy — and the document
	// says why rather than leaving a button that does nothing.
	if has(doc, "actions", "link") || !has(doc, "unavailable", "link") {
		t.Error("link should be unavailable before an in point exists")
	}
	if has(doc, "actions", "keep_watching") || !has(doc, "unavailable", "keep_watching") {
		t.Error("keep_watching should be unavailable when no passage is on")
	}

	// The same document is at its own address.
	w := get(t, h, "/api/sessions/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("GET session = %d: %s", w.Code, w.Body)
	}
	if again := decode(t, w); again["self"] != doc["self"] || again["url"] != doc["url"] {
		t.Errorf("GET /api/sessions/%s is a different document", id)
	}
}

// What the cast bridge reads off the document instead of composing it: the
// media type of the bytes, where they begin, and the picture to draw. The
// sender used to say "video/mp4" for every direct play and show no artwork
// at all — both were guesses, and both are the server's to answer.
func TestSessionDocumentSaysWhatToPlayAndHowItLooks(t *testing.T) {
	_, _, doc := playing(t, idBeach, nil)
	if doc["content_type"] != "video/x-matroska" {
		t.Errorf("content_type = %v (the fixture episode is an mkv)", doc["content_type"])
	}
	if _, ok := doc["seek_seconds"]; ok {
		t.Errorf("a play from the top has no seek: %v", doc["seek_seconds"])
	}
	// The episode has a TMDB still, so that is the picture — a frame of the
	// episode, not the show's poster.
	if got := href(t, doc, "links", "artwork"); got != itemHref(idBeach)+"/still" {
		t.Errorf("links.artwork = %q", got)
	}

	// A passage starts where it says, and the document says so: `t` is the
	// seek the server seeded, and the receiver starts there.
	_, _, doc = playing(t, idBeach, map[string]any{
		"passage": map[string]any{"t": 142, "end": 854},
	})
	if doc["seek_seconds"] != 142.0 {
		t.Errorf("seek_seconds = %v, want the passage's t", doc["seek_seconds"])
	}

	// An audio item is its own cover, and an m4b is not video/mp4.
	_, _, doc = playing(t, idDunePart1, map[string]any{
		"capabilities": model.ClientCapabilities{
			SchemaVersion: model.CapabilitySchemaVersion,
			Containers:    []string{"m4b"}, AudioCodecs: []string{"aac"},
			MaxAudioChannels: 6,
		},
	})
	if doc["content_type"] != "audio/mp4" {
		t.Errorf("content_type = %v (an m4b)", doc["content_type"])
	}
	if got := href(t, doc, "links", "artwork"); got != itemHref(idDunePart1)+"/cover" {
		t.Errorf("links.artwork = %q", got)
	}
}

// Rule 1 (start there) and rule 2 (stop there): the server seeds the seek
// from `t` and publishes the bound the client's clock is to watch.
func TestPlayResolvesThePassage(t *testing.T) {
	_, _, doc := playing(t, idBeach, map[string]any{
		"passage": map[string]any{"t": 142, "end": 854},
	})
	p, ok := doc["passage"].(map[string]any)
	if !ok {
		t.Fatalf("no passage on the session: %v", doc["passage"])
	}
	if p["t"] != 142.0 || p["end"] != 854.0 || p["ends_at"] != 854.0 {
		t.Errorf("passage = %v", p)
	}
	if !has(doc, "actions", "keep_watching") {
		t.Error("a passage session offers keep_watching")
	}

	// A run: `end` belongs to the LAST item, so the first item of the run
	// has no bound of its own to stop at.
	_, _, doc = playing(t, idBeach, map[string]any{
		"passage": map[string]any{"t": 142, "end": 300, "until": idTheJob},
	})
	p, _ = doc["passage"].(map[string]any)
	if p["until"] != float64(idTheJob) {
		t.Errorf("until = %v", p["until"])
	}
	if _, ok := p["ends_at"]; ok {
		t.Errorf("the first item of a run has no ends_at: %v", p)
	}

	// An end at or before the start is no bound at all (the grammar's rule,
	// README "Deep links and passages").
	_, _, doc = playing(t, idBeach, map[string]any{
		"passage": map[string]any{"t": 500, "end": 500},
	})
	p, _ = doc["passage"].(map[string]any)
	if _, ok := p["end"]; ok {
		t.Errorf("end at the start should be dropped: %v", p)
	}
}

// Rule 3, and its escape hatch: a progress write during a passage is
// refused with the remedy that lifts it, and accepted the moment it is
// taken.
func TestProgressIsRefusedWhileAPassageIsOn(t *testing.T) {
	srv, h, doc := playing(t, idBeach, map[string]any{
		"passage": map[string]any{"t": 142, "end": 854},
	})
	progress := href(t, doc, "actions", "progress")
	keep := href(t, doc, "actions", "keep_watching")

	w := post(t, h, progress, map[string]any{"position_seconds": 300})
	if w.Code != http.StatusConflict {
		t.Fatalf("progress during a passage = %d: %s", w.Code, w.Body)
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
	if link["href"] != keep {
		t.Errorf("the remedy does not point at keep_watching: %v", remedy)
	}
	if _, ok := remedy["text"].(string); !ok {
		t.Errorf("the remedy says nothing in words: %v", remedy)
	}
	if pos, err := srv.state.GetPosition(idBeach, "chris"); err != nil || pos.PositionSeconds != 745 {
		t.Errorf("the refused write moved the saved place to %v (%v)", pos.PositionSeconds, err)
	}

	// Keep watching: the passage is gone from the session, and the document
	// says so in the same breath.
	w = post(t, h, keep, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("keep_watching = %d: %s", w.Code, w.Body)
	}
	after := decode(t, w)
	if after["passage"] != nil {
		t.Errorf("keep_watching left a passage on: %v", after["passage"])
	}
	if has(after, "actions", "keep_watching") {
		t.Error("keep_watching is still offered after it was taken")
	}

	w = post(t, h, progress, map[string]any{"position_seconds": 300})
	if w.Code != http.StatusOK {
		t.Fatalf("progress after keep_watching = %d: %s", w.Code, w.Body)
	}
	pos, err := srv.state.GetPosition(idBeach, "chris")
	if err != nil || pos.PositionSeconds != 300 {
		t.Errorf("saved place = %v (%v), want 300", pos.PositionSeconds, err)
	}
}

// The legacy route holds the same rule: a client that still writes to
// /api/progress while its passage session is live is refused there too, and
// pointed at the same remedy.
func TestLegacyProgressRefusedWhileAPassageIsOn(t *testing.T) {
	srv, h, doc := playing(t, idBeach, map[string]any{
		"passage": map[string]any{"t": 142, "end": 854},
	})
	legacy := map[string]any{"item_id": idBeach, "client_id": "chris", "position_seconds": 300}

	w := post(t, h, "/api/progress", legacy)
	if w.Code != http.StatusConflict {
		t.Fatalf("legacy progress during a passage = %d: %s", w.Code, w.Body)
	}
	if decode(t, w)["type"] != "passage-on" {
		t.Errorf("body = %s", w.Body)
	}
	// Another profile's write to the same item is nobody else's business.
	if w := post(t, h, "/api/progress", map[string]any{
		"item_id": idBeach, "client_id": "dana", "position_seconds": 12,
	}); w.Code != http.StatusOK {
		t.Errorf("another profile's write = %d: %s", w.Code, w.Body)
	}

	if w := post(t, h, href(t, doc, "actions", "keep_watching"), nil); w.Code != http.StatusOK {
		t.Fatalf("keep_watching = %d: %s", w.Code, w.Body)
	}
	if w := post(t, h, "/api/progress", legacy); w.Code != http.StatusOK {
		t.Fatalf("legacy progress after keep_watching = %d: %s", w.Code, w.Body)
	}
	if pos, err := srv.state.GetPosition(idBeach, "chris"); err != nil || pos.PositionSeconds != 300 {
		t.Errorf("saved place = %v (%v), want 300", pos.PositionSeconds, err)
	}
}

// Marks are the server's: the snap to a chapter start, the swap when the
// two ends arrive the wrong way round, and a later episode becoming the
// run's `until`.
func TestMarksSnapSwapAndRun(t *testing.T) {
	_, h, doc := playing(t, idBeach, nil)
	markIn := href(t, doc, "actions", "mark_in")
	markOut := href(t, doc, "actions", "mark_out")

	// 143.4s is a beat after the "Titles" chapter at 142: the mark meant
	// the chapter.
	doc = decode(t, post(t, h, markIn, map[string]any{"seconds": 143.4}))
	m, _ := doc["marks"].(map[string]any)
	in, _ := m["in"].(map[string]any)
	if in["seconds"] != 142.0 || in["item_id"] != float64(idBeach) {
		t.Errorf("in mark = %v", m)
	}
	if !has(doc, "actions", "link") {
		t.Error("an in point makes the link available")
	}

	// An out point BEFORE the in point is not a mistake to refuse: the
	// person said where the passage is, not which end they meant.
	doc = decode(t, post(t, h, markOut, map[string]any{"seconds": 60}))
	m, _ = doc["marks"].(map[string]any)
	in, _ = m["in"].(map[string]any)
	out, _ := m["out"].(map[string]any)
	if in["seconds"] != 60.0 || out["seconds"] != 142.0 {
		t.Errorf("marks after the swap = %v", m)
	}

	// Re-marking out at a real out point, and rounding: one decimal.
	doc = decode(t, post(t, h, markOut, map[string]any{"seconds": 854.26}))
	m, _ = doc["marks"].(map[string]any)
	out, _ = m["out"].(map[string]any)
	if out["seconds"] != 854.3 {
		t.Errorf("out mark = %v", m)
	}

	// Clearing one end leaves the other.
	doc = decode(t, post(t, h, markOut, map[string]any{"clear": true}))
	m, _ = doc["marks"].(map[string]any)
	if _, ok := m["out"]; ok {
		t.Errorf("the out mark survived being cleared: %v", m)
	}
	if _, ok := m["in"]; !ok {
		t.Errorf("clearing the out mark took the in mark with it: %v", m)
	}
}

// A run is made by marking out on a later episode: the marks follow the
// person across the advance, and the later episode becomes `until`.
func TestMarksCrossEpisodesIntoARun(t *testing.T) {
	_, h, doc := playing(t, idBeach, nil)
	post(t, h, href(t, doc, "actions", "mark_in"), map[string]any{"seconds": 142})

	// Up next, exactly as the player does it: the finished episode's session
	// is stopped, and the next one is started. The marks have to cross that
	// gap or no run could ever be marked.
	r := httptest.NewRequest(http.MethodDelete, doc["self"].(string), nil)
	h.ServeHTTP(httptest.NewRecorder(), r)
	next := play(t, h, idTheJob, nil)
	if m, _ := next["marks"].(map[string]any); m["in"] == nil {
		t.Fatalf("the marks did not survive the episode advance: %v", next["marks"])
	}
	next = decode(t, post(t, h, href(t, next, "actions", "mark_out"), map[string]any{"seconds": 854}))

	w := get(t, h, href(t, next, "actions", "link"))
	if w.Code != http.StatusOK {
		t.Fatalf("link = %d: %s", w.Code, w.Body)
	}
	link := decode(t, w)
	if link["kind"] != "passage" {
		t.Errorf("kind = %v", link["kind"])
	}
	if got, want := link["href"], "#/item/8?t=142&end=854&until=9"; !strings.HasSuffix(got.(string), want) {
		t.Errorf("minted link = %v, want one ending %q", got, want)
	}
	if got, want := link["sentence"], "S03E22 2:22 – S03E23 14:14 of The Office"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
}

// The minted link and the sentence that says it, from a session's marks and
// from the standalone route both.
func TestMintedLinkAndSentence(t *testing.T) {
	_, h, doc := playing(t, idBeach, nil)
	post(t, h, href(t, doc, "actions", "mark_in"), map[string]any{"seconds": 142})
	doc = decode(t, post(t, h, href(t, doc, "actions", "mark_out"), map[string]any{"seconds": 854}))

	link := decode(t, get(t, h, href(t, doc, "actions", "link")))
	if got, want := link["sentence"], "S03E22 2:22 – 14:14 of Beach Games"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	if got, want := link["href"].(string), "#/item/8?t=142&end=854"; !strings.HasSuffix(got, want) {
		t.Errorf("minted link = %q, want one ending %q", got, want)
	}
	if !strings.HasPrefix(link["href"].(string), "http://") {
		t.Errorf("a minted link is absolute: %q", link["href"])
	}

	// The same passage composed rather than marked — a film this time, whose
	// sentence names no episode.
	w := get(t, h, "/api/-/passage?item=4&t=4740&end=5070")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/-/passage = %d: %s", w.Code, w.Body)
	}
	minted := decode(t, w)
	if got, want := minted["sentence"], "1:19:00 – 1:24:30 of Frozen"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	if got, want := minted["href"].(string), "#/item/4?t=4740&end=5070"; !strings.HasSuffix(got, want) {
		t.Errorf("minted link = %q", got)
	}
	if l := minted["links"].(map[string]any)["item"].(map[string]any); l["href"] != itemHref(idFrozen) {
		t.Errorf("links.item = %v", l)
	}

	// A passage with no end reads as one: from there, on.
	minted = decode(t, get(t, h, "/api/-/passage?item=8&t=142"))
	if got, want := minted["sentence"], "S03E22 from 2:22 of Beach Games"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}

	// A passage that starts nowhere is not a passage, and says so.
	w = get(t, h, "/api/-/passage?item=8")
	if w.Code != http.StatusBadRequest || decode(t, w)["type"] != "empty-passage" {
		t.Errorf("a passage with no start = %d: %s", w.Code, w.Body)
	}
}

// The work form: two of the place TOKENS the work document publishes,
// resolved against that work's members (internal/passage reads the episode
// codes and the locators; the pair goes through the same marks the player
// makes, so a run and a backwards pair need no second rule).
func TestMintFromTheWorkPlaces(t *testing.T) {
	_, h := fixturePlayer(t)
	mint := func(query string) (int, map[string]any) {
		w := get(t, h, "/api/-/passage?"+query)
		return w.Code, decode(t, w)
	}

	// A run across two episodes: the later one becomes `until`.
	code, doc := mint("work=tmdb%3A2316&from=S03E22+2%3A22&to=S03E23+14%3A14")
	if code != http.StatusOK {
		t.Fatalf("the work form = %d: %v", code, doc)
	}
	if doc["kind"] != "passage" {
		t.Errorf("kind = %v", doc["kind"])
	}
	if got, want := doc["href"].(string), "#/item/8?t=142&end=854&until=9"; !strings.HasSuffix(got, want) {
		t.Errorf("minted link = %q, want one ending %q", got, want)
	}
	if got, want := doc["sentence"], "S03E22 2:22 – S03E23 14:14 of The Office"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}

	// One episode, and the two places given the wrong way round: the person
	// said where the passage is, not which end they meant.
	_, doc = mint("work=tmdb%3A2316&from=S03E22+14%3A14&to=S03E22+2%3A22")
	if got, want := doc["href"].(string), "#/item/8?t=142&end=854"; !strings.HasSuffix(got, want) {
		t.Errorf("swapped pair = %q, want one ending %q", got, want)
	}

	// An episode code with no time is that episode's start.
	_, doc = mint("work=tmdb%3A2316&from=S03E22&to=S03E22+2%3A22")
	if got, want := doc["href"].(string), "#/item/8?t=0&end=142"; !strings.HasSuffix(got, want) {
		t.Errorf("bare code = %q, want one ending %q", got, want)
	}

	// A film's places are times in the one file it is.
	_, doc = mint("work=tmdb%3A109445&from=1%3A19%3A00&to=1%3A24%3A30")
	if got, want := doc["href"].(string), "#/item/4?t=4740&end=5070"; !strings.HasSuffix(got, want) {
		t.Errorf("film = %q, want one ending %q", got, want)
	}
	if got, want := doc["sentence"], "1:19:00 – 1:24:30 of Frozen"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}

	// A book's places are its spine sections, spelled as the chips spell
	// them; the link carries the locator grammar's spelling.
	_, doc = mint("work=book%3Ashirley-jackson-the-haunting-of-hill-house-1959&from=ch.+3&to=ch.+4")
	if got, want := doc["href"].(string), "#/item/3?from=ch%3A3&to=ch%3A4"; !strings.HasSuffix(got, want) {
		t.Errorf("book = %q, want one ending %q", got, want)
	}
	if got, want := doc["sentence"], "ch. 3 – ch. 4 of The Haunting of Hill House"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}

	// An audiobook's places are times in the part the work counts by.
	_, doc = mint("work=audiobook%3Afrank-herbert-dune-1965&from=40%3A00&to=1%3A19%3A00")
	if got, want := doc["href"].(string), "#/item/1?t=2400&end=4740"; !strings.HasSuffix(got, want) {
		t.Errorf("audiobook = %q, want one ending %q", got, want)
	}

	// A show has more than one file, so a bare time names no place in it —
	// and the refusal says how the places are spelled instead.
	code, doc = mint("work=tmdb%3A2316&from=2%3A22&to=14%3A14")
	if code != http.StatusBadRequest || doc["type"] != "no-such-place" {
		t.Errorf("a bare time on a show = %d: %v", code, doc)
	}
	if remedy, _ := doc["remedy"].(map[string]any); remedy["link"] == nil {
		t.Errorf("the refusal does not point at the work: %v", doc["remedy"])
	}
	// An episode the show does not have, and a place that is no place.
	if code, doc = mint("work=tmdb%3A2316&from=S09E99+0%3A00"); code != http.StatusBadRequest {
		t.Errorf("an episode that is not there = %d: %v", code, doc)
	}
	if code, doc = mint("work=tmdb%3A2316&from=the+bit+with+the+boat"); code != http.StatusBadRequest {
		t.Errorf("nonsense = %d: %v", code, doc)
	}
	// A work nobody has.
	if code, doc = mint("work=show%3Anot-here&from=S01E01"); code != http.StatusNotFound || doc["type"] != "no-such-work" {
		t.Errorf("an unknown work = %d: %v", code, doc)
	}
	// Neither place: a passage is two places, and the work document lists
	// the ones this work offers.
	if code, doc = mint("work=tmdb%3A2316"); code != http.StatusBadRequest || doc["type"] != "empty-passage" {
		t.Errorf("no places at all = %d: %v", code, doc)
	}
}

// Stopping ends the session: the row goes, and its address answers that it
// is gone rather than pretending it is there.
func TestStopEndsTheSession(t *testing.T) {
	_, h, doc := playing(t, idBeach, nil)
	self := doc["self"].(string)
	if got := href(t, doc, "actions", "stop"); got != self {
		t.Errorf("stop = %q, want the session's own address", got)
	}
	r := httptest.NewRequest(http.MethodDelete, self, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body)
	}
	w = get(t, h, self)
	if w.Code != http.StatusNotFound || decode(t, w)["type"] != "no-such-session" {
		t.Errorf("after the stop, GET = %d: %s", w.Code, w.Body)
	}
}
