package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flickr/internal/model"
	"flickr/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden documents under testdata/hyper")

// ── the fixture library ─────────────────────────────────────────────────
//
// One of everything the documents have to speak for: a film with chapters
// and two subtitle tracks, a show with two episodes and a featurette, a
// two-part audiobook, a two-track album, and a book with a titled spine.
// It is a real store.Library on a temp file — the handlers read through the
// store, so a fixture that skipped it would not be testing them — and no
// MinIO, ffmpeg or TMDB is touched by any read here.
//
// Item ids are the insertion order below, so the golden files' addresses stay
// put as long as this list does — which is why the two Radiohead records the
// artist shelf needs sit at the END of it rather than beside OK Computer:
// appending leaves every id, and so every golden written before them, alone.
const (
	idDunePart1 = 1
	idDunePart2 = 2
	idHillHouse = 3
	idFrozen    = 4
	idAirbag    = 5
	idParanoid  = 6
	idDeleted   = 7
	idBeach     = 8
	idTheJob    = 9
	idYou       = 10
	idTelex     = 11
	idFlatland  = 12
)

// arrived is one fixture file's arrival date. Real arrival times come from
// the bucket, which a test has none of, so the fixture spells them: they are
// what the `recently_added` row is ordered by, and a golden cannot be
// written against time.Now().
func arrived(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

func fixtureItems() []store.Item {
	return []store.Item{
		{
			ObjectKey: "Audiobooks/Frank Herbert/Dune (1965)/01 - Part 1.m4b",
			AddedAt:   arrived(2026, time.August, 1),
			ETag:      "e1", Size: 411000000,
			Identity: &model.Identity{Kind: "audiobook_part", Title: "Dune", Author: "Frank Herbert", Year: 1965, Part: 1},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumAudio, Container: "m4b", AudioCodec: "aac",
				DurationSeconds: 41400, BitrateBps: 64000, AudioChannels: 2,
				Chapters: []model.Chapter{
					{StartSeconds: 0, Title: "Chapter 01"},
					{StartSeconds: 2400, Title: "Chapter 02"},
					{StartSeconds: 4740, Title: "Chapter 03"},
				},
			},
		},
		{
			ObjectKey: "Audiobooks/Frank Herbert/Dune (1965)/02 - Part 2.m4b",
			AddedAt:   arrived(2026, time.August, 2),
			ETag:      "e2", Size: 390000000,
			Identity: &model.Identity{Kind: "audiobook_part", Title: "Dune", Author: "Frank Herbert", Year: 1965, Part: 2},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumAudio, Container: "m4b", AudioCodec: "aac",
				DurationSeconds: 39000, BitrateBps: 64000, AudioChannels: 2,
			},
		},
		{
			ObjectKey: "Books/Shirley Jackson/The Haunting of Hill House.epub",
			AddedAt:   arrived(2026, time.July, 15),
			ETag:      "e3", Size: 1200000,
			Identity: &model.Identity{Kind: "book", Title: "The Haunting of Hill House", Author: "Shirley Jackson", Year: 1959},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumText, Container: "epub", Sections: 8,
				Document: &model.Document{Title: "The Haunting of Hill House", Creator: "Shirley Jackson", Language: "en"},
				Chapters: []model.Chapter{
					{Title: "Cover"}, {Title: "Title Page"}, {Title: "Chapter One"},
					{Title: "Chapter Two"}, {Title: "The Hill"}, {Title: "The Tower"},
					{Title: "The Cellar"}, {Title: "Afterword"},
				},
			},
		},
		{
			ObjectKey: "Movies/Frozen (2013)/Frozen.mkv",
			AddedAt:   arrived(2026, time.September, 1),
			ETag:      "e4", Size: 8400000000,
			Identity: &model.Identity{Kind: "movie", Title: "Frozen", Year: 2013},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumVideo, Container: "mkv", VideoCodec: "h264", AudioCodec: "ac3",
				Width: 1920, Height: 816, DurationSeconds: 6420, BitrateBps: 10400000,
				AudioChannels: 6, FPS: 23.976,
				Chapters: []model.Chapter{
					{StartSeconds: 0, Title: "Frozen Heart"},
					{StartSeconds: 1200, Title: "Do You Want to Build a Snowman"},
					{StartSeconds: 4740, Title: "Let It Go"},
				},
				Subtitles: []model.SubtitleTrack{
					{Ordinal: 0, Codec: "subrip", Language: "eng", Title: "English", Supported: true},
					{Ordinal: 1, Codec: "hdmv_pgs_subtitle", Language: "eng", Title: "English (full)"},
				},
				AudioTracks: []model.AudioTrack{
					{Ordinal: 0, Codec: "ac3", Language: "eng", Channels: 6, Default: true},
				},
			},
		},
		{
			ObjectKey: "Music/Radiohead/OK Computer (1997)/01 Airbag.flac",
			AddedAt:   arrived(2026, time.June, 10),
			ETag:      "e5", Size: 34000000,
			Identity: &model.Identity{Kind: "track", Title: "OK Computer", Author: "Radiohead",
				Year: 1997, Part: 1, TrackTitle: "Airbag"},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumAudio, Container: "flac", AudioCodec: "flac",
				DurationSeconds: 284, BitrateBps: 960000, AudioChannels: 2,
			},
		},
		{
			ObjectKey: "Music/Radiohead/OK Computer (1997)/02 Paranoid Android.flac",
			AddedAt:   arrived(2026, time.June, 10),
			ETag:      "e6", Size: 46000000,
			Identity: &model.Identity{Kind: "track", Title: "OK Computer", Author: "Radiohead",
				Year: 1997, Part: 2, TrackTitle: "Paranoid Android"},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumAudio, Container: "flac", AudioCodec: "flac",
				DurationSeconds: 383, BitrateBps: 960000, AudioChannels: 2,
			},
		},
		{
			ObjectKey: "Shows/The Office (2005)/Featurettes/Deleted Scenes.mkv",
			AddedAt:   arrived(2026, time.August, 20),
			ETag:      "e7", Size: 900000000,
			Identity: &model.Identity{Kind: "extra", Title: "The Office", Season: 3, Episode: 22},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumVideo, Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
				Width: 1280, Height: 720, DurationSeconds: 900, BitrateBps: 4000000, AudioChannels: 2,
			},
		},
		{
			ObjectKey: "Shows/The Office (2005)/Season 3/S03E22 - Beach Games.mkv",
			AddedAt:   arrived(2026, time.August, 18),
			ETag:      "e8", Size: 2400000000,
			Identity: &model.Identity{Kind: "episode", Title: "The Office", Year: 2005, Season: 3, Episode: 22},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumVideo, Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
				Width: 1280, Height: 720, DurationSeconds: 2640, BitrateBps: 5200000, AudioChannels: 2,
				Chapters: []model.Chapter{
					{StartSeconds: 0, Title: "Cold open"},
					{StartSeconds: 142, Title: "Titles"},
					{StartSeconds: 173, Title: "Act one"},
					{StartSeconds: 2580, Title: "End credits"},
				},
				Subtitles: []model.SubtitleTrack{
					{Ordinal: 0, Codec: "subrip", Language: "eng", Title: "English", Supported: true,
						External: true, ObjectKey: "Shows/The Office (2005)/Season 3/S03E22 - Beach Games.en.srt"},
				},
			},
		},
		{
			ObjectKey: "Shows/The Office (2005)/Season 3/S03E23 - The Job.mkv",
			AddedAt:   arrived(2026, time.August, 19),
			ETag:      "e9", Size: 2760000000,
			Identity: &model.Identity{Kind: "episode", Title: "The Office", Year: 2005, Season: 3, Episode: 23},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumVideo, Container: "mkv", VideoCodec: "h264", AudioCodec: "aac",
				Width: 1280, Height: 720, DurationSeconds: 2760, BitrateBps: 5200000, AudioChannels: 2,
			},
		},
		// Two more records by the same artist, so a shelf is a shelf: an
		// EARLIER album (the year, not the key, is the shelf's order) and one
		// nobody dated (which sorts last, after every dated record).
		{
			ObjectKey: "Music/Radiohead/Pablo Honey (1993)/01 You.flac",
			AddedAt:   arrived(2026, time.June, 5),
			ETag:      "e10", Size: 29000000,
			Identity: &model.Identity{Kind: "track", Title: "Pablo Honey", Author: "Radiohead",
				Year: 1993, Part: 1, TrackTitle: "You"},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumAudio, Container: "flac", AudioCodec: "flac",
				DurationSeconds: 208, BitrateBps: 960000, AudioChannels: 2,
			},
		},
		{
			ObjectKey: "Music/Radiohead/The Bends/01 Planet Telex.flac",
			AddedAt:   arrived(2026, time.September, 3),
			ETag:      "e11", Size: 32000000,
			Identity: &model.Identity{Kind: "track", Title: "The Bends", Author: "Radiohead",
				Part: 1, TrackTitle: "Planet Telex"},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumAudio, Container: "flac", AudioCodec: "flac",
				DurationSeconds: 259, BitrateBps: 960000, AudioChannels: 2,
			},
		},
		// The other kind of book: a PDF, which counts in pages where an EPUB
		// counts in spine sections and has no contents list at all.
		{
			ObjectKey: "Books/Edwin A. Abbott/Flatland.pdf",
			AddedAt:   arrived(2026, time.May, 1),
			ETag:      "e12", Size: 3400000,
			Identity: &model.Identity{Kind: "book", Title: "Flatland", Author: "Edwin A. Abbott", Year: 1884},
			MediaInfo: &model.MediaInfo{
				Medium: model.MediumText, Container: "pdf", PageCount: 400,
				Document: &model.Document{Title: "Flatland", Creator: "Edwin A. Abbott"},
			},
		},
	}
}

// fixtureServer stands a server up over a temporary library and playback
// state, and hands back the real routing table. The enricher is left off
// (no TMDB_API_KEY, the ordinary state of a development box), which is why
// every item document below says `enrich` is unavailable and why.
func fixtureServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	library, err := store.OpenLibrary(filepath.Join(dir, "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.OpenState(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	items := fixtureItems()
	if err := library.UpsertBatch(items); err != nil {
		t.Fatal(err)
	}
	// The goldens address items by id, so the ids the insert handed out have
	// to be the ones this file names: one per fixture row, in the order the
	// rows are written (ListItems answers in object-key order, which is a
	// different order once a row is appended rather than inserted).
	stored, err := library.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(items) {
		t.Fatalf("stored %d items, want %d", len(stored), len(items))
	}
	byID := map[int64]string{}
	for _, it := range stored {
		byID[it.ID] = it.ObjectKey
	}
	for i, it := range items {
		if got := byID[int64(i+1)]; got != it.ObjectKey {
			t.Fatalf("id %d is %q, want %q; the fixture rows are its ids", i+1, got, it.ObjectKey)
		}
	}
	// TMDB enrichment, written the way the enrichment pass writes it: the
	// film, and the show's episodes carrying the SHOW's id and their own
	// title and still.
	for _, e := range []struct {
		id   int64
		enr  model.Enrichment
		kind string
	}{
		{idFrozen, model.Enrichment{Version: 3, TMDBID: 109445, Title: "Frozen", Year: 2013,
			Overview: "Young princess Anna sets off to find her sister.", HasPoster: true,
			HasBackdrop: true, Genres: []string{"Animation", "Family", "Adventure"},
			RuntimeMinutes: 102, Certification: "PG",
			Cast: []string{"Kristen Bell", "Idina Menzel", "Jonathan Groff"}}, "movie"},
		{idBeach, model.Enrichment{Version: 3, TMDBID: 2316, Title: "The Office", Year: 2005,
			Overview: "A mockumentary on a group of office workers.", HasPoster: true,
			HasBackdrop: true, Genres: []string{"Comedy"},
			RuntimeMinutes: 22, Certification: "TV-14",
			Cast:            []string{"Steve Carell", "Rainn Wilson", "John Krasinski"},
			EpisodeTitle:    "Beach Games",
			EpisodeOverview: "Michael takes the office to the beach.", HasStill: true}, "episode"},
		{idTheJob, model.Enrichment{Version: 3, TMDBID: 2316, Title: "The Office", Year: 2005,
			Overview: "A mockumentary on a group of office workers.", HasPoster: true,
			HasBackdrop: true, Genres: []string{"Comedy"},
			RuntimeMinutes: 22, Certification: "TV-14",
			Cast:            []string{"Steve Carell", "Rainn Wilson", "John Krasinski"},
			EpisodeTitle:    "The Job",
			EpisodeOverview: "Jim and Karen head to New York.", HasStill: true}, "episode"},
	} {
		enr := e.enr
		if err := library.SetEnrichment(e.id, &enr, items[e.id-1].Identity); err != nil {
			t.Fatal(err)
		}
	}
	// The one episode with no subtitles of its own is the one the
	// transcription stage has been over: its document lists what whisper
	// wrote as a track like any other, addressed by the word rather than a
	// stream number (README 18).
	if err := library.SetTranscript(store.Transcript{
		ItemID: idTheJob, ETag: "e9", Language: "en", Model: "ggml-base.en.bin",
		GeneratedAt: time.Date(2026, time.September, 4, 2, 30, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	// And the lines of it, parsed out of that WebVTT the way the stage does:
	// what the episode SAYS, which no title carries. The first of them is
	// what the search golden's own query finds in the dialogue row.
	if err := library.SetCues(idTheJob, []store.Cue{
		{ItemID: idTheJob, Start: 61.5, End: 64, Text: "He took the job in New York."},
		{ItemID: idTheJob, Start: 302, End: 304.25, Text: "Bears. Beets. Battlestar Galactica."},
	}); err != nil {
		t.Fatal(err)
	}
	// One profile who is partway through the show, partway through the
	// audiobook, and seven sections into the book.
	//
	// The ORDER of these three writes is the resume shelf's order, reversed:
	// /api/continue is most-recent first, and updated_at is a millisecond
	// clock, so three writes in one test may or may not land in the same
	// millisecond. Written oldest-first in DESCENDING item id, the shelf reads
	// 1, 3, 8 whether the clock separated them or the tie-break (item id,
	// ascending) had to — and the continue golden holds still.
	if err := state.SetPosition(idBeach, "chris", 745); err != nil {
		t.Fatal(err)
	}
	if err := state.SetPlace(idHillHouse, "chris", 0,
		&model.Locator{CFI: "epubcfi(/6/14[ch07]!/4/2/1:0)", Section: 7, Fraction: 0.34}); err != nil {
		t.Fatal(err)
	}
	if err := state.SetPosition(idDunePart1, "chris", 4800); err != nil {
		t.Fatal(err)
	}
	// Two things put by for later: the film, and the book. The ORDER is the
	// same trick the three writes above play — My List reads newest first and
	// breaks a tie by key, so the LATER save has the smaller key and the
	// shelf reads [book, film] whether the millisecond clock separated the
	// two writes or the tie-break had to.
	for _, key := range []string{"tmdb:109445", "book:shirley-jackson-the-haunting-of-hill-house-1959"} {
		if err := state.Save("chris", key); err != nil {
			t.Fatal(err)
		}
	}

	srv := &server{library: library, state: state, policy: model.DefaultPolicy()}
	return srv, srv.routes()
}

// ── the goldens ─────────────────────────────────────────────────────────

// golden compares one document against testdata/hyper/<name>.json, or
// rewrites it — and the client's copy of it, see shareGolden — under -update.
// Documents are compared indented so a diff shows the field that moved rather
// than the whole line.
func golden(t *testing.T, name string, body []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatalf("%s: response is not JSON: %v\n%s", name, err, body)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", "hyper", name+".json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		shareGolden(t, name, pretty.Bytes())
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run: go test ./cmd/server -run TestHyperGolden -update)", name, err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Errorf("%s differs from the golden.\n--- want ---\n%s\n--- got ---\n%s", name, want, pretty.Bytes())
	}
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestHyperGolden(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct{ name, target string }{
		{"root", "/api/?client_id=chris"},
		{"item-film", "/api/items/4?client_id=chris"},
		// The episode that had no subtitles of its own, and now has a
		// generated transcript in the same list.
		{"item-episode", "/api/items/9?client_id=chris"},
		{"work-show", "/api/works/tmdb%3A2316?client_id=chris"},
		{"work-audiobook", "/api/works/audiobook%3Afrank-herbert-dune-1965?client_id=chris"},
		{"work-album", "/api/works/album%3Aradiohead-ok-computer-1997?client_id=chris"},
		{"work-book", "/api/works/book%3Ashirley-jackson-the-haunting-of-hill-house-1959?client_id=chris"},
		{"library", "/api/library?client_id=chris"},
		// "he" rather than "the": the one query this library answers in every
		// row the search has, the artist's shelf and the dialogue among them.
		{"search", "/api/search?q=he&client_id=chris"},
		// The same dialogue, asked of one file: the finder on its page.
		{"lines", "/api/items/9/lines?q=job&client_id=chris"},
		{"artist", "/api/artists/Radiohead?client_id=chris"},
		{"continue", "/api/continue?client_id=chris"},
		{"list", "/api/list?client_id=chris"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(t, h, tc.target)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", tc.target, w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q", ct)
			}
			golden(t, tc.name, w.Body.Bytes())
		})
	}
}

// The root is the one address a client knows by heart, and it is the same
// document to a stranger — minus the profile, and with a resume link that
// does not pretend to know who is asking.
func TestRootWithoutAProfile(t *testing.T) {
	_, h := fixtureServer(t)
	var doc struct {
		Self    string                     `json:"self"`
		Kind    string                     `json:"kind"`
		Profile string                     `json:"profile"`
		Links   map[string]json.RawMessage `json:"links"`
		Actions map[string]json.RawMessage `json:"actions"`
	}
	w := get(t, h, "/api/")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Self != "/api/" || doc.Kind != "root" {
		t.Errorf("self=%q kind=%q", doc.Self, doc.Kind)
	}
	if doc.Profile != "" {
		t.Errorf("anonymous root carries profile %q", doc.Profile)
	}
	if got := string(doc.Links["continue"]); got != `{"href":"/api/continue","title":"Continue watching"}` {
		t.Errorf("continue link = %s", got)
	}
	for _, rel := range []string{"library", "continue", "artists", "works", "items", "scan", "system"} {
		if _, ok := doc.Links[rel]; !ok {
			t.Errorf("root has no %q link", rel)
		}
	}
	if _, ok := doc.Actions["scan"]; !ok {
		t.Error("root has no scan action")
	}

	// The profile may also ride a cookie, for a caller that would rather not
	// spell it on every URL.
	r := httptest.NewRequest(http.MethodGet, "/api/", nil)
	r.AddCookie(&http.Cookie{Name: "client_id", Value: "dana"})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Profile != "dana" {
		t.Errorf("cookie profile = %q, want dana", doc.Profile)
	}
}

// "/api/{$}" is the exact path: an address below it that nothing serves is
// still a miss, not the root document wearing the wrong self.
func TestRootDoesNotSwallowTheAPISubtree(t *testing.T) {
	_, h := fixtureServer(t)
	if w := get(t, h, "/api/nothing-here"); w.Code == http.StatusOK &&
		strings.Contains(w.Body.String(), `"kind":"root"`) {
		t.Errorf("/api/nothing-here answered with the root document")
	}
}

// A refusal is an answer: the problem media type, the problem's own status,
// and a remedy that says where to go instead.
func TestHyperProblems(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct {
		target string
		status int
		typ    string
	}{
		{"/api/items/9999", http.StatusNotFound, "no-such-item"},
		{"/api/items/not-a-number", http.StatusBadRequest, "bad-item-id"},
		{"/api/works/show%3Anope", http.StatusNotFound, "no-such-work"},
	} {
		w := get(t, h, tc.target)
		if w.Code != tc.status {
			t.Errorf("GET %s = %d, want %d", tc.target, w.Code, tc.status)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("GET %s: content-type = %q", tc.target, ct)
		}
		var p struct {
			Type   string `json:"type"`
			Status int    `json:"status"`
			Remedy *struct {
				Text string `json:"text"`
				Link *struct {
					Href string `json:"href"`
				} `json:"link"`
			} `json:"remedy"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("GET %s: %v", tc.target, err)
		}
		if p.Type != tc.typ || p.Status != tc.status {
			t.Errorf("GET %s: type=%q status=%d", tc.target, p.Type, p.Status)
		}
		if p.Remedy == nil || p.Remedy.Text == "" || p.Remedy.Link == nil || p.Remedy.Link.Href != "/api/library" {
			t.Errorf("GET %s: remedy = %+v", tc.target, p.Remedy)
		}
	}
}

// An item's neighbours are the work's own order, bonus material aside — the
// order "up next" walks. The featurette sits outside it and has none.
func TestItemNeighbours(t *testing.T) {
	_, h := fixtureServer(t)
	links := func(id string) map[string]struct{ Href, Title string } {
		w := get(t, h, "/api/items/"+id)
		var doc struct {
			Links map[string]struct{ Href, Title string } `json:"links"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("item %s: %v (%s)", id, err, w.Body)
		}
		return doc.Links
	}
	beach := links("8")
	if beach["next"].Href != "/api/items/9" || beach["next"].Title != "The Job" {
		t.Errorf("S03E22 next = %+v", beach["next"])
	}
	if _, ok := beach["prev"]; ok {
		t.Errorf("S03E22 is the first member the work walks, so it has no prev: %+v", beach["prev"])
	}
	if beach["work"].Href != "/api/works/tmdb%3A2316" || beach["work"].Title != "The Office" {
		t.Errorf("S03E22 work = %+v", beach["work"])
	}
	if beach["still"].Href != "/api/items/8/still" {
		t.Errorf("S03E22 still = %+v", beach["still"])
	}
	job := links("9")
	if job["prev"].Href != "/api/items/8" || job["prev"].Title != "Beach Games" {
		t.Errorf("S03E23 prev = %+v", job["prev"])
	}
	if _, ok := job["next"]; ok {
		t.Errorf("S03E23 is the last member, so it has no next: %+v", job["next"])
	}
	deleted := links("7")
	if _, ok := deleted["prev"]; ok {
		t.Errorf("bonus material sits outside the order and has no prev: %+v", deleted["prev"])
	}
	if _, ok := deleted["next"]; ok {
		t.Errorf("bonus material sits outside the order and has no next: %+v", deleted["next"])
	}
}

// A kind that lacks an action says so, in words, where the action would be —
// a button that is not there, and why.
func TestUnavailableActions(t *testing.T) {
	_, h := fixtureServer(t)
	kinds := func(target string) (map[string]json.RawMessage, map[string]struct{ Reason string }) {
		w := get(t, h, target)
		var doc struct {
			Actions     map[string]json.RawMessage         `json:"actions"`
			Unavailable map[string]struct{ Reason string } `json:"unavailable"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: %v (%s)", target, err, w.Body)
		}
		return doc.Actions, doc.Unavailable
	}

	acts, gone := kinds("/api/items/4") // the film
	if _, ok := acts["play"]; !ok {
		t.Error("a film has no play action")
	}
	if gone["read"].Reason != "a film is played, not read" {
		t.Errorf("film read = %q", gone["read"].Reason)
	}
	if gone["enrich"].Reason == "" {
		t.Error("with no TMDB key the film should say why enrich is off")
	}

	acts, gone = kinds("/api/items/3") // the book
	if _, ok := acts["read"]; !ok {
		t.Error("a book has no read action")
	}
	if gone["play"].Reason != "a book is read, not played" {
		t.Errorf("book play = %q", gone["play"].Reason)
	}
	if !strings.Contains(gone["enrich"].Reason, "TMDB") {
		t.Errorf("book enrich = %q", gone["enrich"].Reason)
	}

	acts, gone = kinds("/api/items/1") // an audiobook part
	if _, ok := acts["play"]; !ok {
		t.Error("an audiobook part has no play action")
	}
	if gone["read"].Reason != "an audiobook is played, not read" {
		t.Errorf("audiobook read = %q", gone["read"].Reason)
	}
}

// A work's places are tokens the passage grammar reads back, each with the
// label a chip shows in its stead.
func TestWorkPlaces(t *testing.T) {
	_, h := fixtureServer(t)
	places := func(target string) []place {
		w := get(t, h, target)
		var doc struct {
			Places []place `json:"places"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: %v (%s)", target, err, w.Body)
		}
		return doc.Places
	}
	for _, tc := range []struct {
		name, target string
		want         []place
	}{
		{"a show's episodes, the first second of each", "/api/works/tmdb%3A2316", []place{
			{Token: "S03E22 0:00", Label: "S03E22 · Beach Games"},
			{Token: "S03E23 0:00", Label: "S03E23 · The Job"},
		}},
		{"a film's chapters as times", "/api/works/tmdb%3A109445", []place{
			{Token: "0:00", Label: "Frozen Heart"},
			{Token: "20:00", Label: "Do You Want to Build a Snowman"},
			{Token: "1:19:00", Label: "Let It Go"},
		}},
		{"an audiobook's chapters, from the part that has them",
			"/api/works/audiobook%3Afrank-herbert-dune-1965", []place{
				{Token: "0:00", Label: "Chapter 01"},
				{Token: "40:00", Label: "Chapter 02"},
				{Token: "1:19:00", Label: "Chapter 03"},
			}},
		{"a book's sections, counted the way ch:<n> counts them",
			"/api/works/book%3Ashirley-jackson-the-haunting-of-hill-house-1959", []place{
				{Token: "ch. 1", Label: "Cover"},
				{Token: "ch. 2", Label: "Title Page"},
				{Token: "ch. 3", Label: "Chapter One"},
				{Token: "ch. 4", Label: "Chapter Two"},
				{Token: "ch. 5", Label: "The Hill"},
				{Token: "ch. 6", Label: "The Tower"},
				{Token: "ch. 7", Label: "The Cellar"},
				{Token: "ch. 8", Label: "Afterword"},
			}},
		{"tracks carry no chapter marks, so the album offers no places",
			"/api/works/album%3Aradiohead-ok-computer-1997", []place{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := places(tc.target)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d places, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("place %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The work's play action starts where the profile would: at the
// representative for a stranger, at the first unfinished member for someone
// who has been here before.
func TestWorkPlayTarget(t *testing.T) {
	_, h := fixtureServer(t)
	play := func(target string) (href, label string) {
		w := get(t, h, target)
		var doc struct {
			Actions map[string]struct{ Href, Label string } `json:"actions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: %v (%s)", target, err, w.Body)
		}
		return doc.Actions["play"].Href, doc.Actions["play"].Label
	}
	if href, label := play("/api/works/tmdb%3A2316"); href != "/api/items/8/play" || label != "▶ Play" {
		t.Errorf("anonymous show play = %s %q", href, label)
	}
	if href, label := play("/api/works/tmdb%3A2316?client_id=chris"); href != "/api/items/8/play" || label != "▶ Resume" {
		t.Errorf("chris is 745s into S03E22, so play resumes there: %s %q", href, label)
	}
	// Finish that episode and the work moves on to the next one.
	srv, h2 := fixtureServer(t)
	if err := srv.state.SetPosition(idBeach, "chris", 2600); err != nil {
		t.Fatal(err)
	}
	w := get(t, h2, "/api/works/tmdb%3A2316?client_id=chris")
	var doc struct {
		Progress struct {
			Status string                       `json:"status"`
			Next   struct{ Href, Title string } `json:"next"`
		} `json:"progress"`
		Actions map[string]struct{ Href string } `json:"actions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Actions["play"].Href != "/api/items/9/play" {
		t.Errorf("after finishing S03E22, play = %s", doc.Actions["play"].Href)
	}
	if doc.Progress.Next.Href != "/api/items/9" || doc.Progress.Next.Title != "S03E23 · The Job" {
		t.Errorf("progress.next = %+v", doc.Progress.Next)
	}
}

// A work document read by a stranger has no profile and no progress; the
// same document read by the profile carries both.
func TestWorkProgressIsPerProfile(t *testing.T) {
	_, h := fixtureServer(t)
	var anon, mine struct {
		Profile  string `json:"profile"`
		Progress *struct {
			Fraction float64 `json:"fraction"`
			Text     string  `json:"text"`
		} `json:"progress"`
	}
	if err := json.Unmarshal(get(t, h, "/api/works/tmdb%3A2316").Body.Bytes(), &anon); err != nil {
		t.Fatal(err)
	}
	if anon.Profile != "" || anon.Progress != nil {
		t.Errorf("anonymous work document: profile=%q progress=%+v", anon.Profile, anon.Progress)
	}
	if err := json.Unmarshal(get(t, h, "/api/works/tmdb%3A2316?client_id=chris").Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if mine.Profile != "chris" || mine.Progress == nil || mine.Progress.Text != "S03E22 · 12:25" {
		t.Errorf("chris's work document: profile=%q progress=%+v", mine.Profile, mine.Progress)
	}
}

// Nothing in this bead touches the routes that were already there.
func TestExistingRoutesUnchanged(t *testing.T) {
	_, h := fixtureServer(t)
	// /api/items is still a bare array of store items.
	var items []store.Item
	if err := json.Unmarshal(get(t, h, "/api/items").Body.Bytes(), &items); err != nil {
		t.Fatalf("/api/items: %v", err)
	}
	if len(items) != len(fixtureItems()) {
		t.Errorf("/api/items returned %d items", len(items))
	}
	// /api/works/{key}/items is still that work's members, not a document.
	var members []store.Item
	if err := json.Unmarshal(get(t, h, "/api/works/tmdb%3A2316/items").Body.Bytes(), &members); err != nil {
		t.Fatalf("/api/works/{key}/items: %v", err)
	}
	if len(members) != 3 || members[0].ID != idBeach {
		t.Errorf("/api/works/tmdb%%3A2316/items = %d members, first id %d", len(members), members[0].ID)
	}
}

// A show's members say where they sit and whether they have been seen: the
// season and episode numbers a pane groups its rows by, and `watched` — the
// same 90% rule works.Finished keeps everywhere else. A watched episode has
// no `resume` left (resumeOf drops the credits), which is why the tick cannot
// be read off one.
func TestShowMembersCarrySeasonEpisodeAndWatched(t *testing.T) {
	srv, h := fixtureServer(t)
	if err := srv.state.SetPosition(idTheJob, "chris", 2755); err != nil { // the credits of 2760
		t.Fatal(err)
	}
	var doc struct {
		Members []struct {
			ID      int64 `json:"id"`
			Season  int   `json:"season"`
			Episode int   `json:"episode"`
			Watched bool  `json:"watched"`
			Resume  *struct {
				PositionSeconds float64 `json:"position_seconds"`
			} `json:"resume"`
		} `json:"members"`
	}
	if err := json.Unmarshal(get(t, h, "/api/works/tmdb%3A2316?client_id=chris").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	type place struct {
		season, episode int
		watched, resume bool
	}
	got := map[int64]place{}
	for _, m := range doc.Members {
		got[m.ID] = place{m.Season, m.Episode, m.Watched, m.Resume != nil}
	}
	if g := got[idBeach]; g.season != 3 || g.episode != 22 || g.watched || !g.resume {
		t.Errorf("S03E22, 745s in and unfinished: %+v", g)
	}
	if g := got[idTheJob]; g.season != 3 || g.episode != 23 || !g.watched || g.resume {
		t.Errorf("S03E23 watched to the credits, with no place left to resume: %+v", g)
	}
	// The item's OWN document carries the whole identity instead, so it does
	// not repeat the two numbers a list groups by.
	var item map[string]any
	if err := json.Unmarshal(get(t, h, "/api/items/9?client_id=chris").Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if _, ok := item["season"]; ok {
		t.Error("an item's own document repeats season; its identity already says it")
	}
}
