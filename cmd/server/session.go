package main

// The session document (docs/hypermedia.md §Passages are session state).
//
// Until now a "session" was an ffmpeg process: a transcode had an id and a
// direct play had nothing, so there was no server-side answer to "what is
// this person watching, and under what passage?". Every play gets a row
// here now — direct play included — and that row IS the session document:
// the url, the method, the decision trace, the passage resolved, the marks
// being made, and the actions the player may take next.
//
// The three rules of a passage move here with it. The server seeds the seek
// from `t` (start there), publishes `passage.ends_at` for the client's clock
// (stop there), and REFUSES a progress write while a passage is on — 409
// application/problem+json, type "passage-on", with `keep_watching` as the
// remedy. A rule the client merely observed is now a rule the server holds.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"flickr/internal/decision"
	"flickr/internal/hyper"
	"flickr/internal/model"
	"flickr/internal/pipeline"
	"flickr/internal/store"
	"flickr/internal/works"
)

// ── the registry ────────────────────────────────────────────────────────

// playSession is one play: what the client was handed, what was decided,
// and the passage and marks that belong to this sitting rather than to the
// library. It is in memory only — a session dies with the server, as the
// stream it names does.
type playSession struct {
	ID       string // the pipeline session id for a transcode; minted for a direct play
	Pipeline string // "" for a direct play: there is no ffmpeg to stop
	ItemID   int64
	ClientID string
	WorkKey  string // the work the item belongs to; marks live within one work
	Method   string
	URL      string
	Encoder  string
	// Seek is where these bytes begin: for a transcode the offset the
	// server baked into the stream (so the receiver's own clock runs from
	// there), for a direct play the position the client asked to start at.
	Seek     float64
	Decision model.PlayDecision
	// Caps and the track selections are kept so the run's next member can be
	// started on the same device without the client re-stating itself.
	Caps       model.ClientCapabilities
	AudioTrack *int
	BurnSub    *int
	Passage    *sessionPassage
	Marks      marks
	StartedAt  time.Time
	touched    time.Time
}

// playSessions is every live play, by id. Its zero value is usable — a
// server built in a test has one without being told to.
type playSessions struct {
	mu   sync.Mutex
	rows map[string]*playSession
	// lastMarks is the half-made passage, kept by profile and work for a
	// moment after the session it was made in has gone (markMemoTTL).
	lastMarks map[string]markMemo
}

// markMemo is one profile's marks in one work, and when they were last
// touched.
type markMemo struct {
	marks marks
	at    time.Time
}

// markMemoTTL is how long marks outlive the session they were made in. A
// run is marked ACROSS an episode advance — in on one episode, out on a
// later one — and each episode is its own session: the client stops the
// first a moment before it starts the second, so without this the out point
// would land in an empty session and no run could be marked at all. Only
// that gap is bridged. Marks are not a library record: a player closed and
// come back to later starts clean, as it always did.
const markMemoTTL = 2 * time.Minute

func memoKey(clientID, workKey string) string { return clientID + "\x00" + workKey }

// remember keeps a row's marks past the row, and forgets them when there
// are none left to keep.
func (p *playSessions) remember(row playSession) {
	if row.WorkKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastMarks == nil {
		p.lastMarks = map[string]markMemo{}
	}
	if !row.Marks.any() {
		delete(p.lastMarks, memoKey(row.ClientID, row.WorkKey))
		return
	}
	p.lastMarks[memoKey(row.ClientID, row.WorkKey)] = markMemo{marks: row.Marks, at: time.Now()}
}

func (p *playSessions) put(row playSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]*playSession{}
	}
	row.touched = time.Now()
	if row.StartedAt.IsZero() {
		row.StartedAt = row.touched
	}
	p.rows[row.ID] = &row
}

// get is a COPY of one row: the caller renders a document from it without
// holding the lock, and cannot change the registry by accident.
func (p *playSessions) get(id string) (playSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	row, ok := p.rows[id]
	if !ok {
		return playSession{}, false
	}
	row.touched = time.Now()
	return *row, true
}

// update changes one row under the lock and answers what it became.
func (p *playSessions) update(id string, fn func(*playSession)) (playSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	row, ok := p.rows[id]
	if !ok {
		return playSession{}, false
	}
	fn(row)
	row.touched = time.Now()
	return *row, true
}

func (p *playSessions) drop(id string) {
	p.mu.Lock()
	row, ok := p.rows[id]
	var gone playSession
	if ok {
		gone = *row
	}
	delete(p.rows, id)
	p.mu.Unlock()
	if ok {
		// The stream is over; the half-made passage is not, quite: the next
		// episode of a run is stopped and started in that order.
		p.remember(gone)
	}
}

// passageOn is the live session, if any, that has a passage on for this
// item and profile. It is what the LEGACY progress route asks before
// accepting a write: the rule is the passage's, not the route's.
func (p *playSessions) passageOn(itemID int64, clientID string) (playSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, row := range p.rows {
		if row.ItemID == itemID && row.ClientID == clientID && row.Passage != nil {
			return *row, true
		}
	}
	return playSession{}, false
}

// marksInWork are the marks a new session inherits: the ones this profile
// was making in the same work a moment ago. Another work, another profile,
// or a longer while, and it starts clean.
func (p *playSessions) marksInWork(clientID, workKey string) marks {
	if workKey == "" {
		return marks{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	memo, ok := p.lastMarks[memoKey(clientID, workKey)]
	if !ok || time.Since(memo.at) > markMemoTTL {
		return marks{}
	}
	return memo.marks
}

// reapIdle drops the rows whose client has gone away. A transcode row goes
// when its ffmpeg session does — the stream reaper already decides that,
// from the segment fetches it alone sees. A direct play fetches its bytes
// from storage and never touches us again, so idleness here is the only
// signal there is: no action on the session within maxIdle and it is gone.
func (p *playSessions) reapIdle(maxIdle time.Duration, live func(string) bool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var dropped []string
	now := time.Now()
	for id, row := range p.rows {
		gone := now.Sub(row.touched) > maxIdle
		if row.Method == methodRead {
			// A reader that has gone quiet has been reading the page. There
			// is no stream behind a book to reap, so the grace is long
			// (readIdleTimeout, read.go) and the cost of it is one small row.
			gone = now.Sub(row.touched) > readIdleTimeout
		}
		if row.Pipeline != "" {
			gone = !live(row.Pipeline)
		}
		if gone {
			delete(p.rows, id)
			dropped = append(dropped, id)
		}
	}
	return dropped
}

func newSessionID() string {
	buf := make([]byte, 6)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

// ── starting one ────────────────────────────────────────────────────────

// presignedURL is the item's bytes, addressed so a device can fetch them
// directly. It is a field on the server rather than a bare call so a test
// can stand the play routes up without MinIO.
func (s *server) presignedURL(ctx context.Context, objectKey string) (string, error) {
	if s.presign != nil {
		return s.presign(ctx, objectKey)
	}
	u, err := s.s3.PresignedGetObject(ctx, s.bucket, objectKey, 6*time.Hour, url.Values{})
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// playRequest is one decided play, ready to be acted on.
type playRequest struct {
	item store.Item
	d    model.PlayDecision
	seek float64
	pass *sessionPassage
	in   decisionInput
}

// act carries the decision out — a presigned URL for a direct play, an
// ffmpeg session for a transcode — and registers the row either way.
func (s *server) act(r *http.Request, req playRequest) (playSession, *hyper.Problem) {
	row := playSession{
		ItemID: req.item.ID, ClientID: req.in.ClientID,
		Method: string(req.d.Method), Passage: req.pass,
		Caps: req.in.Capabilities, AudioTrack: req.in.AudioTrack, BurnSub: req.in.SubtitleBurn,
		StartedAt: time.Now(),
	}
	if ws, err := s.buildWorks(); err == nil {
		if wk := works.ByItem(ws)[req.item.ID]; wk != nil {
			row.WorkKey = wk.Key
		}
	}
	row.Marks = s.plays.marksInWork(row.ClientID, row.WorkKey)

	switch req.d.Method {
	case model.DirectPlay:
		u, err := s.presignedURL(r.Context(), req.item.ObjectKey)
		if err != nil {
			p := serverProblem(err)
			return playSession{}, &p
		}
		row.ID, row.URL = newSessionID(), u

	case model.Transcode:
		u, err := s.presignedURL(r.Context(), req.item.ObjectKey)
		if err != nil {
			p := serverProblem(err)
			return playSession{}, &p
		}
		// Seeking into a stream whose audio must be re-encoded needs an
		// output-side trim (see SeekAudioPrerollSeconds), which requires
		// decoded video too — upgrade a copy-video plan and say so.
		if req.seek > 0 && req.d.Target.AudioCodec != "" && req.d.Target.VideoCodec == "" && !req.d.Target.AudioOnly {
			req.d.Target.VideoCodec = s.policy.TranscodeVideoCodec
			req.d.Target.VideoBitrateBps = s.policy.TranscodeVideoBitrateBps
			req.d.Trace = append(req.d.Trace, model.TraceStep{
				Check: "seek_adjustment", Passed: true,
				Detail: fmt.Sprintf(
					"re-encoding video (was copy): seeking to %.0fs with re-encoded audio needs a decoded pre-roll trim",
					req.seek),
			})
		}
		sess, err := s.sessions.Create(u, *req.d.Target, req.seek)
		if err != nil {
			p := serverProblem(err)
			return playSession{}, &p
		}
		// Generous timeout: seeks with audio pre-roll must download and
		// decode ~10s of media first, and the storage box may be slow.
		if err := sess.WaitForPlaylist(45 * time.Second); err != nil {
			// Grab the ffmpeg log before Stop deletes the session dir, so
			// the caller sees why instead of a bare timeout.
			tail := readTail(filepath.Join("data/streams", sess.ID, "ffmpeg.log"), 500)
			s.sessions.Stop(sess.ID)
			if tail != "" {
				err = fmt.Errorf("%s; ffmpeg log tail: %s", err, tail)
			}
			p := serverProblem(err)
			return playSession{}, &p
		}
		// ABR sessions hand the client the master playlist; hls.js and cast
		// receivers both speak master playlists natively.
		row.ID, row.Pipeline = sess.ID, sess.ID
		row.URL = "/streams/" + sess.ID + "/" + sess.PlaylistName()
		if req.d.Target.VideoCodec != "" {
			row.Encoder = pipeline.ResolveEncoder(s.sessions.HW, req.d.Target.VideoCodec)
		}

	default: // deny
		detail := "the decision engine refused this file for these capabilities"
		if n := len(req.d.Trace); n > 0 {
			detail = req.d.Trace[n-1].Detail
		}
		p := hyper.Refuse(http.StatusForbidden, "playback-denied",
			"This file cannot be played on this device", detail).
			WithRemedy("play it on a device that reads the file as it is, or allow transcoding on the server",
				&hyper.Link{Href: itemHref(req.item.ID), Title: itemTitle(req.item)})
		return playSession{}, &p
	}

	row.Decision = req.d
	row.Seek = req.seek
	s.plays.put(row)
	return row, nil
}

// handlePlay is POST /api/items/{id}/play: decide, act, and answer the
// session document. The optional `passage` is the deep-link grammar in
// JSON, already resolved to item ids (the show form is step 2's route
// document's business, before play).
func (s *server) handlePlay(w http.ResponseWriter, r *http.Request) {
	item, in, d, ok := s.itemAndDecision(w, r)
	if !ok {
		return
	}
	// The shelves leave a title a kid profile may not see out; a play aimed
	// straight at one — a stale tab, a pasted link — is refused in words.
	if problem := s.refuseItem(playProfile(r, in.ClientID), *item); problem != nil {
		hyper.WriteProblem(w, *problem)
		return
	}
	pass := resolvePassage(in.Passage, item.ID)
	seek := in.SeekSeconds
	// Rule 1 — start there: a passage's `t` is where playback begins, and
	// the client never reads /api/progress for a passage session.
	if pass != nil && pass.T != nil && seek == 0 {
		seek = *pass.T
	}
	row, problem := s.act(r, playRequest{item: *item, d: *d, seek: seek, pass: pass, in: *in})
	if problem != nil {
		hyper.WriteProblem(w, *problem)
		return
	}
	s.writeSession(w, r, row, http.StatusOK)
}

// handleSessionNext is the run's own advance: the next member of the work
// started for the same device, carrying the passage forward with `t` and
// `end` stripped — they were the first item's. The answer is the NEW
// session's document.
func (s *server) handleSessionNext(w http.ResponseWriter, r *http.Request) {
	row, ok := s.session(w, r)
	if !ok {
		return
	}
	next, _, ok := s.sessionNeighbour(row)
	if !ok || next == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "no-next-member",
			"There is nothing after this one", "this item is the last member of its work").
			WithRemedy("open the library and pick something else",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	if next.MediaInfo == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "unprobed-item",
			"The next item cannot be played", unprobed(*next)).
			WithRemedy("probe it again from its own page",
				&hyper.Link{Href: itemHref(next.ID), Title: itemTitle(*next)}))
		return
	}
	// A new file: the track selections were the old one's ordinals and mean
	// nothing here, but the device's capabilities are still the device's.
	in := decisionInput{Capabilities: row.Caps, ClientID: row.ClientID}
	d := decision.DecideWith(*next.MediaInfo, in.Capabilities, s.policy, decision.Options{})
	pass := passageForNext(row.Passage, next.ID)
	newRow, problem := s.act(r, playRequest{item: *next, d: d, pass: pass, in: in})
	if problem != nil {
		hyper.WriteProblem(w, *problem)
		return
	}
	s.writeSession(w, r, newRow, http.StatusOK)
}

// passageForNext is the passage the NEXT member of a run plays under, which
// the grammar answers (passage.ForNext): `until` carries on, and `t` was the
// first item's. `end` is kept where the grammar's link form drops it — it
// belongs to the LAST member of the run, and this session knows which member
// it is about, where a link only knows the one it opens.
func passageForNext(p *sessionPassage, itemID int64) *sessionPassage {
	next := p.grammar().ForNext()
	if next == nil {
		return nil
	}
	out := &sessionPassage{Until: next.Until, End: p.End}
	out.EndsAt = endsAt(out, itemID)
	return out
}

// ── the document ────────────────────────────────────────────────────────

// session is the row this request is about, or a refusal.
func (s *server) session(w http.ResponseWriter, r *http.Request) (playSession, bool) {
	id := r.PathValue("id")
	row, ok := s.plays.get(id)
	if !ok {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-session",
			"No such session", fmt.Sprintf("session %q is not playing (it may have been stopped or reaped)", id)).
			WithRemedy("play the item again — a session lasts as long as the playback does",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return playSession{}, false
	}
	return row, true
}

// sessionNeighbour is the work's next member after this session's item, and
// the work itself: the order a run walks and marks are ordered by.
func (s *server) sessionNeighbour(row playSession) (next *store.Item, wk *works.Work, ok bool) {
	ws, err := s.buildWorks()
	if err != nil {
		return nil, nil, false
	}
	wk = works.ByItem(ws)[row.ItemID]
	if wk == nil {
		return nil, nil, true
	}
	_, next = works.Neighbours(wk, row.ItemID)
	return next, wk, true
}

func (s *server) handleSessionDoc(w http.ResponseWriter, r *http.Request) {
	row, ok := s.session(w, r)
	if !ok {
		return
	}
	s.writeSession(w, r, row, http.StatusOK)
}

func (s *server) writeSession(w http.ResponseWriter, r *http.Request, row playSession, status int) {
	item, err := s.library.GetItem(row.ItemID)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	if item == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-item",
			"The item this session plays is gone", fmt.Sprintf("the library no longer holds item %d", row.ItemID)).
			WithRemedy("open the library and pick something else",
				&hyper.Link{Href: "/api/library", Title: "Library"}))
		return
	}
	next, wk, _ := s.sessionNeighbour(row)
	// A book is read through a session too, and its document is the text
	// half of this one: no stream, no clock, locators for a place (read.go).
	if row.Method == methodRead {
		hyper.WriteDoc(w, status, s.readEnvelope(row, *item, wk))
		return
	}
	hyper.WriteDoc(w, status, s.sessionEnvelope(row, *item, next, wk))
}

// sessionEnvelope is one play as a document.
func (s *server) sessionEnvelope(row playSession, item store.Item, next *store.Item, wk *works.Work) *hyper.Envelope {
	base := "/api/sessions/" + row.ID
	doc := hyper.Doc(base, "session", itemTitle(item)).
		Field("id", row.ID).
		Field("item_id", row.ItemID)
	if row.ClientID != "" {
		doc.Field("profile", row.ClientID)
	}
	doc.Field("method", row.Method).
		Field("url", row.URL).
		// What the bytes at `url` ARE, in the words a media element and a
		// Cast receiver both take. The client was guessing "video/mp4" for
		// every direct play; the server probed the container and knows.
		Field("content_type", sessionContentType(row, item))
	if row.Seek > 0 {
		doc.Field("seek_seconds", row.Seek)
	}
	if row.Encoder != "" {
		doc.Field("video_encoder", row.Encoder)
	}
	doc.Field("started_at", row.StartedAt.UTC().Format(time.RFC3339)).
		Field("decision", row.Decision).
		// passage and marks are written even when empty: a client that reads
		// `passage: null` knows the answer, where a missing field only means
		// it is reading an older server.
		Field("passage", row.Passage).
		Field("marks", row.Marks).
		// The stretches worth jumping over, read off the file's own chapter
		// names (skipsIn): the client is TOLD where the titles and the
		// credits are rather than taught how to find them.
		Field("skips", skipsIn(item))

	doc.Link("item", itemHref(item.ID), itemTitle(item))
	// The picture this play is shown by — a Cast device draws it behind the
	// title, and it is the server that knows which route has one.
	if href := sessionArtwork(item); href != "" {
		doc.Link("artwork", href, "")
	}
	// `back` is where the end-of-the-passage panel goes when the person is
	// done: the item's own page, the same address the item relation names,
	// under the name the panel renders it by, so the screen needs no rule.
	doc.Link("back", itemHref(item.ID), itemTitle(item))
	if wk != nil {
		doc.Link("work", workHref(wk.Key), wk.Title)
	}
	if next != nil {
		doc.Link("next", itemHref(next.ID), itemTitle(*next))
	}

	doc.Action("progress", hyper.Action{
		Method: "POST", Href: base + "/progress",
		Input: sessionProgressInput(item), Label: "Save the place",
	})
	if row.Passage != nil {
		doc.Action("keep_watching", hyper.Action{
			Method: "POST", Href: base + "/keep_watching", Label: keepLabel(item),
		})
	} else {
		doc.Unavailable("keep_watching", "no passage is on — this is ordinary playback, and the place is being saved")
	}
	doc.Action("stop", hyper.Action{
		Method: "DELETE", Href: base, Label: "Stop",
	})
	if next != nil {
		doc.Action("next", hyper.Action{
			Method: "POST", Href: base + "/next",
			Input: map[string]string{"capabilities": "capabilities?"},
			Label: "▶ " + itemTitle(*next),
		})
	} else {
		doc.Unavailable("next", "this is the last member of its work")
	}
	for _, m := range []struct{ kind, label string }{{"in", "Mark in"}, {"out", "Mark out"}} {
		doc.Action("mark_"+m.kind, hyper.Action{
			Method: "POST", Href: base + "/mark/" + m.kind,
			Input: map[string]string{"seconds": "number", "clear": "boolean?"},
			Label: m.label,
		})
	}
	if row.Marks.In != nil {
		doc.Action("link", hyper.Action{
			Method: "GET", Href: base + "/link", Label: "Copy passage link",
		})
	} else {
		doc.Unavailable("link", "mark an in point first — a passage needs somewhere to start")
	}
	return doc
}

// ── the skips ───────────────────────────────────────────────────────────

// sessionSkip is one stretch of the file worth jumping over: the opening
// titles, or the closing credits. It is Netflix's button without the
// detection — a great many rips carry the chapter names that say where
// those stretches are, and a name is a fact rather than a guess.
type sessionSkip struct {
	Kind  string  `json:"kind"` // "intro" | "credits"
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Label string  `json:"label"` // what the button says, in the server's words
}

// skipLabel is how a skip of each kind is offered.
var skipLabel = map[string]string{"intro": "Skip intro", "credits": "Skip credits"}

// skipKind reads one chapter name. The opening words are tried first: an
// "Opening credits" chapter is the start of the film, not the end of it.
func skipKind(title string) string {
	t := strings.ToLower(title)
	for _, w := range []string{"intro", "opening", "titles"} {
		if strings.Contains(t, w) {
			return "intro"
		}
	}
	if strings.Contains(t, "credits") {
		return "credits"
	}
	return ""
}

// skipsIn is the derivation, and it neither probes nor guesses: a chapter
// whose NAME says what it is, running until the next chapter starts — or,
// for the last chapter, until the file ends, which is exactly what closing
// credits do.
//
// An opening is a stretch one jumps OVER, so it needs somewhere to land: an
// intro-named chapter with nothing after it is a file's last marker rather
// than an opening, and there is no skip in it.
func skipsIn(it store.Item) []sessionSkip {
	out := []sessionSkip{}
	if it.MediaInfo == nil {
		return out
	}
	chs := it.MediaInfo.Chapters
	for i, ch := range chs {
		kind := skipKind(ch.Title)
		if kind == "" {
			continue
		}
		last := i == len(chs)-1
		if kind == "intro" && last {
			continue
		}
		end := it.MediaInfo.DurationSeconds
		if !last {
			end = chs[i+1].StartSeconds
		}
		if end <= ch.StartSeconds {
			continue
		}
		out = append(out, sessionSkip{
			Kind: kind, Start: ch.StartSeconds, End: end, Label: skipLabel[kind]})
	}
	return out
}

// sessionArtwork is the picture that stands for this play: the item's own
// cover for audio and text, the episode still or the film's poster for
// video — and NOTHING when there is no picture, rather than a link to a
// route that would answer 404. The still comes first for video: a cast
// device draws it full-width behind the title, and a frame of the episode
// says more there than the show's poster does.
func sessionArtwork(it store.Item) string {
	base := itemHref(it.ID)
	switch it.MediaInfo.MediumOrVideo() {
	case model.MediumAudio, model.MediumText:
		return base + "/cover"
	}
	if it.Enrichment != nil && it.Enrichment.HasStill {
		return base + "/still"
	}
	if it.Enrichment != nil && it.Enrichment.HasPoster {
		return base + "/poster"
	}
	return ""
}

// sessionContentType is the media type of the bytes at `url`: the HLS
// playlist for a transcode, and for a direct play the container's own type.
// The container is what the probe read, so this is the server saying what
// it is handing over rather than every client guessing from the URL.
func sessionContentType(row playSession, it store.Item) string {
	if row.Method == string(model.Transcode) {
		return "application/x-mpegurl"
	}
	audio := it.MediaInfo.MediumOrVideo() == model.MediumAudio
	container := ""
	if it.MediaInfo != nil {
		container = strings.ToLower(it.MediaInfo.Container)
	}
	switch container {
	case "mp4", "m4v", "mov":
		if audio {
			return "audio/mp4"
		}
		return "video/mp4"
	case "m4a", "m4b":
		return "audio/mp4"
	case "mkv":
		return "video/x-matroska"
	case "webm":
		if audio {
			return "audio/webm"
		}
		return "video/webm"
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "avi":
		return "video/x-msvideo"
	case "ts":
		return "video/mp2t"
	case "epub":
		return "application/epub+zip"
	case "pdf":
		return "application/pdf"
	}
	if audio {
		return "audio/mp4"
	}
	return "video/mp4"
}

// sessionProgressInput is what a progress write on THIS session takes. The
// item and the profile are the session's own, so unlike /api/progress they
// are not asked for again; what is left is the unit the medium counts in.
func sessionProgressInput(it store.Item) map[string]string {
	if it.MediaInfo.MediumOrVideo() == model.MediumText {
		return map[string]string{
			"position_seconds": "number?", "locator": "string?",
			"fraction": "number?", "section": "number?", "page": "number?",
		}
	}
	return map[string]string{"position_seconds": "number"}
}

// keepLabel is the escape hatch in the words of the medium: one leaves a
// passage of a film by watching on, and a passage of an album by listening
// on.
func keepLabel(it store.Item) string {
	switch it.MediaInfo.MediumOrVideo() {
	case model.MediumAudio:
		return "Keep listening"
	case model.MediumText:
		return "Keep reading"
	}
	return "Keep watching"
}

// ── the actions ─────────────────────────────────────────────────────────

// handleSessionProgress is POST /api/sessions/{id}/progress — the place,
// written through the same store /api/progress writes to, and REFUSED while
// a passage is on. Rule 3: a scene replayed for a talk is not where the
// person is in the film.
func (s *server) handleSessionProgress(w http.ResponseWriter, r *http.Request) {
	row, ok := s.session(w, r)
	if !ok {
		return
	}
	if row.Passage != nil {
		hyper.WriteProblem(w, passageOnProblem(row))
		return
	}
	var in progressInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-body",
			"That is not a progress write", err.Error()).
			WithRemedy("send the fields this action's input names", nil))
		return
	}
	loc, err := placeOf(in)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-place",
			"That is not a place in this item", err.Error()).
			WithRemedy("send the fields this action's input names", nil))
		return
	}
	client := row.ClientID
	if client == "" {
		client = in.ClientID
	}
	if err := s.state.SetPlace(row.ItemID, client, in.Position, loc); err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	// A heartbeat every few seconds is answered as briefly as it is asked:
	// nothing about the session changed, and the client already has it.
	writeJSON(w, map[string]string{"status": "ok"})
}

// passageOnProblem is the refusal itself: what happened, and the one action
// that would make the write available.
func passageOnProblem(row playSession) hyper.Problem {
	// The way out is the medium's (escapeFrom, passage.go): a film is watched
	// on, a book read on, and the remedy names the action this session offers.
	out := escapeFrom(row)
	return hyper.Refuse(http.StatusConflict, "passage-on",
		"A passage is on, so the place is not being saved", out.detail).
		WithRemedy(out.remedy, &hyper.Link{Href: out.href, Title: out.label})
}

// handleKeepWatching is the escape hatch: the passage is over, ordinary
// playback carries on from here, and progress writes are accepted again.
func (s *server) handleKeepWatching(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(w, r); !ok {
		return
	}
	row, ok := s.plays.update(r.PathValue("id"), func(row *playSession) { row.Passage = nil })
	if !ok {
		return
	}
	s.writeSession(w, r, row, http.StatusOK)
}

// markInput is a mark_in / mark_out body: where the client's clock is, or
// `clear` to take that end of the passage back.
type markInput struct {
	Seconds float64 `json:"seconds"`
	Clear   bool    `json:"clear"`
}

// handleSessionMark sets one end of the passage being made. The position is
// the client's — it holds the clock — and everything done to it is the
// server's: the snap to a nearby chapter start, the swap when the two ends
// arrive the wrong way round, and a mark on a later member of the work
// becoming the run's `until`.
func (s *server) handleSessionMark(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind != "in" && kind != "out" {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-mark",
			"A passage has two ends", fmt.Sprintf("%q is neither of them", kind)).
			WithRemedy("mark `in` or `out`", nil))
		return
	}
	row, ok := s.session(w, r)
	if !ok {
		return
	}
	var in markInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-body",
			"That is not a mark", err.Error()).
			WithRemedy("send seconds, or clear: true", nil))
		return
	}
	item, err := s.library.GetItem(row.ItemID)
	if err != nil || item == nil {
		hyper.WriteProblem(w, serverProblem(fmt.Errorf("item %d: %v", row.ItemID, err)))
		return
	}
	_, wk, _ := s.sessionNeighbour(row)
	at := mark{ItemID: row.ItemID, Seconds: snapToChapter(
		markSeconds(in.Seconds), chapterStarts(*item), chapterSnapSeconds)}
	row, ok = s.plays.update(row.ID, func(row *playSession) {
		if in.Clear {
			row.Marks = clearMark(row.Marks, kind)
			return
		}
		row.Marks = setMark(row.Marks, kind, at, memberOrder(wk))
	})
	if !ok {
		return
	}
	s.plays.remember(row)
	s.writeSession(w, r, row, http.StatusOK)
}

func chapterStarts(it store.Item) []float64 {
	if it.MediaInfo == nil {
		return nil
	}
	out := make([]float64, 0, len(it.MediaInfo.Chapters))
	for _, ch := range it.MediaInfo.Chapters {
		out = append(out, ch.StartSeconds)
	}
	return out
}

// memberOrder is the work's members in playing order — what makes one mark
// later than another when the two are in different files.
func memberOrder(wk *works.Work) []int64 {
	if wk == nil {
		return nil
	}
	out := make([]int64, 0, len(wk.Items))
	for _, it := range wk.Items {
		out = append(out, it.ID)
	}
	return out
}

// handleSessionLink mints the marked passage: the link and the sentence
// that says it. Refused until an in point exists — the same reason the
// document gives under `unavailable`.
func (s *server) handleSessionLink(w http.ResponseWriter, r *http.Request) {
	row, ok := s.session(w, r)
	if !ok {
		return
	}
	if row.Marks.In == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "no-in-point",
			"There is no passage to link to yet", "a passage needs somewhere to start").
			WithRemedy("mark an in point at the position you want it to start",
				&hyper.Link{Href: "/api/sessions/" + row.ID + "/mark/in", Title: "Mark in"}))
		return
	}
	in, err := s.library.GetItem(row.Marks.In.ItemID)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	if in == nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusNotFound, "no-such-item",
			"The item this passage starts in is gone",
			fmt.Sprintf("the library no longer holds item %d", row.Marks.In.ItemID)).
			WithRemedy("mark the passage again", nil))
		return
	}
	var out *store.Item
	_, wk, _ := s.sessionNeighbour(row)
	if row.Marks.Out != nil && row.Marks.Out.ItemID != in.ID && wk != nil {
		// memberByID falls back to the work's first member; a mark that
		// names something the work does not hold is no out point at all.
		if m := memberByID(wk, row.Marks.Out.ItemID); m != nil && m.ID == row.Marks.Out.ItemID {
			out = m
		}
	}
	p := markedPassage(row.Marks)
	hyper.WriteDoc(w, http.StatusOK,
		passageDoc("/api/sessions/"+row.ID+"/link", s.absoluteBase(r), *in, out, wk, p))
}
