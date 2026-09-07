package main

// The tick, and what it writes: a place at the end of the thing, in the unit
// that thing is measured in. Everything downstream — the row's ✓, the resume
// shelf, a show's Next up — reads that one place through works.Finished, so
// what is asserted here is the PLACE and the document that says which way the
// next press goes.

import (
	"net/http"
	"testing"

	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// watchedHref is the address the item's own document publishes for the mark,
// read out of the document rather than composed — the same rule the browser
// keeps.
func watchedHref(t *testing.T, h http.Handler, id int64) string {
	t.Helper()
	w := get(t, h, itemHref(id)+"?client_id=chris")
	if w.Code != http.StatusOK {
		t.Fatalf("GET item %d = %d: %s", id, w.Code, w.Body)
	}
	return href(t, decode(t, w), "actions", "watched")
}

func TestMarkWatched(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       int64
		body     map[string]any
		finished bool // the profile's place is at the end afterwards
		row      bool // and there is a place at all
		label    string
	}{
		{"a film is watched to its last second", idFrozen,
			map[string]any{"watched": true}, true, true, "Mark unwatched"},
		{"so is an episode, halfway through or not", idBeach,
			map[string]any{"watched": true}, true, true, "Mark unwatched"},
		{"a book is read to its last page", idHillHouse,
			map[string]any{"watched": true}, true, true, "Mark unwatched"},
		{"a PDF counts in pages and lands on the last", idFlatland,
			map[string]any{"watched": true}, true, true, "Mark unwatched"},
		{"a body with nothing in it means watched", idFrozen,
			nil, true, true, "Mark unwatched"},
		{"unwatched drops the place rather than zeroing it", idBeach,
			map[string]any{"watched": false}, false, false, "Mark watched"},
		{"and unwatching what was never watched is no error", idFrozen,
			map[string]any{"watched": false}, false, false, "Mark watched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, h := fixtureServer(t)
			w := post(t, h, watchedHref(t, h, tc.id)+"?client_id=chris", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
			}
			// The answer is the item as it now stands, so the control that
			// was pressed comes back saying which way it goes next.
			doc := decode(t, w)
			acts, _ := doc["actions"].(map[string]any)
			act, _ := acts["watched"].(map[string]any)
			if got, _ := act["label"].(string); got != tc.label {
				t.Errorf("the answer's watched label = %q, want %q", got, tc.label)
			}
			in, _ := act["input"].(map[string]any)
			wantValue := "true"
			if tc.finished {
				wantValue = "false"
			}
			if got, _ := in["watched"].(string); got != wantValue {
				t.Errorf("the action sends back %q, want %q", got, wantValue)
			}

			p, err := srv.state.GetPosition(tc.id, "chris")
			if err != nil {
				t.Fatal(err)
			}
			if (p.ClientID != "") != tc.row {
				t.Fatalf("row present = %v, want %v (%+v)", p.ClientID != "", tc.row, p)
			}
			item, err := srv.library.GetItem(tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got := works.Finished(*item, p); got != tc.finished {
				t.Errorf("works.Finished = %v, want %v (%+v)", got, tc.finished, p)
			}
		})
	}
}

// The mark is one profile's, like every other place: marking it for one
// leaves the other where they were.
func TestMarkWatchedIsOneProfilesOwn(t *testing.T) {
	srv, h := fixtureServer(t)
	if w := post(t, h, itemHref(idBeach)+"/watched?client_id=sam", map[string]any{"watched": true}); w.Code != http.StatusOK {
		t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
	}
	chris, err := srv.state.GetPosition(idBeach, "chris")
	if err != nil {
		t.Fatal(err)
	}
	if chris.PositionSeconds != 745 {
		t.Errorf("chris was moved to %v; the mark was sam's", chris.PositionSeconds)
	}
}

// A watched episode is what the row draws its ✓ from, and it is the same
// field the member envelope already published — nothing new was taught to a
// list.
func TestMarkWatchedTicksTheRowAndClearsTheShelf(t *testing.T) {
	_, h := fixtureServer(t)
	if w := post(t, h, itemHref(idBeach)+"/watched?client_id=chris", map[string]any{"watched": true}); w.Code != http.StatusOK {
		t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
	}
	work := decode(t, get(t, h, "/api/works/tmdb%3A2316?client_id=chris"))
	members, _ := work["members"].([]any)
	seen := false
	for _, m := range members {
		mm, _ := m.(map[string]any)
		if id, _ := mm["id"].(float64); int64(id) != idBeach {
			continue
		}
		seen = true
		if watched, _ := mm["watched"].(bool); !watched {
			t.Errorf("the marked episode's row does not say watched: %v", mm)
		}
	}
	if !seen {
		t.Fatal("the show has no such member")
	}
	// A finished episode is not somewhere to be resumed, so the show's row
	// on the resume shelf is the NEXT episode rather than this one.
	cont := decode(t, get(t, h, "/api/continue?client_id=chris"))
	for _, en := range cont["items"].([]any) {
		e, _ := en.(map[string]any)
		if id, _ := e["id"].(float64); int64(id) == idBeach {
			t.Error("the resume shelf still offers the episode that was just marked")
		}
	}
}

func TestMarkWatchedRefusals(t *testing.T) {
	t.Run("nobody to mark it for", func(t *testing.T) {
		_, h := fixtureServer(t)
		w := post(t, h, itemHref(idFrozen)+"/watched", map[string]any{"watched": true})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
		}
		if got := decode(t, w)["type"]; got != "no-profile" {
			t.Errorf("problem type = %v", got)
		}
	})

	t.Run("no such item", func(t *testing.T) {
		_, h := fixtureServer(t)
		w := post(t, h, "/api/items/999/watched?client_id=chris", map[string]any{"watched": true})
		if w.Code != http.StatusNotFound {
			t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
		}
	})

	t.Run("that is not an item id", func(t *testing.T) {
		_, h := fixtureServer(t)
		w := post(t, h, "/api/items/nope/watched?client_id=chris", map[string]any{"watched": true})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
		}
	})

	// A passage borrows the clock, so it borrows the place too: the mark is
	// refused for exactly as long as a progress write is, and says the same
	// way out.
	t.Run("a passage is on", func(t *testing.T) {
		_, h, _ := playing(t, idBeach, map[string]any{
			"passage": map[string]any{"t": 142, "end": 854},
		})
		w := post(t, h, itemHref(idBeach)+"/watched?client_id=chris", map[string]any{"watched": true})
		if w.Code != http.StatusConflict {
			t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
		}
		if got := decode(t, w)["type"]; got != "passage-on" {
			t.Errorf("problem type = %v", got)
		}
	})

	// A kid profile is not shown the title, and is not allowed to mark it
	// either — the refusal is the shelves' own.
	t.Run("not for this profile", func(t *testing.T) {
		srv, h := fixtureServer(t)
		if err := srv.state.CreateUser(store.User{Name: "kid", Kid: true}); err != nil {
			t.Fatal(err)
		}
		w := post(t, h, itemHref(idBeach)+"/watched?client_id=kid", map[string]any{"watched": true})
		if w.Code != http.StatusForbidden {
			t.Fatalf("POST watched = %d: %s", w.Code, w.Body)
		}
	})
}

// An unprobed file has no length, and so no end for a place to be at. The
// document says so instead of offering the action, and the handler says the
// same thing to anyone who asks anyway.
func TestNothingToMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		it   store.Item
		ok   bool
	}{
		{"a film with a clock", store.Item{MediaInfo: &model.MediaInfo{
			Medium: model.MediumVideo, DurationSeconds: 6420}}, true},
		{"a book, which counts in its own measure", store.Item{MediaInfo: &model.MediaInfo{
			Medium: model.MediumText, Sections: 8}}, true},
		{"a file nobody probed", store.Item{}, false},
		{"a file the probe could not read", store.Item{
			ProbeError: "moov atom not found",
			MediaInfo:  &model.MediaInfo{Medium: model.MediumVideo}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := watchable(tc.it); got != tc.ok {
				t.Fatalf("watchable = %v, want %v", got, tc.ok)
			}
			if !tc.ok && notMarkable(tc.it) == "" {
				t.Error("an unavailable action must say why in words")
			}
		})
	}
}

// The whole of a book is the whole of a book, whichever unit it is measured
// in: a spine with no page count is a full fraction, a PDF is its last page.
func TestWatchedPlaceForText(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *model.MediaInfo
		want model.Locator
	}{
		{"an EPUB", &model.MediaInfo{Medium: model.MediumText, Sections: 8},
			model.Locator{Section: 8, Fraction: 1}},
		{"a PDF", &model.MediaInfo{Medium: model.MediumText, PageCount: 400},
			model.Locator{Page: 400, Fraction: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := store.Item{MediaInfo: tc.info}
			pos, loc, ok := watchedPlace(it)
			if !ok || loc == nil {
				t.Fatalf("watchedPlace = %v, %v, %v", pos, loc, ok)
			}
			if *loc != tc.want {
				t.Errorf("locator = %+v, want %+v", *loc, tc.want)
			}
			if !works.Finished(it, store.Position{Locator: loc}) {
				t.Error("the place written is not one works.Finished reads as the end")
			}
		})
	}
}
