package main

// The household as a document: who is playing what, right now.
//
// Plex's dashboard, one row per live sitting. Everything it says is already
// known — the play registry holds one row per play and per reading
// (session.go), the library says what the file is, and state.db holds the
// place the player last wrote — so this document invents nothing. It only
// puts the three together in one place, which nothing else did: the session
// document answers to whoever holds its id, and a person cannot ask "is
// anybody watching?" by holding somebody else's id.
//
// It is a READ, and only a read. There is no action here to stop somebody
// else's stream: a household that can see each other is not the same thing
// as a household that can reach across the room.

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// live is a COPY of every session now open, so the document is built without
// the registry's lock held. The reaper keeps the map honest: a client that
// closed its tab is gone from here within the idle grace.
func (p *playSessions) live() []playSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]playSession, 0, len(p.rows))
	for _, row := range p.rows {
		out = append(out, *row)
	}
	return out
}

// handleActivity is GET /api/activity — the live plays and readings, the one
// that started last at the front.
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	// Every profile's places at once: this document is about the household,
	// not about whoever opened the panel.
	ps, err := s.state.AllPositions()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	placed := map[string]store.Position{}
	for _, p := range ps {
		placed[placeKey(p.ItemID, p.ClientID)] = p
	}

	rows := s.plays.live()
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].StartedAt.Equal(rows[j].StartedAt) {
			return rows[i].StartedAt.After(rows[j].StartedAt)
		}
		return rows[i].ID < rows[j].ID
	})

	byItem := works.ByItem(ws)
	entries := make([]*hyper.Envelope, 0, len(rows))
	for _, row := range rows {
		it := itemByID(ws, row.ItemID)
		if it == nil {
			continue // a scan dropped the file out from under the stream
		}
		entries = append(entries, activityEntry(row, *it, byItem[row.ItemID],
			placed[placeKey(row.ItemID, row.ClientID)]))
	}

	doc := hyper.Doc("/api/activity", "activity", "Activity").
		Field("count", len(entries)).
		Field("items", entries).
		Link("root", "/api/", "flickr").
		Link("library", "/api/library", "Library")
	hyper.WriteDoc(w, http.StatusOK, doc)
}

// placeKey is one profile's place in one item — the key a position is found
// by, since two people may be in the same file at once.
func placeKey(itemID int64, clientID string) string {
	return fmt.Sprintf("%d\x00%s", itemID, clientID)
}

// activityEntry is one sitting: who, what, how it is being sent, where they
// have got to and how long they have been at it. The place is the last one
// the player WROTE (state.db); a sitting that has not written one yet is
// where its bytes began, which for a transcode is the offset baked into the
// stream and for a direct play the position the client asked to start at.
func activityEntry(row playSession, it store.Item, wk *works.Work, place store.Position) *hyper.Envelope {
	e := hyper.Doc(itemHref(it.ID), "activity_entry", itemTitle(it)).
		Field("session_id", row.ID).
		Field("profile", activityProfile(row)).
		Field("item_id", it.ID).
		Field("label", works.ItemLabel(it))
	if wk != nil {
		e.Field("work_title", wk.Title)
	}
	seconds := row.Seek
	if place.ClientID != "" {
		seconds = place.PositionSeconds
	}
	e.Field("method", row.Method).
		Field("position_seconds", seconds).
		Field("position", activityPlace(it, seconds, place)).
		Field("started_at", row.StartedAt.UTC().Format(time.RFC3339))

	e.Link("item", itemHref(it.ID), itemTitle(it))
	if wk != nil {
		e.Link("work", workHref(wk.Key), wk.Title)
	}
	if href := sessionArtwork(it); href != "" {
		e.Link("artwork", href, "")
	}
	return e
}

// activityProfile is who is playing. A session started without a client_id
// is somebody all the same — the row says so rather than leaving a hole.
func activityProfile(row playSession) string {
	if row.ClientID == "" {
		return "someone"
	}
	return row.ClientID
}

// activityPlace is where they have got to, in the words the medium uses: a
// clock for anything with one, and the book's own percentage for a reading,
// which has no seconds to show. "" when there is nothing to say yet.
func activityPlace(it store.Item, seconds float64, place store.Position) string {
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		if place.Locator == nil {
			return ""
		}
		if place.Locator.Page > 0 {
			return fmt.Sprintf("p. %d", place.Locator.Page)
		}
		return fmt.Sprintf("%d%%", int(place.Locator.Fraction*100+0.5))
	}
	return works.Clock(seconds)
}
