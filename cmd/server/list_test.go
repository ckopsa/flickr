package main

// My List: the shelf a person writes rather than one the library derives.
// What is asserted here is the round trip — the tile offers `save`, pressing
// it answers a shelf with the thing on it, and the same tile now offers
// `unsave` — plus the two rules that are not about a button: a list belongs
// to one profile, and it is filtered like every other shelf.

import (
	"fmt"
	"net/http"
	"testing"
)

// listRows is the titles the shelf is carrying, in its own order.
func listRows(t *testing.T, doc map[string]any) []string {
	t.Helper()
	return titles(t, doc["items"])
}

// tileAction is the save-or-unsave one tile publishes, read out of the
// document rather than composed — the same rule the browser keeps. It answers
// the action's name and its href, or ("", "") when the tile offers neither.
func tileAction(t *testing.T, doc map[string]any, field, title string) (string, string) {
	t.Helper()
	rows, _ := doc[field].([]any)
	for _, row := range rows {
		tile, _ := row.(map[string]any)
		if fmt.Sprint(tile["title"]) != title {
			continue
		}
		acts, _ := tile["actions"].(map[string]any)
		for _, name := range []string{"save", "unsave"} {
			if _, ok := acts[name]; ok {
				return name, href(t, tile, "actions", name)
			}
		}
		return "", ""
	}
	t.Fatalf("no %q tile in %s: %v", title, field, titles(t, doc[field]))
	return "", ""
}

// The whole of it, from the grid: a tile says `save`, the press answers the
// shelf, and the tile says `unsave` from then on — until it is pressed again.
func TestSaveAndUnsaveFromTheGrid(t *testing.T) {
	_, h := fixtureServer(t)
	grid := decode(t, get(t, h, "/api/library?client_id=chris"))
	if got := listRows(t, decode(t, get(t, h, "/api/list?client_id=chris"))); len(got) != 2 {
		t.Fatalf("the fixture list starts at %v, want the two it was given", got)
	}

	name, addr := tileAction(t, grid, "items", "The Office")
	if name != "save" {
		t.Fatalf("a title not on the list offers %q, want save", name)
	}
	shelf := decode(t, post(t, h, addr+"?client_id=chris", nil))
	// The answer is the shelf as it now stands: newest first, so the title
	// just saved leads it.
	if got := listRows(t, shelf); fmt.Sprint(got) != "[The Office The Haunting of Hill House Frozen]" {
		t.Errorf("after saving: %v", got)
	}
	if got := shelf["count"]; fmt.Sprint(got) != "3" {
		t.Errorf("count = %v, want 3", got)
	}

	// And the grid now offers the other half of the same press.
	grid = decode(t, get(t, h, "/api/library?client_id=chris"))
	name, addr = tileAction(t, grid, "items", "The Office")
	if name != "unsave" {
		t.Fatalf("a saved title offers %q, want unsave", name)
	}
	// The library carries the shelf itself, so a home draws the row without
	// asking a second time — and the row is the same one /api/list answers.
	if got := titles(t, grid["list"]); fmt.Sprint(got) != "[The Office The Haunting of Hill House Frozen]" {
		t.Errorf("the library's own list = %v", got)
	}

	shelf = decode(t, del(t, h, addr+"?client_id=chris"))
	if got := listRows(t, shelf); fmt.Sprint(got) != "[The Haunting of Hill House Frozen]" {
		t.Errorf("after unsaving: %v", got)
	}
	// Pressing it twice is not an error: it is off the list either way.
	if w := del(t, h, addr+"?client_id=chris"); w.Code != http.StatusOK {
		t.Errorf("unsaving what is not on the list = %d: %s", w.Code, w.Body)
	}
	// Nor is saving it twice a second entry.
	if w := post(t, h, addr+"?client_id=chris", nil); w.Code != http.StatusOK {
		t.Fatalf("re-saving = %d: %s", w.Code, w.Body)
	}
	again := decode(t, post(t, h, addr+"?client_id=chris", nil))
	if got := listRows(t, again); fmt.Sprint(got) != "[The Office The Haunting of Hill House Frozen]" {
		t.Errorf("after saving twice: %v", got)
	}
}

// The work's own page offers the same bookmark the tile does, and by the same
// address — the action is about the WORK, wherever it is drawn.
func TestWorkDocumentCarriesTheBookmark(t *testing.T) {
	_, h := fixtureServer(t)
	office := "/api/works/tmdb%3A2316?client_id=chris"
	doc := decode(t, get(t, h, office))
	addr := href(t, doc, "actions", "save")
	if w := post(t, h, addr+"?client_id=chris", nil); w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", addr, w.Code, w.Body)
	}
	doc = decode(t, get(t, h, office))
	if got := href(t, doc, "actions", "unsave"); got != addr {
		t.Errorf("unsave = %q, save was %q; one work, one address", got, addr)
	}
	acts, _ := doc["actions"].(map[string]any)
	if _, ok := acts["save"]; ok {
		t.Error("a saved work still offers save; the two are one control")
	}
}

// A list belongs to somebody. Every one of the three addresses says so in
// the same words, and the work document says it as an `unavailable` rather
// than offering a button that would refuse.
func TestListNeedsAProfile(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct {
		name string
		do   func() *http.Response
	}{
		{"reading it", func() *http.Response { return get(t, h, "/api/list").Result() }},
		{"saving", func() *http.Response { return post(t, h, saveHref("tmdb:2316"), nil).Result() }},
		{"unsaving", func() *http.Response { return del(t, h, saveHref("tmdb:2316")).Result() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.do()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s without a profile = %d", tc.name, res.StatusCode)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content-type = %q", ct)
			}
		})
	}
	doc := decode(t, get(t, h, "/api/works/tmdb%3A2316"))
	un, _ := doc["unavailable"].(map[string]any)
	entry, ok := un["save"].(map[string]any)
	if !ok {
		t.Fatalf("an anonymous work document offers no reason there is no bookmark: %v", un)
	}
	if reason, _ := entry["reason"].(string); reason == "" {
		t.Error("the reason is not in words")
	}
	acts, _ := doc["actions"].(map[string]any)
	if _, ok := acts["save"]; ok {
		t.Error("an anonymous work document offers a save that would refuse")
	}
}

// Saving is a write against a work, and the two things that are not one are
// refused the way every other address refuses them.
func TestSaveRefusals(t *testing.T) {
	srv, h := profiled(t)
	// A kid may not put by what they may not see. The address is spelled here
	// rather than followed, because the whole point is that no document this
	// profile reads offers it.
	if w := post(t, h, saveHref("tmdb:2316")+"?client_id=pip", nil); w.Code != http.StatusForbidden {
		t.Errorf("a kid saving a TV-14 show = %d: %s", w.Code, w.Body)
	}
	// The one they may see, they may save.
	if w := post(t, h, saveHref("tmdb:109445")+"?client_id=pip", nil); w.Code != http.StatusOK {
		t.Fatalf("a kid saving a PG film = %d: %s", w.Code, w.Body)
	}
	// A key nothing answers to is a problem with the library as its remedy.
	w := post(t, h, saveHref("show:the-lost-tapes")+"?client_id=chris", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("saving a work that is not there = %d: %s", w.Code, w.Body)
	}
	// Taking one off, though, asks the library nothing: a title the scan has
	// dropped could come off no other way.
	if err := srv.state.Save("chris", "show:the-lost-tapes"); err != nil {
		t.Fatal(err)
	}
	if w := del(t, h, saveHref("show:the-lost-tapes")+"?client_id=chris"); w.Code != http.StatusOK {
		t.Errorf("unsaving a work that is no longer there = %d: %s", w.Code, w.Body)
	}
	rows, err := srv.state.SavedFor("chris")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.WorkKey == "show:the-lost-tapes" {
			t.Error("the row is still on the list")
		}
	}
}

// The shelf is filtered like every other one: it is built out of the works
// this profile may SEE, so a row saved by somebody else on the same device
// does not come back through it.
func TestListIsFilteredForAKid(t *testing.T) {
	srv, h := profiled(t)
	for _, key := range []string{"tmdb:2316", "tmdb:109445"} {
		if err := srv.state.Save("pip", key); err != nil {
			t.Fatal(err)
		}
	}
	doc := decode(t, get(t, h, "/api/list?client_id=pip"))
	if got := listRows(t, doc); fmt.Sprint(got) != "[Frozen]" {
		t.Errorf("a kid's list = %v, want only the title they may see", got)
	}
	// The library's copy of the row is the same list, so it is filtered by
	// having been built the same way.
	grid := decode(t, get(t, h, "/api/library?client_id=pip"))
	if got := titles(t, grid["list"]); fmt.Sprint(got) != "[Frozen]" {
		t.Errorf("the library's list for a kid = %v", got)
	}
}

// The root names the shelf, carrying the profile when it knows one — the link
// refuses without it, so it should be followable as it stands.
func TestRootNamesTheList(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct{ target, want string }{
		{"/api/", "/api/list"},
		{"/api/?client_id=chris", "/api/list?client_id=chris"},
	} {
		doc := decode(t, get(t, h, tc.target))
		if got := href(t, doc, "links", "list"); got != tc.want {
			t.Errorf("GET %s: links.list = %q, want %q", tc.target, got, tc.want)
		}
	}
}

// An artist's shelf is not a work, and there is nothing to put on a list: the
// tile carries no bookmark at all.
func TestAnArtistShelfHasNoBookmark(t *testing.T) {
	_, h := fixtureServer(t)
	grid := decode(t, get(t, h, "/api/library?client_id=chris"))
	if name, _ := tileAction(t, grid, "items", "Radiohead"); name != "" {
		t.Errorf("the artist tile offers %q", name)
	}
}

// A saved key whose work the scan has since dropped is skipped rather than
// drawn as a hole — the shelf is the works it can still show.
func TestListSkipsWhatIsGone(t *testing.T) {
	srv, h := fixtureServer(t)
	if err := srv.state.Save("chris", "movie:the-one-that-got-away"); err != nil {
		t.Fatal(err)
	}
	doc := decode(t, get(t, h, "/api/list?client_id=chris"))
	if got := listRows(t, doc); len(got) != 2 {
		t.Errorf("the shelf = %v, want the two works that are still there", got)
	}
	if got := doc["count"]; fmt.Sprint(got) != "2" {
		t.Errorf("count = %v, want 2 — what is drawn, not what is stored", got)
	}
}
