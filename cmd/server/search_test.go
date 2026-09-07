package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// searchDoc is the search document as a client reads it.
type searchDoc struct {
	Self   string `json:"self"`
	Kind   string `json:"kind"`
	Query  string `json:"query"`
	Count  int    `json:"count"`
	Groups []struct {
		Key   string `json:"key"`
		Title string `json:"title"`
		Count int    `json:"count"`
		Items []struct {
			Self     string `json:"self"`
			Kind     string `json:"kind"`
			Title    string `json:"title"`
			Label    string `json:"label"`
			Subtitle string `json:"subtitle"`
			WorkKind string `json:"work_kind"`
			WorkKey  string `json:"work_key"`
			ItemID   int64  `json:"item_id"`
			Links    map[string]struct {
				Href string `json:"href"`
			} `json:"links"`
		} `json:"items"`
	} `json:"groups"`
}

// hits is every result as "group/title", in the document's own order — the
// whole of what a search answers, said in one line. The title is read back
// out of the envelope's own JSON, which is what a client reads.
func hits(t *testing.T, groups []searchGroup) []string {
	t.Helper()
	var out []string
	for _, g := range groups {
		for _, en := range g.Items {
			b, err := json.Marshal(en)
			if err != nil {
				t.Fatal(err)
			}
			var f struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(b, &f); err != nil {
				t.Fatal(err)
			}
			out = append(out, g.Key+"/"+f.Title)
		}
	}
	return out
}

// The matching is one pure function, and this is its table: what each kind
// of thing is found BY, which row it lands in, and what is not answered
// twice. The library is the fixture's — The Office with two episodes and a
// featurette, Frozen, Dune in two parts, three Radiohead records, two books.
func TestSearchLibrary(t *testing.T) {
	ws := fixtureWorks(t)
	for _, tc := range []struct {
		name, q string
		want    []string
	}{
		{
			name: "an episode by its own title, which no tile carries",
			q:    "beach",
			want: []string{"episodes/Beach Games"},
		},
		{
			name: "a track by its name",
			q:    "paranoid",
			want: []string{"tracks/Paranoid Android"},
		},
		{
			name: "case is folded on both sides",
			q:    "BEACH GAMES",
			want: []string{"episodes/Beach Games"},
		},
		{
			name: "an episode by its label — the code the file was named for",
			q:    "s03e23",
			want: []string{"episodes/The Job"},
		},
		{
			name: "a work by its title, and its members by theirs",
			q:    "dune",
			want: []string{"works/Dune"},
		},
		{
			name: "everything an author made",
			q:    "frank herbert",
			want: []string{"works/Dune", "parts/Part 1", "parts/Part 2"},
		},
		{
			name: "a show by what it is about",
			q:    "mockumentary",
			want: []string{"works/The Office"},
		},
		{
			name: "an episode by its own synopsis",
			q:    "michael takes the office",
			want: []string{"episodes/Beach Games"},
		},
		{
			name: "a book is answered once, among the books",
			q:    "flatland",
			want: []string{"books/Flatland"},
		},
		{
			name: "an artist by name: the shelf, and everything on it",
			q:    "radiohead",
			want: []string{"works/OK Computer", "works/Pablo Honey", "works/The Bends",
				"artists/Radiohead",
				"tracks/Airbag", "tracks/Paranoid Android", "tracks/You", "tracks/Planet Telex"},
		},
		{
			name: "a substring, not a word",
			q:    "adio",
			want: []string{"works/OK Computer", "works/Pablo Honey", "works/The Bends",
				"artists/Radiohead",
				"tracks/Airbag", "tracks/Paranoid Android", "tracks/You", "tracks/Planet Telex"},
		},
		{
			name: "an album's title is not its artist's name",
			q:    "pablo honey",
			want: []string{"works/Pablo Honey"},
		},
		{name: "nothing matches", q: "ninjago"},
		{name: "an empty query finds nothing at all", q: ""},
		{name: "and neither does a blank one", q: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hits(t, searchLibrary(tc.q, ws))
			if strings.Join(got, ", ") != strings.Join(tc.want, ", ") {
				t.Errorf("search(%q) = [%s], want [%s]",
					tc.q, strings.Join(got, ", "), strings.Join(tc.want, ", "))
			}
		})
	}
}

// A group nothing landed in is not a heading, and the rows come in the
// document's own order whatever order the library was walked in.
func TestSearchGroupOrder(t *testing.T) {
	ws := fixtureWorks(t)
	var keys []string
	for _, g := range searchLibrary("e", ws) {
		keys = append(keys, g.Key)
		if g.Title == "" {
			t.Errorf("group %q has no heading", g.Key)
		}
		if g.Count != len(g.Items) {
			t.Errorf("group %q: count = %d, %d items", g.Key, g.Count, len(g.Items))
		}
	}
	if want := "works, artists, episodes, tracks, parts, books"; strings.Join(keys, ", ") != want {
		t.Errorf("groups = [%s], want [%s]", strings.Join(keys, ", "), want)
	}
	if got := searchLibrary("ninjago", ws); len(got) != 0 {
		t.Errorf("a search that matches nothing offers no rows: %+v", got)
	}
}

// Every result says where it goes and what to draw, and the ids a hash is
// spelled from are on it: nothing composes an address to open one.
func TestSearchDocument(t *testing.T) {
	_, h := fixtureServer(t)
	// "he" is "the" plus the artist: one query that lands something in every
	// row the search has, which is what the golden is written from too.
	w := get(t, h, "/api/search?q=he&client_id=chris")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/search = %d: %s", w.Code, w.Body)
	}
	var doc searchDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "search" || doc.Query != "he" || doc.Self != "/api/search?q=he" {
		t.Errorf("the document = %+v", doc)
	}
	n := 0
	for _, g := range doc.Groups {
		n += g.Count
		for _, en := range g.Items {
			if en.Links["self"].Href != en.Self {
				t.Errorf("%q: links.self = %q, self = %q", en.Title, en.Links["self"].Href, en.Self)
			}
			if en.Title == "" || en.Links["artwork"].Href == "" {
				t.Errorf("%q is not drawable: %+v", g.Key, en)
			}
			// An artist's shelf is opened by the name; everything else
			// carries the member a tap opens at.
			if g.Key != "artists" && en.ItemID == 0 {
				t.Errorf("%q: no item to open: %+v", g.Key, en)
			}
		}
	}
	if doc.Count != n {
		t.Errorf("count = %d, %d hits", doc.Count, n)
	}

	// Title → the first group it landed in. One title can answer twice — an
	// episode found by its name is also an episode whose dialogue says the
	// word — and the row it is FOUND in is the first one.
	byTitle := map[string]string{}
	for _, g := range doc.Groups {
		for _, en := range g.Items {
			if _, seen := byTitle[en.Title]; !seen {
				byTitle[en.Title] = g.Key
			}
		}
	}
	for title, want := range map[string]string{
		"The Office": "works", "The Bends": "works", "Radiohead": "artists",
		"The Job": "episodes", "The Haunting of Hill House": "books",
	} {
		if got := byTitle[title]; got != want {
			t.Errorf("%q landed in %q, want %q", title, got, want)
		}
	}

	// A show is opened by its title, an artist by their name and a member by
	// its id: the three hashes the client spells, each from a field the
	// document carries.
	for _, g := range doc.Groups {
		for _, en := range g.Items {
			switch g.Key {
			case "works":
				if en.Kind != "work" || en.WorkKind == "" {
					t.Errorf("%q: a work result says which kind it is: %+v", en.Title, en)
				}
			case "artists":
				if en.Kind != "artist" || en.Self != artistHref(en.Title) {
					t.Errorf("%q: an artist result is their shelf: %+v", en.Title, en)
				}
			default:
				if en.Kind != "item" || en.WorkKey == "" || en.Label == "" {
					t.Errorf("%q: a member result names its work: %+v", en.Title, en)
				}
			}
		}
	}
	// The line under an episode is the show it is in — which is also the
	// title `#/show/<title>` is spelled with. A book, being its own work,
	// carries no second line at all.
	for _, g := range doc.Groups {
		for _, en := range g.Items {
			switch {
			case g.Key == "episodes" && en.Subtitle != "The Office":
				t.Errorf("%q: subtitle = %q, want the show's title", en.Title, en.Subtitle)
			case g.Key == "books" && en.Subtitle != "":
				t.Errorf("%q: subtitle = %q repeats the title", en.Title, en.Subtitle)
			}
		}
	}
}

// An empty query is a search that has found nothing yet — a cleared box, not
// a refusal.
func TestSearchWithoutAQuery(t *testing.T) {
	_, h := fixtureServer(t)
	w := get(t, h, "/api/search")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/search = %d: %s", w.Code, w.Body)
	}
	var doc searchDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Count != 0 || len(doc.Groups) != 0 || doc.Query != "" {
		t.Errorf("the empty search = %+v", doc)
	}
	if !strings.Contains(w.Body.String(), `"groups":[]`) {
		t.Errorf("groups is a list, empty or not: %s", w.Body)
	}
}

// linesDoc is the dialogue answer, whether it comes as a group of a search
// or as one file's own document.
type lineHit struct {
	Title   string  `json:"title"`
	ItemID  int64   `json:"item_id"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Passage struct {
		T   float64 `json:"t"`
		End float64 `json:"end"`
	} `json:"passage"`
	Links map[string]struct {
		Href string `json:"href"`
	} `json:"links"`
}

type linesDoc struct {
	Self  string    `json:"self"`
	Kind  string    `json:"kind"`
	Query string    `json:"query"`
	Count int       `json:"count"`
	Items []lineHit `json:"items"`
}

// A line of dialogue is a place: the words, the file that says them, and the
// scene around them — a couple of seconds either side, so the passage plays
// what was being answered rather than dropping in mid-breath.
func TestDialogueSearch(t *testing.T) {
	_, h := fixtureServer(t)
	w := get(t, h, "/api/search?q=took%20the%20job&client_id=chris")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/search = %d: %s", w.Code, w.Body)
	}
	var doc searchDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// Nothing in the library is CALLED "took the job": the dialogue is the
	// only row that answers, which is the whole point of it.
	if len(doc.Groups) != 1 || doc.Groups[0].Key != "lines" || doc.Groups[0].Title != "Dialogue" {
		t.Fatalf("the groups = %+v", doc.Groups)
	}
	var hits struct {
		Groups []struct {
			Items []lineHit `json:"items"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &hits); err != nil {
		t.Fatal(err)
	}
	got := hits.Groups[0].Items
	if len(got) != 1 {
		t.Fatalf("%d lines, want 1: %+v", len(got), got)
	}
	line := got[0]
	if line.ItemID != idTheJob || line.Text != "He took the job in New York." {
		t.Errorf("the line = %+v", line)
	}
	if line.Passage.T != line.Start-cueLead || line.Passage.End != line.End+cueLead {
		t.Errorf("the passage %+v is not the scene around %v–%v",
			line.Passage, line.Start, line.End)
	}
	if line.Links["self"].Href != itemHref(idTheJob) {
		t.Errorf("a line does not lead to the file that says it: %+v", line.Links)
	}

	// The same words asked of one file: the finder on its page.
	w = get(t, h, "/api/items/9/lines?q=battlestar&client_id=chris")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/items/9/lines = %d: %s", w.Code, w.Body)
	}
	var lines linesDoc
	if err := json.Unmarshal(w.Body.Bytes(), &lines); err != nil {
		t.Fatal(err)
	}
	if lines.Kind != "lines" || lines.Count != 1 || len(lines.Items) != 1 ||
		lines.Items[0].Start != 302 {
		t.Fatalf("one file's dialogue = %+v", lines)
	}

	// Words nobody says, and a file nobody transcribed: an empty answer, not
	// a refusal.
	for _, target := range []string{
		"/api/items/9/lines?q=parachute&client_id=chris",
		"/api/items/4/lines?q=job&client_id=chris",
		"/api/items/9/lines?client_id=chris",
	} {
		w := get(t, h, target)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body)
		}
		var empty linesDoc
		if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
			t.Fatal(err)
		}
		if empty.Count != 0 || len(empty.Items) != 0 {
			t.Errorf("GET %s answered %+v", target, empty)
		}
	}
	if w := get(t, h, "/api/items/nine/lines"); w.Code != http.StatusBadRequest {
		t.Errorf("an id that is not a number = %d", w.Code)
	}
}

// The finder is offered by the item that has words to search, and by no
// other: a file nobody has transcribed carries no `lines` relation, so the
// box is never drawn over nothing.
func TestLinesLinkFollowsTheTranscript(t *testing.T) {
	_, h := fixtureServer(t)
	for id, want := range map[int64]bool{idTheJob: true, idFrozen: false} {
		w := get(t, h, fmt.Sprintf("/api/items/%d?client_id=chris", id))
		if w.Code != http.StatusOK {
			t.Fatalf("GET item %d = %d: %s", id, w.Code, w.Body)
		}
		var doc struct {
			Links map[string]struct {
				Href string `json:"href"`
			} `json:"links"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if got := doc.Links["lines"].Href != ""; got != want {
			t.Errorf("item %d offers links.lines = %v, want %v", id, got, want)
		}
		if want && doc.Links["lines"].Href != linesHref(id) {
			t.Errorf("item %d: links.lines = %q", id, doc.Links["lines"].Href)
		}
	}
}
