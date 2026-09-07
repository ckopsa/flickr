// Package passage is flickr's passage grammar: the one implementation.
//
// A PASSAGE is a start and an end within a work — a scene from 1:19:00 to
// 1:24:30, episodes 5 to 6, chapters 3 and 4 of a book. Other systems (the
// household's day planner) mint links to passages, so the grammar is fixed
// and documented in README.md ("Deep links and passages"). It rides on the
// query part of a hash route:
//
//	#/item/<id>?t=<start_seconds>&end=<end_seconds>
//	  &until=<item_id>      episode RUN: playback continues through episodes
//	                        and stops after the item with that id (`end` then
//	                        applies within that last item)
//	#/item/<id>?from=<locator>&to=<locator>
//	                        a TEXT passage: the reader opens at from and stops
//	                        at to. A locator is cfi:<epub cfi>, ch:<n> (1-based
//	                        spine section), pct:<0..1> or pg:<n> (a PDF's
//	                        1-based page)
//	#/show/<title>?ep=S02E05&t=…&end=…[&until=S02E07]
//	                        the same passage addressed by show and episode
//	                        code, for a minter that knows no item ids; it is
//	                        resolved against the show's members (ResolveShow)
//	                        and then behaves exactly like the item form
//
// This package was web/passage.js until 2026-09-07 (docs/hypermedia.md,
// §Routes are resolved by the server): the browser now asks
// GET /api/-/route and parses nothing past splitting the hash off the URL.
// Its table test carries the same cases, under the same names, as
// web/passage_test.mjs, so the two files can be read side by side.
//
// Everything here is pure: no store, no request, no clock.
package passage

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Passage is what a hash's query says about where a work is entered and
// left. Every field is optional; a nil *Passage is "no passage at all".
//
// Until is an item id, the item form's spelling of a run's last episode. A
// non-numeric until (an episode code) lands in UntilEp as written, and Ep is
// kept as written too — both are resolved, and their spelling judged, by
// ResolveShow.
type Passage struct {
	T       *float64 `json:"t,omitempty"`
	End     *float64 `json:"end,omitempty"`
	Until   *int64   `json:"until,omitempty"`
	UntilEp string   `json:"until_ep,omitempty"`
	Ep      string   `json:"ep,omitempty"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
}

// Split separates a hash's path from its query:
// "#/item/51?t=4740&end=5070" → "#/item/51", "t=4740&end=5070".
func Split(hash string) (path, query string) {
	if i := strings.IndexByte(hash, '?'); i >= 0 {
		return hash[:i], hash[i+1:]
	}
	return hash, ""
}

var (
	secondsRe = regexp.MustCompile(`^\d+(\.\d+)?$`)
	itemIDRe  = regexp.MustCompile(`^\d+$`)
)

// parseSeconds reads non-negative seconds, integer or decimal ("4740",
// "79.5"); anything else is treated as absent rather than guessed at.
func parseSeconds(v string) *float64 {
	if !secondsRe.MatchString(v) {
		return nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return nil
	}
	return &n
}

func parseItemID(v string) *int64 {
	if !itemIDRe.MatchString(v) {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// Parse reads the query part of a hash (with or without the leading "?")
// and returns the passage it names, or nil when it names none. A malformed
// value is absent rather than guessed at, and an end at or before the start
// is no bound and is dropped.
func Parse(query string) *Passage {
	q := strings.TrimPrefix(query, "?")
	if q == "" {
		return nil
	}
	// A query flickr did not mint may carry a malformed escape; url.ParseQuery
	// reports it and still hands back every pair it could read, which is what
	// URLSearchParams did for the browser.
	vals, _ := url.ParseQuery(q)
	p := &Passage{
		T:    parseSeconds(vals.Get("t")),
		End:  parseSeconds(vals.Get("end")),
		Ep:   vals.Get("ep"),
		From: vals.Get("from"),
		To:   vals.Get("to"),
	}
	if until := vals.Get("until"); until != "" {
		if id := parseItemID(until); id != nil {
			p.Until = id
		} else {
			p.UntilEp = until
		}
	}
	if p.End != nil && p.T != nil && *p.End <= *p.T {
		p.End = nil
	}
	if p.empty() {
		return nil
	}
	return p
}

func (p *Passage) empty() bool {
	return p.T == nil && p.End == nil && p.Until == nil &&
		p.UntilEp == "" && p.Ep == "" && p.From == "" && p.To == ""
}

// Query is the inverse of Parse: "" for nil, else
// "?t=…&end=…&until=…&ep=…&from=…&to=…" with only the present fields, in
// that order, so the same passage always spells the same hash.
func (p *Passage) Query() string {
	if p == nil {
		return ""
	}
	var parts []string
	if p.T != nil {
		parts = append(parts, "t="+Seconds(*p.T))
	}
	if p.End != nil {
		parts = append(parts, "end="+Seconds(*p.End))
	}
	switch {
	case p.Until != nil:
		parts = append(parts, "until="+strconv.FormatInt(*p.Until, 10))
	case p.UntilEp != "":
		parts = append(parts, "until="+escapeComponent(p.UntilEp))
	}
	if p.Ep != "" {
		parts = append(parts, "ep="+escapeComponent(p.Ep))
	}
	if p.From != "" {
		parts = append(parts, "from="+escapeComponent(p.From))
	}
	if p.To != "" {
		parts = append(parts, "to="+escapeComponent(p.To))
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

// Seconds spells a number of seconds the way the grammar does: whole seconds
// as integers ("4740"), anything else with the decimals it has ("79.5",
// "330.25"). It is JavaScript's String(n) for the values the grammar admits,
// which is what every minted link out there was written with.
func Seconds(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// escapeComponent is JavaScript's encodeURIComponent: every byte outside
// A-Za-z0-9 and -_.!~*'() percent-encoded, a space as %20 rather than the
// '+' url.QueryEscape writes. A passage must spell one way on both sides —
// "?from=epubcfi(%2F6%2F4!%2F4%2F2)" is the form the day planner holds.
func escapeComponent(s string) string {
	const safe = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// IsTimed reports whether the passage puts the PLAYER into passage mode: it
// says something about time or episodes. from/to alone are the reader's
// business.
func (p *Passage) IsTimed() bool {
	return p != nil && (p.T != nil || p.End != nil || p.Until != nil)
}

// IsText reports whether the passage says something about the text: from or
// to present. An item is a film or a book, so the two never both apply to
// one route.
func (p *Passage) IsText() bool {
	return p != nil && (p.From != "" || p.To != "")
}

// ForNext is the passage the NEXT item of a run is routed with: `until`
// carries on, t/end were the first item's (and from/to are the reader's).
// nil when this is no run.
func (p *Passage) ForNext() *Passage {
	if p == nil || p.Until == nil {
		return nil
	}
	until := *p.Until
	return &Passage{Until: &until}
}

// ── episode codes ───────────────────────────────────────────────────────

// Episode is a season and an episode number, as an episode code names them.
type Episode struct {
	Season int
	Number int
}

func (e Episode) String() string { return fmt.Sprintf("S%02dE%02d", e.Season, e.Number) }

var episodeCodeRe = regexp.MustCompile(`^\s*[sS](\d{1,3})[eE](\d{1,4})\s*$`)

// ParseEpisodeCode reads "S02E05" / "s2e5" in any case, and nothing else.
func ParseEpisodeCode(s string) (Episode, bool) {
	m := episodeCodeRe.FindStringSubmatch(s)
	if m == nil {
		return Episode{}, false
	}
	season, _ := strconv.Atoi(m[1])
	number, _ := strconv.Atoi(m[2])
	return Episode{Season: season, Number: number}, true
}

// Member is one member of a show as the resolver sees it: its item id and
// the numbers its identity carries. Kind is the identity's kind — bonus
// material ("extra") named after an episode is never that episode, and a
// member with no kind at all is read as an episode, the way an unnumbered
// rip is.
type Member struct {
	ID      int64
	Kind    string
	Season  int
	Episode int
}

// FindEpisode is the member matching a code: season and episode numbers,
// with zero values meaning "unnumbered" the way the store omits them. nil
// when the show has no such episode.
func FindEpisode(members []Member, e Episode) *Member {
	for i := range members {
		m := &members[i]
		if m.Kind != "" && m.Kind != "episode" {
			continue
		}
		if m.Season == e.Season && m.Episode == e.Number {
			return m
		}
	}
	return nil
}

// ResolveShow turns a show-addressed passage into the item form: the member
// `ep` names, and a passage ready for an item route (until resolved to an
// id, ep dropped). A run whose `until` names no episode is an error too —
// silently shortening the run would play something other than what the link
// says. The error is one plain sentence for a person to read.
func ResolveShow(p *Passage, members []Member) (*Member, *Passage, error) {
	ep := ""
	if p != nil {
		ep = p.Ep
	}
	want, ok := ParseEpisodeCode(ep)
	if !ok {
		return nil, nil, fmt.Errorf("%q is not an episode code like S02E05.", ep)
	}
	item := FindEpisode(members, want)
	if item == nil {
		return nil, nil, fmt.Errorf("This show has no episode %s.", want)
	}
	until := p.Until
	if p.UntilEp != "" {
		var u *Member
		w, ok := ParseEpisodeCode(p.UntilEp)
		if ok {
			u = FindEpisode(members, w)
		}
		if u == nil {
			named := p.UntilEp
			if ok {
				named = w.String()
			}
			return nil, nil, fmt.Errorf("This show has no episode %s to run until.", named)
		}
		id := u.ID
		until = &id
	}
	return item, &Passage{T: p.T, End: p.End, Until: until, From: p.From, To: p.To}, nil
}

// ── text locators ───────────────────────────────────────────────────────

// Locator is one place in a book, in one of four spellings: "cfi" (a point,
// as the EPUB reader itself reports it), "ch" (the n-th spine section,
// 1-based, the way media_info.chapters lists them), "pct" (the book's own
// percentage) or "pg" (the n-th page of a PDF, 1-based). cfi and ch mean
// nothing to a PDF and pg nothing to an EPUB: the reader that gets one
// ignores it.
type Locator struct {
	Kind string  `json:"kind"`
	CFI  string  `json:"cfi,omitempty"`
	N    int     `json:"n,omitempty"`
	F    float64 `json:"f,omitempty"`
}

var (
	cfiRe      = regexp.MustCompile(`^epubcfi\(.*\)$`)
	countRe    = regexp.MustCompile(`^[1-9]\d*$`)
	fractionRe = regexp.MustCompile(`^(0(\.\d+)?|1(\.0+)?|\.\d+)$`)
	spineRe    = regexp.MustCompile(`^\s*epubcfi\(/\d+/(\d+)`)
)

// ParseLocator reads the four spellings and nothing else. A bare
// "epubcfi(…)" is read as a cfi locator too — the reader's progress speaks
// that form.
func ParseLocator(s string) *Locator {
	v := strings.TrimSpace(s)
	if v == "" {
		return nil
	}
	if cfiRe.MatchString(v) {
		return &Locator{Kind: "cfi", CFI: v}
	}
	i := strings.IndexByte(v, ':')
	if i < 0 {
		return nil
	}
	kind, rest := strings.ToLower(v[:i]), v[i+1:]
	switch kind {
	case "cfi":
		if cfiRe.MatchString(rest) {
			return &Locator{Kind: kind, CFI: rest}
		}
	case "ch", "pg":
		if countRe.MatchString(rest) {
			n, _ := strconv.Atoi(rest)
			return &Locator{Kind: kind, N: n}
		}
	case "pct":
		if fractionRe.MatchString(rest) {
			f, _ := strconv.ParseFloat(rest, 64)
			return &Locator{Kind: kind, F: f}
		}
	}
	return nil
}

// String is the canonical spelling of a locator, the inverse of
// ParseLocator; "" for nil or a kind that is none of the four.
func (l *Locator) String() string {
	if l == nil {
		return ""
	}
	switch l.Kind {
	case "cfi":
		return "cfi:" + l.CFI
	case "ch":
		return "ch:" + strconv.Itoa(l.N)
	case "pct":
		return "pct:" + strconv.FormatFloat(l.F, 'f', -1, 64)
	case "pg":
		return "pg:" + strconv.Itoa(l.N)
	}
	return ""
}

// SectionFromCFI is the 1-based spine section a CFI points into, read off
// its spine step: the itemref is an even child index, so "epubcfi(/6/14!/4/2)"
// is section 14/2 = 7. Mirrors model.SectionFromCFI. 0 when the string
// carries no such step.
func SectionFromCFI(cfi string) int {
	m := spineRe.FindStringSubmatch(cfi)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 2 || n%2 != 0 {
		return 0
	}
	return n / 2
}

// Section is the spine section (1-based) a locator lands in, for a book of
// `sections` spine items: ch is itself, pct is proportional (1 lands in the
// last section), cfi is read off the string; clamped into [1, sections]. 0
// when it cannot be told (a page says nothing about sections), or the book
// has no sections.
func (l *Locator) Section(sections int) int {
	if l == nil || sections <= 0 {
		return 0
	}
	n := 0
	switch l.Kind {
	case "ch":
		n = l.N
	case "pct":
		n = int(math.Floor(l.F*float64(sections))) + 1
	case "cfi":
		n = SectionFromCFI(l.CFI)
	}
	if n == 0 {
		return 0
	}
	return min(sections, max(1, n))
}

// Page is the page (1-based) a locator lands on, for a PDF of `pages` pages:
// pg is itself, pct is proportional (1 lands on the last page); clamped into
// [1, pages]. 0 when it cannot be told (a section or a CFI says nothing
// about pages — those are an EPUB's spellings), or the file's page count is
// unknown. The page counterpart of Section: a PDF has pages where an EPUB
// has sections, and the reader is handed a place rather than a spelling.
func (l *Locator) Page(pages int) int {
	if l == nil || pages <= 0 {
		return 0
	}
	n := 0
	switch l.Kind {
	case "pg":
		n = l.N
	case "pct":
		n = int(math.Floor(l.F*float64(pages))) + 1
	}
	if n == 0 {
		return 0
	}
	return min(pages, max(1, n))
}
