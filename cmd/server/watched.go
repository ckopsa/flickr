package main

// Mark as watched, and mark it unwatched again — the context action a library
// is asked for most, and the one thing a person could not say to this server
// without playing the whole file at it.
//
// It writes no new kind of state: watched IS a place, the place at the END of
// the thing, and unwatched is no place at all. Every reader of progress
// already agrees on that one threshold (works.Finished, 90%), so a marked
// episode ticks in its row, drops off the resume shelf and moves the show's
// "Next up" without any of them learning a second rule. And because it is a
// place write, it is refused while a passage is on for the same reason a
// progress write is: the clock is borrowed (docs/hypermedia.md §Passages are
// session state).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// watchedInput is the body of POST /api/items/{id}/watched. Both fields are
// optional: an absent `watched` is true, since marking it watched is what the
// address is for, and the profile is the request's own unless the body names
// one (a tool with no cookie can still say who it is asking for).
type watchedInput struct {
	Watched  *bool  `json:"watched"`
	ClientID string `json:"client_id"`
}

// handleWatched is POST /api/items/{id}/watched — the item's `watched`
// action. It answers the item's own document, freshly built, so the caller
// reads the mark it just made rather than being told "ok".
func (s *server) handleWatched(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("follow an item's link rather than composing its address",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	var in watchedInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-body",
			"That body could not be read", err.Error()).
			WithRemedy(`send {"watched": true} or {"watched": false}`, nil))
		return
	}
	profile := strings.TrimSpace(in.ClientID)
	if profile == "" {
		profile = profileOf(r)
	}
	if profile == "" {
		hyper.WriteProblem(w, noProfileProblem("There is nobody to mark it for",
			"a mark is one profile's place in a thing, and this request named none"))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	if item == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-item",
			"No such item", fmt.Sprintf("the library holds no item %d", id)).
			WithRemedy("scan the library, or open it and follow an item from there",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	// A title this profile may not see is not one it may mark either.
	if problem := s.refuseItem(profile, *item); problem != nil {
		hyper.WriteProblem(w, *problem)
		return
	}
	if row, on := s.plays.passageOn(id, profile); on {
		hyper.WriteProblem(w, passageOnProblem(row))
		return
	}

	watched := in.Watched == nil || *in.Watched
	if watched {
		pos, loc, ok := watchedPlace(*item)
		if !ok {
			hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "nothing-to-mark",
				"There is no end to mark this at", notMarkable(*item)).
				WithRemedy("probe the file again, then mark it",
					&hyper.Link{Href: itemHref(id) + "/reprobe", Title: "Probe this file again"}))
			return
		}
		err = s.state.SetPlace(id, profile, pos, loc)
	} else {
		err = s.state.ClearPlace(id, profile)
	}
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}

	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	positions, err := s.positionsFor(profile)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	hyper.WriteDoc(w, http.StatusOK, s.itemEnvelope(*item, works.ByItem(ws)[id], true, positions))
}

// watchedPlace is the place that counts as the end of one item, in the unit
// that item is measured in: the whole of the clock for anything with one, and
// the whole of the text for a book — a page count where the probe read one, a
// full fraction otherwise. ok=false when the file has no end to speak of,
// which is an unprobed file and nothing else.
func watchedPlace(it store.Item) (float64, *model.Locator, bool) {
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		loc := &model.Locator{Fraction: 1}
		if it.MediaInfo != nil {
			loc.Section, loc.Page = it.MediaInfo.Sections, it.MediaInfo.PageCount
		}
		return 0, loc, true
	}
	if dur := durationSeconds(it); dur > 0 {
		return dur, nil, true
	}
	return 0, nil, false
}

func durationSeconds(it store.Item) float64 {
	if it.MediaInfo == nil {
		return 0
	}
	return it.MediaInfo.DurationSeconds
}

// watchable says whether this item can be marked at all. It is the same
// question watchedPlace answers, asked by the document that has to decide
// between an action and an `unavailable` with a reason.
func watchable(it store.Item) bool {
	_, _, ok := watchedPlace(it)
	return ok
}

// notMarkable is why `watched` is not on offer, in words: without a probe
// there is no length, and without a length there is no end for a place to be
// at. `reprobe` is the remedy, and the item's own page offers it.
func notMarkable(it store.Item) string {
	if it.ProbeError != "" {
		return "the probe could not read this file, so it has no end to mark: " + it.ProbeError
	}
	return "this file has not been probed yet, so it has no end to mark"
}

// noProfileProblem is the refusal every per-profile write shares: playback
// state is keyed by the profile, so a request that names none is asking for
// nobody's place to be changed.
func noProfileProblem(title, detail string) hyper.Problem {
	return hyper.Refuse(http.StatusBadRequest, "no-profile", title, detail).
		WithRemedy("say who is asking — client_id on the address, or the cookie of the same name",
			&hyper.Link{Href: "/api/", Title: "flickr"})
}
