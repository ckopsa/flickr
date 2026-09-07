package main

// The resume list as a document — docs/hypermedia.md's `continue` row:
// "entries as item envelopes with the resume position and the work link;
// per entry: resume".
//
// It used to be a bare JSON array of works.ContinueEntry, and the browser
// made up the rest: it guessed at a picture (/poster, then /cover on the
// 404), it decided which of `title` and `label` to draw, and it worked the
// percentage out of two numbers whose units differ by medium. All three
// were rules with nowhere to live. They live here now: the entry IS an item
// envelope, its picture is a link, its standing is a number and a phrase,
// and the one thing a tap does is an action with a label.

import (
	"net/http"
	"net/url"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// continueMax is how many rows the shelf carries. It was 20 when the list
// was an array and it is 20 now.
const continueMax = 20

// handleContinue is GET /api/continue — one profile's resume list, most
// recent first, as a collection of item envelopes.
func (s *server) handleContinue(w http.ResponseWriter, r *http.Request) {
	profile := profileOf(r)
	if profile == "" {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "no-profile",
			"There is nobody to resume for",
			"the resume list is per profile, and this request named none").
			WithRemedy("say who is asking — client_id on the address, or the cookie of the same name",
				&hyper.Link{Href: "/api/", Title: "flickr"}))
		return
	}
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	ps, err := s.state.PositionsFor(profile)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	positions := make(map[int64]store.Position, len(ps))
	for _, p := range ps {
		positions[p.ItemID] = p
	}
	byItem := works.ByItem(ws)

	entries := works.ContinueList(ws, ps, continueMax)
	rows := make([]*hyper.Envelope, 0, len(entries))
	for _, en := range entries {
		it := itemByID(ws, en.ItemID)
		if it == nil {
			continue // a position for an item the scan has since dropped
		}
		rows = append(rows, s.continueEntry(*it, byItem[en.ItemID], en, positions))
	}

	self := "/api/continue?client_id=" + url.QueryEscape(profile)
	doc := hyper.Doc(self, "continue", "Continue watching").
		Field("profile", profile).
		Field("count", len(rows)).
		Field("items", rows).
		Link("root", "/api/", "flickr").
		Link("library", "/api/library", "Library")
	hyper.WriteDoc(w, http.StatusOK, doc)
}

// continueEntry is one row: the member's own envelope, plus what makes it a
// RESUME row rather than a list row — whose work it is, how far in, and the
// one action the row offers.
func (s *server) continueEntry(it store.Item, wk *works.Work, en works.ContinueEntry, positions map[int64]store.Position) *hyper.Envelope {
	doc := s.itemEnvelope(it, wk, false, positions)
	// The headline is the WORK's title — a resume shelf says "The Office",
	// not "Beach Games" — and the member's own label is the line under it.
	// The envelope's `title` stays the item's; this names the other half so
	// a row needs no second document to draw itself.
	doc.Field("work_title", en.Title)
	doc.Field("position_seconds", en.PositionSeconds)
	if en.Fraction > 0 {
		doc.Field("fraction", en.Fraction)
	}
	doc.Field("percent", continuePercent(en))
	if href := continueArtwork(it, wk); href != "" {
		doc.Link("artwork", href, "")
	}
	// One name for "pick this up", whichever verb the medium uses: the row
	// renders `resume` and never asks what kind of thing it is holding.
	if act, ok := resumeAction(doc, it); ok {
		doc.Action("resume", act)
	}
	return doc
}

// continuePercent is how far along the row draws its bar, in one unit: a
// book counts in its own fraction, everything else in seconds of a running
// time. Deciding it here is what keeps the two units out of the browser.
func continuePercent(en works.ContinueEntry) float64 {
	if en.DurationSeconds > 0 {
		return clampPercent(en.PositionSeconds / en.DurationSeconds * 100)
	}
	return clampPercent(en.Fraction * 100)
}

func clampPercent(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
}

// continueArtwork is the picture the row shows: the item's own — an episode
// still, a film's poster, a record's or a book's cover — and, when this one
// file has none, the picture that stands for its work. "" when there is
// none at all, so a screen draws its text tile without a round-trip to a
// route that would answer 404.
func continueArtwork(it store.Item, wk *works.Work) string {
	if href := sessionArtwork(it); href != "" {
		return href
	}
	if wk != nil {
		return artworkHref(wk.RepresentativeItemID, wk.Medium)
	}
	return ""
}

// resumeAction is the row's one action, taken from the actions the item
// envelope already published: a book is read, everything else is played.
// Following the envelope's own answer is what keeps the two in step — an
// unprobed file offers neither, and the row then offers nothing either.
func resumeAction(doc *hyper.Envelope, it store.Item) (hyper.Action, bool) {
	name, label := "play", "▶ Resume"
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		name, label = "read", "Keep reading"
	}
	act, ok := doc.ActionNamed(name)
	if !ok {
		return hyper.Action{}, false
	}
	act.Label = label
	return act, true
}
