package main

// Profiles: the face, the audience, and the shelf a kid profile is shown.
//
// The fixture library (hyper_test.go) has one title of each rating the
// filter cares about: Frozen is PG, The Office is TV-14, and the records,
// the audiobook and the books are rated by nobody at all. So one table over
// the three kinds of profile — nobody, an adult, a kid — is the whole of the
// rule, and the rest of the file is the same rule seen from search, the
// resume shelf, a route and a play.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"flickr/internal/store"
)

// profiled is the fixture server with two profiles in it: one adult, one
// kid. Everything below reads a document as one of them.
func profiled(t *testing.T) (*server, http.Handler) {
	t.Helper()
	srv, h := fixtureServer(t)
	for _, u := range []store.User{
		{Name: "chris", Avatar: "🦊"},
		{Name: "pip", Avatar: "🐼", Kid: true},
	} {
		if err := srv.state.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	return srv, h
}

// titles is what a list of tiles (or of hits, or of resume rows) is called.
func titles(t *testing.T, rows any) []string {
	t.Helper()
	list, ok := rows.([]any)
	if !ok {
		t.Fatalf("not a list of documents: %#v", rows)
	}
	var out []string
	for _, row := range list {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("not a document: %#v", row)
		}
		out = append(out, fmt.Sprint(m["title"]))
	}
	return out
}

func TestLibraryByProfileKind(t *testing.T) {
	_, h := profiled(t)
	everything := []string{"The Office", "Frozen", "Radiohead", "Dune", "Flatland",
		"The Haunting of Hill House"}
	for _, tc := range []struct {
		name    string
		profile string
		want    []string
		bands   int
	}{
		// A stranger is nobody's child: the shelf is the whole library.
		{"anonymous", "", everything, 5},
		{"an adult", "chris", everything, 5},
		// PG stands; TV-14 does not, and neither does a title nobody rated —
		// which is every record and every book in this library.
		{"a kid", "pip", []string{"Frozen"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := "/api/library"
			if tc.profile != "" {
				target += "?client_id=" + tc.profile
			}
			w := get(t, h, target)
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body)
			}
			doc := decode(t, w)
			if got := titles(t, doc["items"]); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("tiles = %v, want %v", got, tc.want)
			}
			// A band with nothing in it is not a heading, so the rows go with
			// the tiles.
			if got := len(doc["bands"].([]any)); got != tc.bands {
				t.Errorf("bands = %d, want %d", got, tc.bands)
			}
			// The recently-added row is a selection of the same tiles, so it
			// is filtered by having been drawn from them.
			for _, name := range titles(t, doc["recently_added"]) {
				if fmt.Sprint(tc.want) == "[Frozen]" && name != "Frozen" {
					t.Errorf("recently added holds %q, which this profile may not see", name)
				}
			}
		})
	}
}

func TestSearchIsFilteredForAKid(t *testing.T) {
	_, h := profiled(t)
	for _, tc := range []struct {
		profile, query string
		want           float64
	}{
		{"chris", "office", 2}, // the show, and the episode set at the beach
		{"pip", "office", 0},   // none of which a kid profile may see
		{"pip", "frozen", 1},   // and PG is still there
	} {
		target := "/api/search?q=" + tc.query + "&client_id=" + tc.profile
		w := get(t, h, target)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body)
		}
		if got := decode(t, w)["count"]; got != tc.want {
			t.Errorf("GET %s: count = %v, want %v", target, got, tc.want)
		}
	}
}

func TestContinueIsFilteredForAKid(t *testing.T) {
	srv, h := profiled(t)
	// The kid has been part-way through both: the PG film and the TV-14 show.
	if err := srv.state.SetPosition(idBeach, "pip", 400); err != nil {
		t.Fatal(err)
	}
	if err := srv.state.SetPosition(idFrozen, "pip", 900); err != nil {
		t.Fatal(err)
	}
	w := get(t, h, "/api/continue?client_id=pip")
	if w.Code != http.StatusOK {
		t.Fatalf("GET continue = %d: %s", w.Code, w.Body)
	}
	if got := titles(t, decode(t, w)["items"]); fmt.Sprint(got) != "[Frozen]" {
		t.Errorf("resume shelf = %v, want [Frozen]", got)
	}
}

func TestRouteIsFilteredForAKid(t *testing.T) {
	_, h := profiled(t)
	for _, tc := range []struct {
		name, hash string
		status     int
	}{
		{"the show is not there", "#/show/The%20Office", http.StatusNotFound},
		{"nor one of its episodes", fmt.Sprintf("#/item/%d", idBeach), http.StatusNotFound},
		{"the film is", fmt.Sprintf("#/item/%d", idFrozen), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(t, h, "/api/-/route?client_id=pip&hash="+tc.hash)
			if w.Code != tc.status {
				t.Fatalf("route %s = %d, want %d: %s", tc.hash, w.Code, tc.status, w.Body)
			}
		})
	}
}

func TestPlayIsRefusedForAKid(t *testing.T) {
	srv, h := fixturePlayer(t)
	for _, u := range []store.User{{Name: "chris"}, {Name: "pip", Kid: true}} {
		if err := srv.state.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	// The adult plays the TV-14 episode, and the kid plays the PG film.
	play(t, h, idBeach, map[string]any{"client_id": "chris"})
	play(t, h, idFrozen, map[string]any{"client_id": "pip"})

	// The kid, addressing the episode directly, is told why and where to go.
	w := post(t, h, fmt.Sprintf("/api/items/%d/play", idBeach), map[string]any{
		"capabilities": directPlayCaps(), "client_id": "pip",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("kid play = %d, want 403: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q", ct)
	}
	p := decode(t, w)
	if p["type"] != "not-for-this-profile" {
		t.Errorf("type = %v", p["type"])
	}
	remedy, _ := p["remedy"].(map[string]any)
	if remedy == nil || remedy["text"] == "" {
		t.Fatalf("refusal without a remedy: %v", p)
	}
	link, _ := remedy["link"].(map[string]any)
	if link == nil || link["title"] != "Profiles" {
		t.Errorf("the remedy does not name the profile switch: %v", remedy)
	}
}

// A profile is created with a face whether or not one was asked for, and the
// kid flag it was created with is the one it is listed with.
func TestCreateProfileGivesItAFace(t *testing.T) {
	_, h := fixtureServer(t)
	made := decode(t, post(t, h, "/api/users", map[string]any{"name": "pip", "kid": true}))
	if made["avatar"] == nil || made["avatar"] == "" {
		t.Errorf("a new profile has no face: %v", made)
	}
	if made["kid"] != true {
		t.Errorf("kid = %v, want true", made["kid"])
	}
	// Creating it again keeps the face and the audience it already had.
	again := decode(t, post(t, h, "/api/users", map[string]any{"name": "pip"}))
	if again["avatar"] != made["avatar"] || again["kid"] != true {
		t.Errorf("re-created profile = %v, want %v", again, made)
	}

	w := get(t, h, "/api/users")
	if w.Code != http.StatusOK {
		t.Fatalf("GET users = %d: %s", w.Code, w.Body)
	}
	var rows []store.User
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "pip" || !rows[0].Kid || rows[0].Avatar == "" {
		t.Errorf("users = %+v", rows)
	}
}

func TestFitForKids(t *testing.T) {
	for _, tc := range []struct {
		cert string
		want bool
	}{
		{"G", true}, {"PG", true}, {"TV-Y7", true}, {"TV-PG", true},
		{"pg", true}, // whatever case TMDB spelled it in
		{"PG-13", false}, {"R", false}, {"NC-17", false},
		{"TV-14", false}, {"TV-MA", false},
		{"", false}, // nobody rated it, so nobody vouched for it
	} {
		if got := fitForKids(tc.cert); got != tc.want {
			t.Errorf("fitForKids(%q) = %v, want %v", tc.cert, got, tc.want)
		}
	}
}
