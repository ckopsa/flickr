package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"flickr/internal/model"
)

// activityDoc is the household document as a test reads it.
type activityDoc struct {
	Count int `json:"count"`
	Items []struct {
		Self      string  `json:"self"`
		Kind      string  `json:"kind"`
		Title     string  `json:"title"`
		SessionID string  `json:"session_id"`
		Profile   string  `json:"profile"`
		ItemID    int64   `json:"item_id"`
		Label     string  `json:"label"`
		WorkTitle string  `json:"work_title"`
		Method    string  `json:"method"`
		Seconds   float64 `json:"position_seconds"`
		Position  string  `json:"position"`
		StartedAt string  `json:"started_at"`
		Links     map[string]struct {
			Href string `json:"href"`
		} `json:"links"`
	} `json:"items"`
}

// started is one sitting the registry is holding: the fields the dashboard
// reads, and a start time written down rather than taken from the clock, so
// the order below is the one the test asked for.
func started(id string, itemID int64, profile, workKey, method string, seek float64, at time.Time) playSession {
	return playSession{
		ID: id, ItemID: itemID, ClientID: profile, WorkKey: workKey,
		Method: method, Seek: seek, StartedAt: at,
	}
}

func activity(t *testing.T, h http.Handler) activityDoc {
	t.Helper()
	w := get(t, h, "/api/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/activity = %d: %s", w.Code, w.Body)
	}
	var doc activityDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET /api/activity: %v (%s)", err, w.Body)
	}
	return doc
}

// Nothing playing is an empty list, not a refusal: the panel draws "nothing
// is playing" from a document that answered.
func TestActivityIsEmptyWhenNobodyIsPlaying(t *testing.T) {
	_, h := fixtureServer(t)
	doc := activity(t, h)
	if doc.Count != 0 || len(doc.Items) != 0 {
		t.Errorf("an idle house is busy: %+v", doc)
	}
}

// One row per live sitting, the one that started last at the front: who, what
// they are in, how it is being sent and where they have got to.
func TestActivityRows(t *testing.T) {
	srv, h := fixtureServer(t)
	base := time.Date(2026, time.September, 5, 20, 0, 0, 0, time.UTC)
	// Chris is partway through an episode (the fixture wrote the place);
	// dana is a minute into a film nobody has saved a place for, and her
	// bytes began at the offset the transcode was seeked to.
	srv.plays.put(started("s1", idBeach, "chris", "tmdb:2316", string(model.Transcode), 0, base))
	srv.plays.put(started("s2", idFrozen, "dana", "tmdb:109445", string(model.DirectPlay), 60, base.Add(time.Minute)))

	doc := activity(t, h)
	if doc.Count != 2 {
		t.Fatalf("count = %d, %d rows", doc.Count, len(doc.Items))
	}
	first, second := doc.Items[0], doc.Items[1]
	if first.SessionID != "s2" || second.SessionID != "s1" {
		t.Errorf("the newest sitting does not lead: %q then %q", first.SessionID, second.SessionID)
	}

	if first.Profile != "dana" || first.Method != string(model.DirectPlay) || first.WorkTitle != "Frozen" {
		t.Errorf("dana's row = %+v", first)
	}
	if first.Seconds != 60 || first.Position != "1:00" {
		t.Errorf("a sitting with no saved place is where its bytes began: %+v", first)
	}
	if first.StartedAt != "2026-09-05T20:01:00Z" {
		t.Errorf("started_at = %q", first.StartedAt)
	}

	if second.Profile != "chris" || second.WorkTitle != "The Office" ||
		second.Label != "S03E22 · Beach Games" {
		t.Errorf("chris's row = %+v", second)
	}
	// 745 seconds is what the fixture's profile last wrote for that episode.
	if second.Seconds != 745 || second.Position != "12:25" {
		t.Errorf("the place is the last one written: %+v", second)
	}
	// Every address is the document's own: the item, its work, its picture.
	if second.Self != second.Links["item"].Href ||
		second.Links["work"].Href != "/api/works/tmdb%3A2316" ||
		second.Links["artwork"].Href == "" {
		t.Errorf("chris's row links = %+v", second.Links)
	}

	// The row goes when the sitting does.
	srv.plays.drop("s1")
	if doc := activity(t, h); doc.Count != 1 || doc.Items[0].SessionID != "s2" {
		t.Errorf("a stopped session is still on the dashboard: %+v", doc)
	}
}

// A reading has no clock, so its place is the book's own measure — and a
// session nobody has named is still somebody.
func TestActivityReadingAndAnonymousSitting(t *testing.T) {
	srv, h := fixtureServer(t)
	srv.plays.put(started("r1", idHillHouse, "chris", "", methodRead, 0,
		time.Date(2026, time.September, 5, 21, 0, 0, 0, time.UTC)))
	srv.plays.put(started("r2", idFlatland, "", "", methodRead, 0,
		time.Date(2026, time.September, 5, 20, 0, 0, 0, time.UTC)))

	doc := activity(t, h)
	if doc.Count != 2 {
		t.Fatalf("count = %d", doc.Count)
	}
	// The fixture has chris 34% into the EPUB and nobody in the PDF.
	if doc.Items[0].Method != methodRead || doc.Items[0].Position != "34%" {
		t.Errorf("the reading's place = %+v", doc.Items[0])
	}
	if doc.Items[1].Profile != "someone" || doc.Items[1].Position != "" {
		t.Errorf("an unnamed reader = %+v", doc.Items[1])
	}
}

// The dashboard is a read and nothing else: no action to reach across the
// room and stop somebody else's stream.
func TestActivityOffersNoActions(t *testing.T) {
	srv, h := fixtureServer(t)
	srv.plays.put(started("s1", idFrozen, "chris", "tmdb:109445", string(model.DirectPlay), 0, time.Now()))
	body := get(t, h, "/api/activity").Body.String()
	if strings.Contains(body, `"actions"`) {
		t.Errorf("the dashboard offers an action: %s", body)
	}
}

// The root names it, so the client reaches it by a relation like everything
// else — and the document itself is a golden.
func TestActivityGolden(t *testing.T) {
	srv, h := fixtureServer(t)
	var root struct {
		Links map[string]struct {
			Href, Title string
		} `json:"links"`
	}
	if err := json.Unmarshal(get(t, h, "/api/").Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root.Links["activity"].Href != "/api/activity" {
		t.Fatalf("the root does not name the activity: %+v", root.Links)
	}
	srv.plays.put(started("a1c9f2", idBeach, "chris", "tmdb:2316", string(model.Transcode), 0,
		time.Date(2026, time.September, 5, 20, 0, 0, 0, time.UTC)))
	w := get(t, h, root.Links["activity"].Href)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", root.Links["activity"].Href, w.Code, w.Body)
	}
	golden(t, "activity", w.Body.Bytes())
}
