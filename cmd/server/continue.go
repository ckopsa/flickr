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
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
	// The shelf is built out of the works this profile may see, so a title a
	// kid profile is not shown does not come back through the resume row it
	// once left behind.
	ws, err := s.worksFor(r)
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
		rows = append(rows, s.continueEntry(*it, byItem[en.ItemID], en, positions, profile))
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
func (s *server) continueEntry(it store.Item, wk *works.Work, en works.ContinueEntry, positions map[int64]store.Position, profile string) *hyper.Envelope {
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
	// And one name for "not this": the × on the row. A shelf is somewhere
	// things are LEFT, so being able to put one down is half of it.
	doc.Action("forget", hyper.Action{
		Method: "DELETE", Href: forgetHref(it.ID, profile),
		Label: "Remove from Continue watching",
	})
	return doc
}

// forgetHref is the address that drops a place. The item and the profile ride
// as query parameters because that is how GET /api/progress already spells
// the same two things.
func forgetHref(itemID int64, profile string) string {
	return "/api/progress?item_id=" + strconv.FormatInt(itemID, 10) +
		"&client_id=" + url.QueryEscape(profile)
}

// handleForgetProgress is DELETE /api/progress — a row's `forget` action.
//
// It clears the profile's place in every member of the item's WORK, not just
// in the item named: the shelf carries one row per work, so clearing the
// episode that is showing would only hand the row on to the next episode —
// the show would still be there, and the × would have done nothing a person
// can see. A work of one member (a film, a book) is that same rule, cheaply.
//
// The answer is the shelf as it now stands, which is the document the caller
// was looking at.
func (s *server) handleForgetProgress(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("item_id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("follow a resume row's own action rather than composing its address",
				&hyper.Link{Href: "/api/continue", Title: "Continue watching"}))
		return
	}
	profile := profileOf(r)
	if profile == "" {
		hyper.WriteProblem(w, noProfileProblem("There is nobody to forget it for",
			"a place belongs to one profile, and this request named none"))
		return
	}
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	for _, member := range forgettable(ws, id) {
		if err := s.state.ClearPlace(member, profile); err != nil {
			hyper.WriteProblem(w, serverProblem(err))
			return
		}
	}
	s.handleContinue(w, r)
}

// forgettable are the items one × clears: every member of the item's work, or
// the item alone when the scan has since dropped it from every work.
func forgettable(ws []works.Work, itemID int64) []int64 {
	wk := works.ByItem(ws)[itemID]
	if wk == nil {
		return []int64{itemID}
	}
	out := make([]int64, 0, len(wk.Items))
	for _, it := range wk.Items {
		out = append(out, it.ID)
	}
	return out
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
