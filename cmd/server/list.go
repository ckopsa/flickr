package main

// My List: the things a profile has put by for later, and the two writes
// that put them there and take them off again.
//
// It is the one shelf the library cannot derive. Everything else a home
// screen shows is an ordering of what the scan found — what arrived lately,
// what has a new episode, what is like something already watched — and this
// is the row a PERSON writes: "not now, but don't lose it". So it is the one
// new table in state.db, and it is as small as the thing it holds: a profile,
// a work key, and when.
//
// A WORK key rather than an item id, because a list is a list of titles. A
// show is one entry however many episodes come and go under it, and the tile
// the row draws is the very tile the grid draws.
//
// The list is filtered like every other shelf: it is built out of the works
// this profile may SEE (worksFor), so a title a kid may not have does not
// come back through a row somebody else saved on the same device — and a
// saved key the scan has since dropped is skipped rather than drawn as a
// hole.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/store"
	"flickr/internal/works"
)

// listHref is the shelf's own address, carrying the profile when we know who
// is asking — the list refuses without one, so the link that names it should
// be followable as it stands (the root's `continue` link is spelled the same
// way).
func listHref(profile string) string {
	if profile == "" {
		return "/api/list"
	}
	return "/api/list?client_id=" + url.QueryEscape(profile)
}

// saveHref is one work's place on the list: the address `save` posts to and
// `unsave` deletes. The key is a path segment and carries a colon, escaped
// the way workHref escapes it.
func saveHref(key string) string {
	return "/api/list/" + strings.ReplaceAll(url.PathEscape(key), ":", "%3A")
}

// listAction writes the one thing a work says about the list: `unsave` when
// it is on it, `save` when it is not.
//
// Two names rather than one toggle with a value, because these are two
// different writes — a POST that puts it there and a DELETE that takes it
// off — and a screen that draws whichever it was given never decides which
// way the press goes. Nothing at all when nobody is asking: a list belongs to
// a profile, and there is none to save it for.
func listAction(doc *hyper.Envelope, key, profile string, saved bool) {
	if profile == "" {
		return
	}
	if saved {
		doc.Action("unsave", hyper.Action{
			Method: "DELETE", Href: saveHref(key), Label: "Remove from My List",
		})
		return
	}
	doc.Action("save", hyper.Action{
		Method: "POST", Href: saveHref(key), Label: "Add to My List",
	})
}

// savedSet is the keys on one profile's list, for the documents that only
// need to know whether a thing is on it. Empty for a caller who named no
// profile — nobody has a list.
func (s *server) savedSet(profile string) (map[string]bool, error) {
	if profile == "" {
		return nil, nil
	}
	rows, err := s.state.SavedFor(profile)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.WorkKey] = true
	}
	return out, nil
}

// savedTiles is the shelf itself: the saved works as the tiles the grid
// draws, newest first, each carrying the `unsave` that takes it off again.
// The library document carries this list too, so a home draws the row without
// a second fetch.
func savedTiles(rows []store.Saved, byKey map[string]*works.Work, profile string) []*hyper.Envelope {
	out := make([]*hyper.Envelope, 0, len(rows))
	for _, r := range rows {
		wk := byKey[r.WorkKey]
		if wk == nil {
			continue // a title this profile may not see, or one the scan dropped
		}
		t := workTile(wk)
		listAction(t, wk.Key, profile, true)
		out = append(out, t)
	}
	return out
}

// handleList is GET /api/list — one profile's My List, newest first, as the
// tiles the grid draws.
func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	profile := profileOf(r)
	if profile == "" {
		hyper.WriteProblem(w, noProfileProblem("There is nobody to list it for",
			"a list belongs to one profile, and this request named none"))
		return
	}
	ws, err := s.worksFor(r)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	rows, err := s.state.SavedFor(profile)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	tiles := savedTiles(rows, works.ByKey(ws), profile)
	doc := hyper.Doc(listHref(profile), "list", "My List").
		Field("profile", profile).
		Field("count", len(tiles)).
		Field("items", tiles).
		Link("root", "/api/", "flickr").
		Link("library", "/api/library", "Library")
	hyper.WriteDoc(w, http.StatusOK, doc)
}

// handleSave is POST /api/list/{key} — a work's `save` action. It answers the
// shelf as it now stands, which is the row the press was meant to change.
func (s *server) handleSave(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	profile := profileOf(r)
	if profile == "" {
		hyper.WriteProblem(w, noProfileProblem("There is nobody to save it for",
			"a list belongs to one profile, and this request named none"))
		return
	}
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	wk := works.ByKey(ws)[key]
	if wk == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-work",
			"No such work", fmt.Sprintf("the library holds no work keyed %q", key)).
			WithRemedy("follow a tile's own save action rather than composing its address",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	// A title a profile may not see is not one it may put on its list either.
	if problem := s.refuseWork(profile, wk); problem != nil {
		hyper.WriteProblem(w, *problem)
		return
	}
	if err := s.state.Save(profile, key); err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	s.handleList(w, r)
}

// handleUnsave is DELETE /api/list/{key} — the `unsave` action.
//
// It asks the library nothing: taking a thing off a list is allowed whatever
// the thing is, and a row whose title the scan has since dropped could be
// taken off no other way. It answers the shelf, like its opposite.
func (s *server) handleUnsave(w http.ResponseWriter, r *http.Request) {
	profile := profileOf(r)
	if profile == "" {
		hyper.WriteProblem(w, noProfileProblem("There is nobody to take it off for",
			"a list belongs to one profile, and this request named none"))
		return
	}
	if err := s.state.Unsave(profile, r.PathValue("key")); err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	s.handleList(w, r)
}
