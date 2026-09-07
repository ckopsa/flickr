package main

// The passage, server-side (docs/hypermedia.md §Passages are session state,
// step 4 of its order of work).
//
// A PASSAGE is a start and an end within a work: a scene from 1:19:00 to
// 1:24:30, or a run of episodes. The browser used to hold all of this —
// the grammar, the marks, the minted link — and a rule that lives only in a
// client is manners, not a rule. What lives here now is everything about a
// passage that is not a clock: how a play request's `passage` resolves, how
// a mark lands (snapped to a chapter, swapped when it is the wrong way
// round, a later member becoming `until`), the link the two marks mint, and
// the sentence that says it in words.
//
// The show form (`ep=S03E22`) never reaches PLAY: docs/hypermedia.md has
// step 2's route document resolve an episode code against the show's members
// before play, so a session takes item ids only. The MINT reads both forms —
// an item and two times, or a work and two of the place tokens its document
// publishes ("S03E22 0:00", "1:19:00", "ch. 7") — and internal/passage is the
// grammar for both: one implementation, table-tested there.

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/passage"
	"flickr/internal/store"
	"flickr/internal/works"
)

// chapterSnapSeconds is how near a chapter start a mark has to land to be
// taken as meaning it: a tap a beat after the scene change meant the scene
// change, and the scrubber's chapter ticks are the natural cut points.
const chapterSnapSeconds = 2.0

// passageInput is the optional `passage` object a play request carries. It
// is the hash grammar in JSON: seconds for the player's bounds, an item id
// for a run's last member, text locators for the reader (hyper 5 acts on
// those; they are carried and answered here so a link that has them is not
// silently narrowed).
type passageInput struct {
	T     *float64 `json:"t"`
	End   *float64 `json:"end"`
	Until *int64   `json:"until"`
	From  string   `json:"from"`
	To    string   `json:"to"`
}

// sessionPassage is a passage as the session document carries it: resolved,
// item ids throughout, and with EndsAt — the bound that applies to THIS
// session's item — spelled out, so the client's clock has nothing left to
// work out. `end` belongs to the single item of a scene or to the LAST item
// of a run; earlier items of a run play out to their natural end, and their
// document says so by having no ends_at.
type sessionPassage struct {
	T      *float64 `json:"t,omitempty"`
	End    *float64 `json:"end,omitempty"`
	Until  *int64   `json:"until,omitempty"`
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
	EndsAt *float64 `json:"ends_at,omitempty"`
}

// resolvePassage is the grammar's own reading of a play request's passage,
// for the item it is starting on.
//
// The grammar is internal/passage's, so a passage that arrived as JSON is
// spelled back into the query a link would have carried and read by that
// same parser: negative seconds, an `end` at or before `t` (README "Deep
// links and passages") and a passage that says nothing all fall away here
// exactly as they do in a link, because it is the same code deciding.
func resolvePassage(in *passageInput, itemID int64) *sessionPassage {
	if in == nil {
		return nil
	}
	spelled := (&passage.Passage{
		T: in.T, End: in.End, Until: in.Until,
		From: strings.TrimSpace(in.From), To: strings.TrimSpace(in.To),
	}).Query()
	read := passage.Parse(spelled)
	if read == nil {
		return nil
	}
	p := &sessionPassage{T: read.T, End: read.End, Until: read.Until, From: read.From, To: read.To}
	p.EndsAt = endsAt(p, itemID)
	return p
}

// endsAt is the end bound that applies while itemID plays.
// text reports whether this passage says something about a book: from or to
// present. The clock-side counterpart is a `t` or an `end`; an item is a
// film or a book, so the two never both apply.
func (p *sessionPassage) text() bool {
	return p != nil && (p.From != "" || p.To != "")
}

func endsAt(p *sessionPassage, itemID int64) *float64 {
	if p == nil || p.End == nil {
		return nil
	}
	if p.Until != nil && *p.Until != itemID {
		return nil
	}
	return p.End
}

// ── marks ───────────────────────────────────────────────────────────────
//
// The producer side. A session keeps an in point and an out point, each a
// place in one item of the work, and these are the whole logic of them.

// mark is one bound of a passage being made: seconds into one item.
type mark struct {
	ItemID  int64   `json:"item_id"`
	Seconds float64 `json:"seconds"`
}

// marks is a session's pair of them; either may be absent.
type marks struct {
	In  *mark `json:"in,omitempty"`
	Out *mark `json:"out,omitempty"`
}

func (m marks) any() bool { return m.In != nil || m.Out != nil }

// markSeconds spells a mark's position the way the grammar does: whole
// seconds stay integers, anything else is rounded to one decimal (79.5).
// Nonsense is the start of the file.
func markSeconds(pos float64) float64 {
	if pos != pos || pos < 0 { // NaN, or before the beginning
		return 0
	}
	return float64(int64(pos*10+0.5)) / 10
}

// snapToChapter is the chapter start within tolerance of pos (the nearest,
// if several), else pos itself.
func snapToChapter(pos float64, starts []float64, tolerance float64) float64 {
	best, bestD := pos, tolerance
	for _, s := range starts {
		d := s - pos
		if d < 0 {
			d = -d
		}
		if d <= tolerance && d <= bestD {
			best, bestD = s, d
		}
	}
	return best
}

// setMark is the mark state after marking `kind` ("in" | "out") at m.
// `order` is the ids of the work's members in playing order, so a mark on a
// later episode counts as later. An out point at or before the in point
// swaps the two — and marking in past the out point does the same; the
// person said where the passage is, not which end they meant. Two marks on
// the very same instant are one point, not a passage: the out is dropped.
func setMark(cur marks, kind string, m mark, order []int64) marks {
	in, out := cur.In, cur.Out
	if kind == "in" {
		in = &m
	} else {
		out = &m
	}
	if in == nil || out == nil {
		return marks{In: in, Out: out}
	}
	switch c := compareMarks(*in, *out, order); {
	case c == 0:
		out = nil
	case c > 0:
		in, out = out, in
	}
	return marks{In: in, Out: out}
}

// clearMark removes one end.
func clearMark(cur marks, kind string) marks {
	if kind == "in" {
		return marks{Out: cur.Out}
	}
	return marks{In: cur.In}
}

// compareMarks orders two marks: by their item's place in the work first,
// by seconds within one item.
func compareMarks(a, b mark, order []int64) int {
	ai, bi := placeIn(order, a.ItemID), placeIn(order, b.ItemID)
	switch {
	case ai != bi:
		return ai - bi
	case a.Seconds < b.Seconds:
		return -1
	case a.Seconds > b.Seconds:
		return 1
	}
	return 0
}

func placeIn(order []int64, id int64) int {
	for i, v := range order {
		if v == id {
			return i
		}
	}
	return 0
}

// markedPassage is the marks as a passage: the out point's item becomes
// `until` when it is a later member than the in point's. nil until an in
// point exists — a passage needs somewhere to start.
func markedPassage(m marks) *sessionPassage {
	if m.In == nil {
		return nil
	}
	t := m.In.Seconds
	p := &sessionPassage{T: &t}
	if m.Out != nil {
		end := m.Out.Seconds
		p.End = &end
		if m.Out.ItemID != m.In.ItemID {
			until := m.Out.ItemID
			p.Until = &until
		}
	}
	p.EndsAt = endsAt(p, m.In.ItemID)
	return p
}

// ── the minted link, and its sentence ───────────────────────────────────

// grammar is this passage as internal/passage holds one — the shape the
// grammar's own functions take. `ends_at` is not part of it: that is what
// this server worked out for one session, not something a link says.
func (p *sessionPassage) grammar() *passage.Passage {
	if p == nil {
		return nil
	}
	return &passage.Passage{T: p.T, End: p.End, Until: p.Until, From: p.From, To: p.To}
}

// passageQuery is the hash grammar's query for a resolved passage — the
// package's own spelling, so a link minted here and a link parsed there are
// the same string.
func passageQuery(p *sessionPassage) string { return p.grammar().Query() }

// passageHash is the passage as a deep link's hash, item form.
func passageHash(itemID int64, p *sessionPassage) string {
	return "#/item/" + strconv.FormatInt(itemID, 10) + passageQuery(p)
}

// mintedLink is the absolute link to a passage: the web shell's own address
// with the passage's hash on it. base is the origin the caller reached us
// on (absoluteBase), so a link copied on the LAN works on the LAN.
func mintedLink(base string, itemID int64, p *sessionPassage) string {
	return strings.TrimSuffix(base, "/") + "/" + passageHash(itemID, p)
}

// absoluteBase is the origin to mint links on: the one the CALLER used, so
// a person copying a link from the page gets the address they are already
// on. ADVERTISE_URL stands in when there is no Host header to read (a
// synthetic request), since that is what the server tells cast devices.
func (s *server) absoluteBase(r *http.Request) string {
	if r.Host == "" {
		return s.baseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

// passageLabel is how a sentence names one member inside its work — an
// episode's code, a part's or a track's number — and nothing at all for a
// work that is one file: a film is named by its title at the end of the
// sentence, not twice.
func passageLabel(it store.Item) string {
	id := it.Identity
	if id == nil {
		return ""
	}
	switch id.Kind {
	case "episode":
		if id.Episode > 0 {
			return fmt.Sprintf("S%02dE%02d", id.Season, id.Episode)
		}
	case "audiobook_part":
		if id.Part > 0 {
			return fmt.Sprintf("Part %d", id.Part)
		}
	case "track":
		if id.Part > 0 {
			return fmt.Sprintf("Track %d", id.Part)
		}
	}
	return ""
}

// passageSentence says a passage in words, the way a person would read it
// out: "S03E22 2:22 – 14:14 of Beach Games", "1:19:00 – 1:24:30 of 12 Angry
// Men". A passage with no end reads "from 2:22 of …"; a run across members
// names both ends and the WORK, since the two halves are in different files
// — "S03E22 2:22 – S03E23 14:14 of The Office".
func passageSentence(in store.Item, out *store.Item, wk *works.Work, p *sessionPassage) string {
	if p == nil {
		return ""
	}
	if p.T == nil {
		if !p.text() {
			return ""
		}
		return textSentence(p) + " of " + itemTitle(in)
	}
	left := joinWords(passageLabel(in), works.Clock(*p.T))
	title := itemTitle(in)
	if out != nil && out.ID != in.ID {
		right := passageLabel(*out)
		if p.End != nil {
			right = joinWords(right, works.Clock(*p.End))
		}
		if wk != nil && wk.Title != "" {
			title = wk.Title
		}
		return left + " – " + right + " of " + title
	}
	if p.End == nil {
		return joinWords(passageLabel(in), "from "+works.Clock(*p.T)) + " of " + title
	}
	return left + " – " + works.Clock(*p.End) + " of " + title
}

// textSentence is a book's passage in words: "ch. 3 – ch. 4", "from 40%".
// The bounds are locators, four spellings of a place, and each is said the
// way a person would say it rather than the way a link spells it.
func textSentence(p *sessionPassage) string {
	from, to := locatorWords(p.From), locatorWords(p.To)
	switch {
	case from != "" && to != "":
		return from + " – " + to
	case from != "":
		return "from " + from
	}
	return "to " + to
}

// locatorWords says one locator: a section, a page, a percentage, or a place
// somebody marked in the text and nobody can read out.
func locatorWords(spelled string) string {
	l := passage.ParseLocator(spelled)
	if l == nil {
		return ""
	}
	switch l.Kind {
	case "ch":
		return "ch. " + strconv.Itoa(l.N)
	case "pg":
		return "p. " + strconv.Itoa(l.N)
	case "pct":
		return strconv.FormatFloat(l.F*100, 'f', -1, 64) + "%"
	}
	return "a marked place"
}

func joinWords(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}

// ── GET /api/-/passage ──────────────────────────────────────────────────

// handlePassageDoc mints a link from a passage spelled in the grammar and
// answers it with the sentence that says it: the document the work's
// `passage` action points at.
//
// Two spellings reach here. The ITEM form — item=<id>&t=&end=[&until=] — is
// the grammar as a link already spells it. The WORK form —
// work=<key>&from=<place>&to=<place> — is the grammar as a PERSON picks it,
// two of the place tokens the work document publishes; it is resolved
// against that work's members below.
func (s *server) handlePassageDoc(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if key := q.Get("work"); key != "" {
		s.mintFromPlaces(w, r, key, q.Get("from"), q.Get("to"))
		return
	}
	raw := q.Get("item")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("name the item a passage starts in: item=<id>&t=<seconds>&end=<seconds>",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
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
			WithRemedy("open the library and follow an item from there",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	in := &passageInput{T: querySeconds(q, "t"), End: querySeconds(q, "end"),
		From: q.Get("from"), To: q.Get("to")}
	if u, err := strconv.ParseInt(q.Get("until"), 10, 64); err == nil {
		in.Until = &u
	}
	p := resolvePassage(in, id)
	if p == nil || (p.T == nil && !p.text()) {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "empty-passage",
			"That is not a passage",
			"a passage needs somewhere to start: t=<seconds>, or from=<locator> for a book").
			WithRemedy("say where it starts, and where it ends: item=<id>&t=<seconds>&end=<seconds>",
				&hyper.Link{Href: itemHref(id), Title: itemTitle(*item)}))
		return
	}
	var out *store.Item
	var wk *works.Work
	if p.Until != nil {
		ws, err := s.buildWorks()
		if err != nil {
			hyper.WriteProblem(w, serverProblem(err))
			return
		}
		if wk = works.ByItem(ws)[id]; wk != nil {
			out = memberByID(wk, *p.Until)
		}
		if out == nil || out.ID != *p.Until {
			hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "run-leaves-the-work",
				"A run ends inside its own work",
				fmt.Sprintf("item %d is not a member of the work item %d belongs to", *p.Until, id)).
				WithRemedy("name a later member of the same work as `until`, or drop it",
					&hyper.Link{Href: itemHref(id), Title: itemTitle(*item)}))
			return
		}
	}
	self := "/api/-/passage?item=" + strconv.FormatInt(id, 10)
	if pq := passageQuery(p); pq != "" {
		self += "&" + strings.TrimPrefix(pq, "?")
	}
	hyper.WriteDoc(w, http.StatusOK, passageDoc(self, s.absoluteBase(r), *item, out, wk, p))
}

// ── the work form: two of the places a work publishes ───────────────────

// mintFromPlaces answers work=<key>&from=<place>&to=<place>: the place
// TOKENS the work document's `places` publishes, resolved against that
// work's members. A show's places name the episode and a time inside it
// ("S03E22 0:00"), a film's, an audiobook's and an album's are times in the
// one file the work counts by ("1:19:00"), and a book's are spine sections
// ("ch. 7"). Two of them are a passage; which item it starts in, whether it
// runs across members, and which way round they were given are worked out
// here rather than asked of the person.
func (s *server) mintFromPlaces(w http.ResponseWriter, r *http.Request, key, fromTok, toTok string) {
	ws, err := s.buildWorks()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	wk := works.ByKey(ws)[key]
	if wk == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-work",
			"No such work", fmt.Sprintf("the library holds no work keyed %q", key)).
			WithRemedy("open the library and follow a work from there",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	if strings.TrimSpace(fromTok) == "" && strings.TrimSpace(toTok) == "" {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "empty-passage",
			"That is not a passage", "a passage is two places in a work, and neither was given").
			WithRemedy("name two of the places this work publishes",
				&hyper.Link{Href: workHref(wk.Key), Title: wk.Title}))
		return
	}

	var (
		in, out *store.Item
		p       *sessionPassage
		refusal error
	)
	if wk.Medium == model.MediumText {
		in, p, refusal = textPassageOf(wk, fromTok, toTok)
	} else {
		in, out, p, refusal = timedPassageOf(wk, fromTok, toTok)
	}
	if refusal != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "no-such-place",
			"That is not a place in this work", refusal.Error()).
			WithRemedy("the work document lists the places it offers, spelled as they are taken",
				&hyper.Link{Href: workHref(wk.Key), Title: wk.Title}))
		return
	}
	self := "/api/-/passage?work=" + url.QueryEscape(wk.Key)
	if fromTok != "" {
		self += "&from=" + url.QueryEscape(fromTok)
	}
	if toTok != "" {
		self += "&to=" + url.QueryEscape(toTok)
	}
	hyper.WriteDoc(w, http.StatusOK, passageDoc(self, s.absoluteBase(r), *in, out, wk, p))
}

// timedPassageOf reads two clock-side places into a passage. The pair is put
// through the same marks the player makes, so a run across members and a
// pair given the wrong way round are handled once, in one place.
func timedPassageOf(wk *works.Work, fromTok, toTok string) (in, out *store.Item, p *sessionPassage, err error) {
	m := marks{}
	order := memberOrder(wk)
	for _, side := range []struct {
		kind, token string
	}{{"in", fromTok}, {"out", toTok}} {
		if strings.TrimSpace(side.token) == "" {
			continue
		}
		it, secs, err := placeInWork(wk, side.token)
		if err != nil {
			return nil, nil, nil, err
		}
		m = setMark(m, side.kind, mark{ItemID: it.ID, Seconds: markSeconds(secs)}, order)
	}
	if m.In == nil {
		// `to` alone: a passage from the top of that place's item to there.
		if m.Out == nil {
			return nil, nil, nil, fmt.Errorf("neither place could be read")
		}
		m = marks{In: &mark{ItemID: m.Out.ItemID}, Out: m.Out}
		if m.In.Seconds == m.Out.Seconds {
			m.Out = nil
		}
	}
	in = memberByID(wk, m.In.ItemID)
	if in == nil {
		return nil, nil, nil, fmt.Errorf("that place is not in this work")
	}
	if m.Out != nil && m.Out.ItemID != m.In.ItemID {
		out = memberByID(wk, m.Out.ItemID)
	}
	return in, out, markedPassage(m), nil
}

// placeInWork is one clock-side place token as an item and a time in it:
// "S03E22 14:14" is that episode at that time (the code alone is its start),
// and a bare "1:19:00" is that time in the file the work counts by — which
// for a show would name no episode at all, so a show is asked for the code.
func placeInWork(wk *works.Work, token string) (*store.Item, float64, error) {
	tok := strings.TrimSpace(token)
	if code, rest := splitEpisodeCode(tok); code != "" {
		ep, _ := passage.ParseEpisodeCode(code)
		member := passage.FindEpisode(membersOf(wk), ep)
		if member == nil {
			return nil, 0, fmt.Errorf("%s has no episode %s", wk.Title, ep)
		}
		it := memberByID(wk, member.ID)
		if rest == "" {
			return it, 0, nil
		}
		secs, ok := parseClock(rest)
		if !ok {
			return nil, 0, fmt.Errorf("%q is not a time like 14:14", rest)
		}
		return it, secs, nil
	}
	secs, ok := parseClock(tok)
	if !ok {
		return nil, 0, fmt.Errorf("%q is not a place in this work: its places are times like 1:19:00", tok)
	}
	if wk.Kind == "show" {
		return nil, 0, fmt.Errorf("%q names no episode: a show's places are spelled like S03E22 %s", tok, tok)
	}
	it := chapterSource(wk)
	if it == nil {
		it = memberByID(wk, wk.RepresentativeItemID)
	}
	if it == nil {
		return nil, 0, fmt.Errorf("this work has no file to count time in")
	}
	return it, secs, nil
}

// textPassageOf reads two places in a book: its spine sections as the work
// document publishes them ("ch. 7"), or any locator spelling the grammar
// takes (ch:7, pct:0.4, pg:213, cfi:epubcfi(…)).
func textPassageOf(wk *works.Work, fromTok, toTok string) (*store.Item, *sessionPassage, error) {
	it := chapterSource(wk)
	if it == nil {
		it = memberByID(wk, wk.RepresentativeItemID)
	}
	if it == nil {
		return nil, nil, fmt.Errorf("this work has no book to read")
	}
	p := &sessionPassage{}
	for _, side := range []struct {
		into  *string
		token string
	}{{&p.From, fromTok}, {&p.To, toTok}} {
		tok := strings.TrimSpace(side.token)
		if tok == "" {
			continue
		}
		loc := passage.ParseLocator(spellLocator(tok))
		if loc == nil {
			return nil, nil, fmt.Errorf("%q is not a place in a book: its places are spelled like \"ch. 7\"", tok)
		}
		*side.into = loc.String()
	}
	if p.From == "" && p.To == "" {
		return nil, nil, fmt.Errorf("neither place could be read")
	}
	return it, p, nil
}

var episodeCodeRe = regexp.MustCompile(`^[Ss](\d{1,3})[Ee](\d{1,4})\b\s*(.*)$`)

// splitEpisodeCode takes an episode code off the front of a place token:
// "S03E22 14:14" → "S03E22", "14:14". "" when the token starts with no code.
func splitEpisodeCode(token string) (code, rest string) {
	m := episodeCodeRe.FindStringSubmatch(strings.TrimSpace(token))
	if m == nil {
		return "", token
	}
	return "S" + m[1] + "E" + m[2], strings.TrimSpace(m[3])
}

// spellLocator turns the work document's own place spelling into the
// grammar's: "ch. 7" and "ch 7" are the label a chip takes, "ch:7" is what a
// link carries, and they mean the same section.
func spellLocator(token string) string {
	t := strings.TrimSpace(token)
	if m := regexp.MustCompile(`^(?i)(ch|pg|pct)\.?\s+(\S+)$`).FindStringSubmatch(t); m != nil {
		return strings.ToLower(m[1]) + ":" + m[2]
	}
	return t
}

// parseClock reads a time the way the places are spelled: "1:19:00",
// "14:14", and the bare seconds a link carries ("4740", "79.5").
func parseClock(s string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) > 3 {
		return 0, false
	}
	total := 0.0
	for i, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || v < 0 || (i < len(parts)-1 && v != math.Trunc(v)) {
			return 0, false
		}
		total = total*60 + v
	}
	return total, true
}

// passageDoc is one minted passage as a document: the link, the sentence,
// and the item it starts in. GET /api/-/passage and a session's `link`
// action both answer it, so a copy button renders the same way whether the
// passage was marked while watching or composed from two places.
func passageDoc(self, base string, in store.Item, out *store.Item, wk *works.Work, p *sessionPassage) *hyper.Envelope {
	return hyper.Doc(self, "passage", "").
		Field("href", mintedLink(base, in.ID, p)).
		Field("sentence", passageSentence(in, out, wk, p)).
		Field("passage", p).
		Link("item", itemHref(in.ID), itemTitle(in))
}

func querySeconds(q url.Values, name string) *float64 {
	v, err := strconv.ParseFloat(q.Get(name), 64)
	if err != nil {
		return nil
	}
	return &v
}
