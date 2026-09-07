package main

import (
	"encoding/json"
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
			name: "a substring, not a word",
			q:    "adio",
			want: []string{"works/OK Computer", "works/Pablo Honey", "works/The Bends",
				"tracks/Airbag", "tracks/Paranoid Android", "tracks/You", "tracks/Planet Telex"},
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
	if want := "works, episodes, tracks, parts, books"; strings.Join(keys, ", ") != want {
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
	w := get(t, h, "/api/search?q=the&client_id=chris")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/search = %d: %s", w.Code, w.Body)
	}
	var doc searchDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "search" || doc.Query != "the" || doc.Self != "/api/search?q=the" {
		t.Errorf("the document = %+v", doc)
	}
	n := 0
	for _, g := range doc.Groups {
		n += g.Count
		for _, en := range g.Items {
			if en.Links["self"].Href != en.Self {
				t.Errorf("%q: links.self = %q, self = %q", en.Title, en.Links["self"].Href, en.Self)
			}
			if en.Title == "" || en.ItemID == 0 || en.Links["artwork"].Href == "" {
				t.Errorf("%q is not drawable: %+v", g.Key, en)
			}
		}
	}
	if doc.Count != n {
		t.Errorf("count = %d, %d hits", doc.Count, n)
	}

	byTitle := map[string]string{} // title → the group it landed in
	for _, g := range doc.Groups {
		for _, en := range g.Items {
			byTitle[en.Title] = g.Key
		}
	}
	for title, want := range map[string]string{
		"The Office": "works", "The Bends": "works",
		"The Job": "episodes", "The Haunting of Hill House": "books",
	} {
		if got := byTitle[title]; got != want {
			t.Errorf("%q landed in %q, want %q", title, got, want)
		}
	}

	// A show is opened by its title and a member by its id: the two hashes
	// the client spells, each from a field the document carries.
	for _, g := range doc.Groups {
		for _, en := range g.Items {
			switch g.Key {
			case "works":
				if en.Kind != "work" || en.WorkKind == "" {
					t.Errorf("%q: a work result says which kind it is: %+v", en.Title, en)
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
