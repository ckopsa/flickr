package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"flickr/internal/works"
)

// fixtureWorks is the fixture library as the resolver sees it: the works
// projection, nothing else. resolveRoute is pure, so the whole grammar is
// tested without a request.
func fixtureWorks(t *testing.T) []works.Work {
	t.Helper()
	srv, _ := fixtureServer(t)
	ws, err := srv.buildWorks()
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// Every hash spelling README "Deep links and passages" documents, against the
// fixture library: The Office S03E22 (item 8) and S03E23 (item 9), Frozen
// (item 4), the eight-section Hill House epub (item 3), Radiohead's album.
func TestResolveRoute(t *testing.T) {
	ws := fixtureWorks(t)
	const office = "/api/works/tmdb%3A2316"
	for _, tc := range []struct {
		name, hash string
		view       string
		self       string // the document the browser is to render
		passage    string // the resolved passage, as JSON
		autoplay   bool
		problem    string // the problem's type, "" when the hash resolves
		detail     string
		remedy     string // where the refusal sends the caller instead
	}{
		{name: "the library", hash: "#/", view: "library", self: "/api/library", passage: "null"},
		{name: "an empty hash is the library too", hash: "", view: "library", self: "/api/library", passage: "null"},
		{name: "an address nothing serves is the library", hash: "#/nonsense",
			view: "library", self: "/api/library", passage: "null"},
		{name: "a show", hash: "#/show/The%20Office", view: "work", self: office, passage: "null"},
		{name: "a show, matched case-insensitively", hash: "#/show/the%20office",
			view: "work", self: office, passage: "null"},
		{name: "a show with a query that says nothing about episodes",
			hash: "#/show/The%20Office?t=60", view: "work", self: office, passage: "null"},
		{name: "an episode by code becomes the item route", hash: "#/show/The%20Office?ep=S03E22",
			view: "item", self: "/api/items/8", passage: "null"},
		{name: "an episode code in any case", hash: "#/show/The%20Office?ep=s3e22",
			view: "item", self: "/api/items/8", passage: "null"},
		{name: "a scene addressed by episode code", hash: "#/show/The%20Office?ep=S03E22&t=142&end=854",
			view: "item", self: "/api/items/8", passage: `{"t":142,"end":854}`, autoplay: true},
		{name: "a run addressed by episode codes",
			hash: "#/show/The%20Office?ep=S03E22&t=142&end=854&until=S03E23",
			view: "item", self: "/api/items/8", passage: `{"t":142,"end":854,"until":9}`, autoplay: true},
		{name: "an item", hash: "#/item/4", view: "item", self: "/api/items/4", passage: "null"},
		{name: "a scene of an item", hash: "#/item/4?t=4740&end=5070",
			view: "item", self: "/api/items/4", passage: `{"t":4740,"end":5070}`, autoplay: true},
		{name: "decimal seconds", hash: "#/item/4?t=79.5&end=330.25",
			view: "item", self: "/api/items/4", passage: `{"t":79.5,"end":330.25}`, autoplay: true},
		{name: "an episode run by item id", hash: "#/item/8?t=120&until=9",
			view: "item", self: "/api/items/8", passage: `{"t":120,"until":9}`, autoplay: true},
		{name: "an end at or before the start is no bound", hash: "#/item/4?t=100&end=90",
			view: "item", self: "/api/items/4", passage: `{"t":100}`, autoplay: true},
		{name: "a text passage lands in the book's sections", hash: "#/item/3?from=ch%3A3&to=ch%3A4",
			view: "item", self: "/api/items/3",
			passage: `{"from":"ch:3","to":"ch:4","from_section":3,"to_section":4}`, autoplay: true},
		{name: "a percentage and a cfi land there too",
			hash: "#/item/3?from=pct%3A0.4&to=cfi%3Aepubcfi(%2F6%2F14!%2F4%2F2)",
			view: "item", self: "/api/items/3",
			passage:  `{"from":"pct:0.4","to":"cfi:epubcfi(/6/14!/4/2)","from_section":4,"to_section":7}`,
			autoplay: true},
		{name: "a page bound says nothing about an epub's sections", hash: "#/item/3?from=pg%3A213&to=pg%3A240",
			view: "item", self: "/api/items/3", passage: `{"from":"pg:213","to":"pg:240"}`, autoplay: true},
		{name: "a page passage lands on a PDF's pages", hash: "#/item/12?from=pg%3A213&to=pg%3A240",
			view: "item", self: "/api/items/12",
			passage:  `{"from":"pg:213","to":"pg:240","from_page":213,"to_page":240}`,
			autoplay: true},
		{name: "a percentage lands there too, and a section says nothing about pages",
			hash: "#/item/12?from=pct%3A0.5&to=ch%3A4", view: "item", self: "/api/items/12",
			passage:  `{"from":"pct:0.5","to":"ch:4","from_page":201}`,
			autoplay: true},
		{name: "a search", hash: "#/search/beach", view: "search",
			self: "/api/search?q=beach", passage: "null"},
		{name: "a search whose words have spaces in them",
			hash: "#/search/beach%20games", view: "search",
			self: "/api/search?q=beach+games", passage: "null"},
		{name: "a search nothing matches is still a search", hash: "#/search/ninjago",
			view: "search", self: "/api/search?q=ninjago", passage: "null"},
		{name: "an artist", hash: "#/artist/Radiohead",
			view: "artist", self: "/api/artists/Radiohead", passage: "null"},
		{name: "an artist, matched case-insensitively and spelled their way",
			hash: "#/artist/radiohead", view: "artist", self: "/api/artists/Radiohead", passage: "null"},

		{name: "a title that names no show", hash: "#/show/Ninjago",
			problem: "no-such-show", detail: `the library holds no show called "Ninjago"`,
			remedy: "/api/library"},
		{name: "an ep that names no episode", hash: "#/show/The%20Office?ep=S09E99&t=60",
			problem: "no-such-episode", detail: "This show has no episode S09E99.", remedy: office},
		{name: "an ep that is not an episode code", hash: "#/show/The%20Office?ep=finale",
			problem: "no-such-episode", detail: `"finale" is not an episode code like S02E05.`,
			remedy: office},
		{name: "a run until an episode that is not there",
			hash:    "#/show/The%20Office?ep=S03E22&until=S09E99",
			problem: "no-such-episode", detail: "This show has no episode S09E99 to run until.",
			remedy: office},
		{name: "an item id nothing holds", hash: "#/item/9999",
			problem: "no-such-item", detail: "the library holds no item 9999", remedy: "/api/library"},
		{name: "an artist nobody goes by", hash: "#/artist/Nobody",
			problem: "no-such-artist", detail: `the library holds no artist called "Nobody"`,
			remedy: "/api/library"},
		{name: "a hash that is not a link at all", hash: "#/show/%zz",
			problem: "bad-hash", remedy: "/api/library"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRoute(tc.hash, ws)
			if tc.problem != "" {
				if got.Problem == nil {
					t.Fatalf("%s resolved to %s, want the problem %q", tc.hash, got.View, tc.problem)
				}
				if got.Problem.Type != tc.problem {
					t.Errorf("problem = %q, want %q", got.Problem.Type, tc.problem)
				}
				if tc.detail != "" && got.Problem.Detail != tc.detail {
					t.Errorf("detail = %q, want %q", got.Problem.Detail, tc.detail)
				}
				if got.Problem.Remedy == nil || got.Problem.Remedy.Text == "" ||
					got.Problem.Remedy.Link == nil || got.Problem.Remedy.Link.Href != tc.remedy {
					t.Errorf("remedy = %+v, want a link to %s", got.Problem.Remedy, tc.remedy)
				}
				return
			}
			if got.Problem != nil {
				t.Fatalf("%s refused: %+v", tc.hash, got.Problem)
			}
			if got.View != tc.view {
				t.Errorf("view = %q, want %q", got.View, tc.view)
			}
			if self := targetSelf(t, got); self != tc.self {
				t.Errorf("document = %q, want %q", self, tc.self)
			}
			p, err := json.Marshal(got.Passage)
			if err != nil {
				t.Fatal(err)
			}
			if string(p) != tc.passage {
				t.Errorf("passage = %s, want %s", p, tc.passage)
			}
			if got.Autoplay != tc.autoplay {
				t.Errorf("autoplay = %v, want %v", got.Autoplay, tc.autoplay)
			}
		})
	}
}

// targetSelf is the address of the document the target names, without
// building it — the one thing a resolver test cares about.
func targetSelf(t *testing.T, target routeTarget) string {
	t.Helper()
	switch target.View {
	case "work":
		return workHref(target.Work.Key)
	case "artist":
		return artistEnvelope(target.Artist).Self()
	case "item":
		return itemHref(target.Item.ID)
	case "search":
		return searchHref(target.Query)
	}
	return libraryEnvelope().Self()
}

// The handler is the thin wrapper: the resolver's answer in the envelope,
// with the document itself where the resolver named one.
func TestRouteDocument(t *testing.T) {
	_, h := fixtureServer(t)
	fetch := func(hash string) (*httpRoute, int) {
		w := get(t, h, "/api/-/route?client_id=chris&hash="+queryEscape(hash))
		var doc httpRoute
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: %v (%s)", hash, err, w.Body)
		}
		return &doc, w.Code
	}

	doc, code := fetch("#/item/4?t=4740&end=5070")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if doc.Self != "/api/-/route?hash=%23%2Fitem%2F4%3Ft%3D4740%26end%3D5070" {
		t.Errorf("self = %q", doc.Self)
	}
	if doc.Kind != "route" || doc.View != "item" || !doc.Autoplay {
		t.Errorf("kind=%q view=%q autoplay=%v", doc.Kind, doc.View, doc.Autoplay)
	}
	if doc.Document.Self != "/api/items/4" || doc.Document.Kind != "item" ||
		doc.Document.Title != "Frozen" || doc.Document.MediaInfo == nil {
		t.Errorf("document = %+v", doc.Document)
	}
	if doc.Passage == nil || *doc.Passage.T != 4740 || *doc.Passage.End != 5070 {
		t.Errorf("passage = %+v", doc.Passage)
	}

	// The show form answers the episode's own document — the browser replaces
	// the hash with that item link and everything downstream is one path.
	doc, _ = fetch("#/show/The Office?ep=S03E22&t=142&until=S03E23")
	if doc.View != "item" || doc.Document.Self != "/api/items/8" {
		t.Errorf("show form = %s %s", doc.View, doc.Document.Self)
	}
	if doc.Passage == nil || doc.Passage.Until == nil || *doc.Passage.Until != 9 {
		t.Errorf("until = %+v", doc.Passage)
	}

	// A bare show route is the work document, and it is the profile's own:
	// the same document GET /api/works/{key} answers.
	doc, _ = fetch("#/show/The%20Office")
	if doc.View != "work" || doc.Document.Self != "/api/works/tmdb%3A2316" ||
		doc.Document.Profile != "chris" || doc.Document.Progress == nil {
		t.Errorf("show = %s %+v", doc.View, doc.Document)
	}

	// The library and the artist are links until hyper 3 serves their
	// documents; the relation is there now so a screen can be built on it.
	doc, _ = fetch("#/")
	if doc.View != "library" || doc.Document.Self != "/api/library" ||
		doc.Document.Links["library"].Href != "/api/library" || doc.Passage != nil {
		t.Errorf("library = %s %+v", doc.View, doc.Document)
	}
	// A search is a route like any other: the words are in the address, so
	// the results page can be sent to somebody.
	doc, _ = fetch("#/search/beach")
	if doc.View != "search" || doc.Document.Self != "/api/search?q=beach" ||
		doc.Document.Kind != "search" {
		t.Errorf("search = %s %+v", doc.View, doc.Document)
	}

	doc, _ = fetch("#/artist/Radiohead")
	if doc.View != "artist" || doc.Document.Self != "/api/artists/Radiohead" ||
		doc.Document.Title != "Radiohead" || doc.Document.Links["artist"].Href == "" {
		t.Errorf("artist = %s %+v", doc.View, doc.Document)
	}
}

// A hash that names nothing is refused the way every other miss is: the
// problem media type, a detail a person can read, and a remedy that says
// where to go instead.
func TestRouteProblems(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct {
		hash   string
		status int
		typ    string
	}{
		{"#/show/Ninjago", http.StatusNotFound, "no-such-show"},
		{"#/show/The Office?ep=S09E99", http.StatusNotFound, "no-such-episode"},
		{"#/item/9999", http.StatusNotFound, "no-such-item"},
		{"#/artist/Nobody", http.StatusNotFound, "no-such-artist"},
		{"#/show/%zz", http.StatusBadRequest, "bad-hash"},
	} {
		w := get(t, h, "/api/-/route?hash="+queryEscape(tc.hash))
		if w.Code != tc.status {
			t.Errorf("%s = %d, want %d (%s)", tc.hash, w.Code, tc.status, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s: content-type = %q", tc.hash, ct)
		}
		var p struct {
			Type   string `json:"type"`
			Detail string `json:"detail"`
			Remedy *struct {
				Text string                 `json:"text"`
				Link *struct{ Href string } `json:"link"`
			} `json:"remedy"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("%s: %v", tc.hash, err)
		}
		if p.Type != tc.typ || p.Detail == "" {
			t.Errorf("%s: type=%q detail=%q", tc.hash, p.Type, p.Detail)
		}
		if p.Remedy == nil || p.Remedy.Link == nil || p.Remedy.Link.Href == "" {
			t.Errorf("%s: remedy = %+v", tc.hash, p.Remedy)
		}
	}
}

// The route envelope, pinned: what a client can count on finding.
func TestRouteGolden(t *testing.T) {
	_, h := fixtureServer(t)
	w := get(t, h, "/api/-/route?client_id=chris&hash="+queryEscape("#/item/3?from=ch:3&to=ch:4"))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	golden(t, "route-text-passage", w.Body.Bytes())
}

// httpRoute is the route document as a client reads it: enough of the
// envelope to check the contract, not a second copy of the item golden.
type httpRoute struct {
	Self     string `json:"self"`
	Kind     string `json:"kind"`
	View     string `json:"view"`
	Autoplay bool   `json:"autoplay"`
	Document struct {
		Self      string                           `json:"self"`
		Kind      string                           `json:"kind"`
		Title     string                           `json:"title"`
		Profile   string                           `json:"profile"`
		Progress  json.RawMessage                  `json:"progress"`
		MediaInfo json.RawMessage                  `json:"media_info"`
		Links     map[string]struct{ Href string } `json:"links"`
	} `json:"document"`
	Passage *resolvedPassage `json:"passage"`
}

// queryEscape spells a hash into the route's own query parameter.
func queryEscape(hash string) string { return url.QueryEscape(hash) }
