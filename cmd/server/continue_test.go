package main

// The × on a resume row. Putting a thing down is the other half of a shelf
// that things are left on, and the test that matters is that the row actually
// GOES: a show whose next episode still has a place would come straight back
// under another member's name.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func del(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, target, nil))
	return w
}

// shelfRows is the works the resume shelf is carrying, by their titles.
func shelfRows(t *testing.T, doc map[string]any) []string {
	t.Helper()
	items, _ := doc["items"].([]any)
	out := make([]string, 0, len(items))
	for _, en := range items {
		e, _ := en.(map[string]any)
		title, _ := e["work_title"].(string)
		out = append(out, title)
	}
	return out
}

// forgetOf is the row's own action, read out of the document.
func forgetOf(t *testing.T, doc map[string]any, workTitle string) string {
	t.Helper()
	for _, en := range doc["items"].([]any) {
		e, _ := en.(map[string]any)
		if title, _ := e["work_title"].(string); title == workTitle {
			return href(t, e, "actions", "forget")
		}
	}
	t.Fatalf("the shelf carries no row for %q: %v", workTitle, shelfRows(t, doc))
	return ""
}

func TestForgetAContinueRow(t *testing.T) {
	srv, h := fixtureServer(t)
	// The show has a second episode, and giving it a place of its own is what
	// makes this test worth writing: forgetting one member must not hand the
	// row to the next one.
	if err := srv.state.SetPosition(idTheJob, "chris", 300); err != nil {
		t.Fatal(err)
	}
	before := decode(t, get(t, h, "/api/continue?client_id=chris"))
	if got := shelfRows(t, before); len(got) != 3 {
		t.Fatalf("the shelf starts with %v, want three rows", got)
	}

	w := del(t, h, forgetOf(t, before, "The Office"))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body)
	}
	// The answer IS the shelf as it now stands: no second read to see it.
	for _, title := range shelfRows(t, decode(t, w)) {
		if title == "The Office" {
			t.Error("the answer still carries the row that was forgotten")
		}
	}
	after := decode(t, get(t, h, "/api/continue?client_id=chris"))
	if got := shelfRows(t, after); len(got) != 2 {
		t.Errorf("the shelf is %v, want the two rows that were not forgotten", got)
	}
	// Every member of the work went, which is why the show did.
	for _, id := range []int64{idBeach, idTheJob} {
		p, err := srv.state.GetPosition(id, "chris")
		if err != nil {
			t.Fatal(err)
		}
		if p.ClientID != "" {
			t.Errorf("item %d still has a place: %+v", id, p)
		}
	}
}

// Another profile's places are untouched, and so is the library: forgetting
// is forgetting a PLACE, not deleting a thing.
func TestForgetIsOneProfilesOwn(t *testing.T) {
	srv, h := fixtureServer(t)
	if err := srv.state.SetPosition(idBeach, "sam", 900); err != nil {
		t.Fatal(err)
	}
	doc := decode(t, get(t, h, "/api/continue?client_id=chris"))
	if w := del(t, h, forgetOf(t, doc, "The Office")); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body)
	}
	p, err := srv.state.GetPosition(idBeach, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if p.PositionSeconds != 900 {
		t.Errorf("sam's place moved to %v", p.PositionSeconds)
	}
	if it, err := srv.library.GetItem(idBeach); err != nil || it == nil {
		t.Errorf("the item itself is gone: %v %v", it, err)
	}
}

func TestForgetRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		status       int
		problem      string
	}{
		{"nobody to forget it for", "/api/progress?item_id=8", http.StatusBadRequest, "no-profile"},
		{"that is not an item id", "/api/progress?item_id=eight&client_id=chris",
			http.StatusBadRequest, "bad-item-id"},
		{"no item at all", "/api/progress?client_id=chris", http.StatusBadRequest, "bad-item-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, h := fixtureServer(t)
			w := del(t, h, tc.target)
			if w.Code != tc.status {
				t.Fatalf("DELETE %s = %d: %s", tc.target, w.Code, w.Body)
			}
			if got := decode(t, w)["type"]; got != tc.problem {
				t.Errorf("problem type = %v, want %v", got, tc.problem)
			}
		})
	}
}

// An item the scan has since dropped is still a place worth forgetting: the
// row goes, and nothing panics on the way past a work that is not there.
func TestForgetAnItemNoWorkHolds(t *testing.T) {
	srv, h := fixtureServer(t)
	if err := srv.state.SetPosition(9999, "chris", 120); err != nil {
		t.Fatal(err)
	}
	if w := del(t, h, "/api/progress?item_id=9999&client_id=chris"); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", w.Code, w.Body)
	}
	p, err := srv.state.GetPosition(9999, "chris")
	if err != nil {
		t.Fatal(err)
	}
	if p.ClientID != "" {
		t.Errorf("the stale place is still there: %+v", p)
	}
}
