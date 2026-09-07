package main

// The share page: a passage link that unfurls.
//
// A passage IS its URL — "#/item/4?t=4740&end=5070" — and a hash never
// reaches a server, so that link pasted into the family chat is a bare
// origin with nothing to preview: no picture, no title, nothing that says
// what was sent. /s/… is the same address spelled as a PATH, which does
// reach us: one small HTML page whose Open Graph tags are the passage's own
// sentence, the work's overview and its picture, and whose whole body is a
// hand-off to the hash form. The preview is for whatever is unfurling the
// link; the person who clicks lands exactly where the link always went.
//
// Nothing is resolved twice. The path after /s IS the hash, so the route
// document's own resolver (route.go) answers it, and the sentence is
// passageSentence — the one GET /api/-/passage says. The tags are what this
// file adds, and they are the only place in the server that writes HTML.

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/store"
	"flickr/internal/works"
)

// unfurl is the card a chat window draws from the tags: what was shared, in
// one sentence, with the picture of the thing it is part of. Every field may
// be empty — a link to a library nobody has enriched still redirects.
type unfurl struct {
	Title       string
	Description string
	Image       string // absolute: a crawler is not on this page's origin
	Type        string // og:type — what medium this is, in og's own words
	Href        string // where a browser is sent: the hash form
}

// handleShare is GET /s/… — the hash grammar, said as a path.
//
// It is registered as a SUBTREE rather than one route per spelling: the hash
// is the public grammar (README "Deep links and passages") and "#" + the
// path after /s is a hash of it, so every spelling that resolves today
// unfurls today, and a spelling added to the grammar tomorrow needs no
// second route here.
func (s *server) handleShare(w http.ResponseWriter, r *http.Request) {
	hash := shareHash(r.URL)
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	base := s.absoluteBase(r)
	target := resolveRoute(hash, ws)
	card := shareCard(target, ws, base)
	card.Href = strings.TrimSuffix(base, "/") + "/" + hash
	status := http.StatusOK
	if target.Problem != nil {
		// A link to something the library no longer holds is answered
		// honestly — but it is still answered as a page, so the person who
		// clicked reaches the client and reads the same refusal in words.
		status = target.Problem.Status
	}
	writeSharePage(w, status, card)
}

// shareHash is the hash a share path means: "/s/item/4?t=4740" is
// "#/item/4?t=4740". The path is taken ESCAPED, since that is how a hash is
// written down — "#/show/The%20Office" — and the resolver unescapes it
// itself.
func shareHash(u *url.URL) string {
	hash := "#" + strings.TrimPrefix(u.EscapedPath(), "/s")
	if u.RawQuery != "" {
		hash += "?" + u.RawQuery
	}
	return hash
}

// shareCard is what the tags say about a resolved hash: the passage's
// sentence over the work's overview and its picture, and for anything that
// is not a passage the plainest true thing — the item's title, the work's,
// or the refusal's.
func shareCard(target routeTarget, ws []works.Work, base string) unfurl {
	if target.Problem != nil {
		return unfurl{Title: target.Problem.Title, Description: target.Problem.Detail,
			Type: "website"}
	}
	switch {
	case target.Item != nil:
		it := *target.Item
		wk := works.ByItem(ws)[it.ID]
		card := unfurl{Title: itemTitle(it), Description: itemOverview(it),
			Image: shareImage(&it, wk, base),
			Type:  shareType(it.MediaInfo.MediumOrVideo())}
		if wk != nil && wk.Overview != "" {
			// A passage is a piece of the WORK, and the work's blurb is what
			// a person reading the card wants: the sentence above it already
			// says which piece.
			card.Description = wk.Overview
		}
		if p := sharePassage(target.Passage); p != nil {
			var out *store.Item
			if p.Until != nil && wk != nil {
				out = memberByID(wk, *p.Until)
			}
			if sentence := passageSentence(it, out, wk, p); sentence != "" {
				card.Title = sentence
			}
		}
		return card
	case target.Work != nil:
		// A whole work is shown by the work's own picture — the poster, not
		// one episode's still, which is why no item is named here.
		wk := target.Work
		return unfurl{Title: wk.Title, Description: wk.Overview,
			Image: shareImage(nil, wk, base), Type: shareType(wk.Medium)}
	case target.Artist != "":
		return unfurl{Title: target.Artist, Type: "profile"}
	}
	return unfurl{Title: "Library", Type: "website"}
}

// shareType says what was shared in the vocabulary Open Graph has for it.
func shareType(medium string) string {
	switch medium {
	case model.MediumAudio:
		return "music.song"
	case model.MediumText:
		return "book"
	}
	return "video.other"
}

// sharePassage is a resolved route passage as the sentence's grammar holds
// one. The sections and pages the route worked out are for a reader to open
// at; a sentence says the locators themselves.
func sharePassage(p *resolvedPassage) *sessionPassage {
	if p == nil {
		return nil
	}
	return &sessionPassage{T: p.T, End: p.End, Until: p.Until, From: p.From, To: p.To}
}

// shareImage is the picture the card shows, absolute against base: this
// one file's own still, its poster, and otherwise the work's — a record's or
// a book's cover where there is no poster to be had. "" when the library has
// no picture of this at all, which is a card with no picture rather than a
// broken one.
func shareImage(it *store.Item, wk *works.Work, base string) string {
	abs := func(href string) string { return strings.TrimSuffix(base, "/") + href }
	if it != nil && it.Enrichment != nil {
		if it.Enrichment.HasStill {
			return abs(itemHref(it.ID) + "/still")
		}
		if it.Enrichment.HasPoster {
			return abs(itemHref(it.ID) + "/poster")
		}
	}
	if wk == nil {
		return ""
	}
	rep := memberByID(wk, wk.RepresentativeItemID)
	if rep == nil {
		return ""
	}
	if rep.Enrichment != nil && rep.Enrichment.HasPoster {
		return abs(itemHref(rep.ID) + "/poster")
	}
	if wk.Medium != model.MediumVideo {
		return abs(itemHref(rep.ID) + "/cover")
	}
	return ""
}

// sharePage is the whole page: the tags, a redirect for a browser that runs
// no script (a crawler, a text browser), the sentence as a link for one that
// honours neither, and the script that makes the hop instant.
//
// html/template rather than fmt: every value here is a title, a synopsis or
// a URL out of the library, and escaping them for an attribute and for a
// script string is the template's job rather than this file's.
var sharePage = template.Must(template.New("share").Parse(
	`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<meta property="og:type" content="{{.Type}}">
<meta property="og:title" content="{{.Title}}">
{{if .Description}}<meta property="og:description" content="{{.Description}}">
{{end}}{{if .Image}}<meta property="og:image" content="{{.Image}}">
<meta name="twitter:card" content="summary_large_image">
{{else}}<meta name="twitter:card" content="summary">
{{end}}<meta property="og:url" content="{{.Href}}">
<meta http-equiv="refresh" content="0; url={{.Href}}">
<link rel="canonical" href="{{.Href}}">
<p><a href="{{.Href}}">{{.Title}}</a></p>
<script>location.replace({{.Href}})</script>
`))

func writeSharePage(w http.ResponseWriter, status int, card unfurl) {
	// Rendered whole before a header is set, the way a document is
	// marshalled whole: a template that failed halfway would otherwise be a
	// truncated 200.
	var buf bytes.Buffer
	if err := sharePage.Execute(&buf, card); err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The card is a fresh read of the library every time, and a link that
	// unfurled once is unfurled again by the next chat window along.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
