package main

// Transcription as work handed out: the queue a worker elsewhere reads, and
// the delivery it posts back.
//
// The local whisper stage (main.go) hears one file at a time on this box's
// CPU, which is hours per film. A machine with a GPU can hear the same file
// in minutes, and it is never this machine — so flickr says WHICH files
// still have nothing to read and takes the WebVTT back, while it keeps
// owning the library, the transcript rows and the dialogue index. Nothing
// about the local stage changes: a box with no worker still transcribes
// itself, and the rule for what needs hearing (needsTranscript) is the one
// both halves read.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"flickr/internal/hyper"
	"flickr/internal/pipeline"
	"flickr/internal/store"
)

// transcriptsHref is the queue's own address, named on the root so a worker
// finds it the way every other client finds everything: by following a link.
const transcriptsHref = "/api/transcripts"

const (
	// transcriptLeaseFor is how long an entry handed out is skipped by the
	// next read. Long enough that a slow file is not given away twice, short
	// enough that a worker that died does not hold a film for a day.
	transcriptLeaseFor = time.Hour

	// maxTranscriptBytes is the largest WebVTT a delivery may carry. An
	// eight-hour audiobook's cues are a couple of megabytes; 32 MiB is a
	// mistake, not a transcript.
	maxTranscriptBytes = 32 << 20

	transcriptQueueDefault = 20
	transcriptQueueMax     = 100
)

// transcriptLeases is the work handed out: item id → when a worker was given
// it. It is in memory on purpose — a restart forgets every lease, and the
// worst a forgotten lease costs is one file heard twice.
type transcriptLeases struct {
	mu  sync.Mutex
	out map[int64]time.Time
}

// held says whether this item is somebody else's work right now.
func (l *transcriptLeases) held(id int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.out[id]
	return ok && now.Sub(at) < transcriptLeaseFor
}

// hand records that the entry has just been given out.
func (l *transcriptLeases) hand(id int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		l.out = map[int64]time.Time{}
	}
	l.out[id] = now
}

// drop forgets one lease: the work came back, or it never will.
func (l *transcriptLeases) drop(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.out, id)
}

// count is how many leases are still live, expired ones swept as it counts.
func (l *transcriptLeases) count(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, at := range l.out {
		if now.Sub(at) >= transcriptLeaseFor {
			delete(l.out, id)
		}
	}
	return len(l.out)
}

// queueEntry is one file waiting to be heard, said the way a worker needs it:
// what to fetch — a presigned URL, and the object key for a worker on the LAN
// that would rather read MinIO itself — what the file is, and the one write
// it may make when it is done.
type queueEntry struct {
	ID              int64                   `json:"id"`
	ObjectKey       string                  `json:"object_key"`
	ETag            string                  `json:"etag"`
	Medium          string                  `json:"medium"`
	DurationSeconds float64                 `json:"duration_seconds,omitempty"`
	Title           string                  `json:"title"`
	URL             string                  `json:"url,omitempty"`
	Links           map[string]hyper.Link   `json:"links,omitempty"`
	Actions         map[string]hyper.Action `json:"actions,omitempty"`
}

// transcriptHref is where a worker posts what it heard.
func transcriptHref(id int64) string { return itemHref(id) + "/transcript" }

// handleTranscriptQueue is GET /api/transcripts — the files with nothing to
// read, oldest id first. Reading it HANDS the entries out: they are skipped
// by the next read for an hour, so two workers do not hear the same film.
// `?all=1` reads the queue without taking any of it, which is how a person
// looks at what is left.
func (s *server) handleTranscriptQueue(w http.ResponseWriter, r *http.Request) {
	limit := transcriptQueueDefault
	if n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); err == nil && n > 0 {
		limit = min(n, transcriptQueueMax)
	}
	all := r.URL.Query().Get("all") == "1"

	items, err := s.library.ListItems()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	have, err := s.library.Transcripts()
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	// ListItems answers in object-key order; the queue is oldest id first, so
	// the file that has waited longest is the one a worker is given.
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	now := time.Now()
	count := 0
	entries := []queueEntry{}
	for _, it := range items {
		var got *store.Transcript
		if t, ok := have[it.ID]; ok {
			got = &t
		}
		if !needsTranscript(it, got) {
			continue
		}
		count++
		if len(entries) >= limit {
			continue // still counted: `count` is the whole of the backlog
		}
		if !all && s.leases.held(it.ID, now) {
			continue
		}
		entries = append(entries, s.queueEntry(r, it))
		if !all {
			s.leases.hand(it.ID, now)
		}
	}

	hyper.WriteDoc(w, http.StatusOK, hyper.Doc(transcriptsHref, "transcripts", "Transcription queue").
		Field("queue", entries).
		Field("count", count).
		Field("leased", s.leases.count(now)).
		Link("root", "/api/", "flickr"))
}

func (s *server) queueEntry(r *http.Request, it store.Item) queueEntry {
	e := queueEntry{
		ID: it.ID, ObjectKey: it.ObjectKey, ETag: it.ETag,
		Medium: it.MediaInfo.MediumOrVideo(), Title: itemTitle(it),
		Links: map[string]hyper.Link{"item": {Href: itemHref(it.ID), Title: itemTitle(it)}},
		Actions: map[string]hyper.Action{"transcript": {
			Method: "POST", Href: transcriptHref(it.ID),
			Input: map[string]string{"etag": it.ETag, "language": "string?", "model": "string?"},
			Label: "Deliver the transcript",
		}},
	}
	if it.MediaInfo != nil {
		e.DurationSeconds = it.MediaInfo.DurationSeconds
	}
	// The bytes, addressed for a worker that cannot reach the bucket. A box
	// with no storage client signs nothing and the entry still stands: the
	// object key is the other way in.
	if s.presign != nil || s.presigner != nil {
		if u, err := s.presignedURL(r.Context(), it.ObjectKey); err == nil {
			e.URL = u
		} else {
			log.Printf("transcripts: presign item %d (%s): %v", it.ID, it.ObjectKey, err)
		}
	}
	return e
}

// transcriptLanguage is what a language tag may be: two or three letters,
// the shape whisper reports and the shape a WebVTT track is labelled with.
var transcriptLanguage = regexp.MustCompile(`^[A-Za-z]{2,3}$`)

// handleDeliverTranscript is POST /api/items/{id}/transcript — the queue's
// other half. The body is the WebVTT itself (text/vtt, or
// application/octet-stream for a worker that would rather not name it); the
// etag it was handed rides the query or an X-Transcript- header.
//
// The expected caller is a BEARER viewer: a worker's token, audience one of
// OIDC_DELEGATE_CLIENTS, read by the same gate every other write goes
// through (auth.go). There is nothing here for the gate to learn.
func (s *server) handleDeliverTranscript(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-item-id",
			"That is not an item id", fmt.Sprintf("%q is not a number", raw)).
			WithRemedy("post to the href the queue entry carries",
				&hyper.Link{Href: transcriptsHref, Title: "Transcription queue"}))
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
			WithRemedy("read the queue again; a scan may have dropped the file",
				&hyper.Link{Href: transcriptsHref, Title: "Transcription queue"}))
		return
	}
	// The author's own words beat a machine's hearing of them — the same rule
	// needsTranscript keeps, said here for a worker that heard the file
	// anyway.
	if item.MediaInfo != nil && len(item.MediaInfo.Subtitles) > 0 {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "has-subtitles",
			"That file has subtitles of its own",
			fmt.Sprintf("item %d carries %d subtitle track(s) the file itself came with",
				id, len(item.MediaInfo.Subtitles))).
			WithRemedy("nothing: a file with its own subtitles is not transcribed",
				&hyper.Link{Href: itemHref(id), Title: itemTitle(*item)}))
		return
	}
	etag := transcriptParam(r, "etag")
	if etag == "" {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "missing-etag",
			"That delivery says nothing about which file it heard",
			"etag is required: send back the one the queue entry carried").
			WithRemedy("read the queue again and send its etag with the transcript",
				&hyper.Link{Href: transcriptsHref, Title: "Transcription queue"}))
		return
	}
	if etag != item.ETag {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusConflict, "stale-transcript",
			"That transcript is of a file that has changed",
			fmt.Sprintf("item %d was heard as %q and the file under that id is now %q",
				id, etag, item.ETag)).
			WithRemedy("Read the queue again.",
				&hyper.Link{Href: transcriptsHref, Title: "Transcription queue"}))
		return
	}
	lang := transcriptParam(r, "language")
	if lang != "" && !transcriptLanguage.MatchString(lang) {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusBadRequest, "bad-language",
			"That is not a language tag", fmt.Sprintf("%q is not two or three letters", lang)).
			WithRemedy("send the tag whisper reported, or leave language out", nil))
		return
	}
	// The model is a name for the log and the row, not a path: a worker that
	// spells its whole model directory is recorded by the file at the end.
	whisperModel := transcriptParam(r, "model")
	if i := strings.LastIndexAny(whisperModel, `/\`); i >= 0 {
		whisperModel = whisperModel[i+1:]
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			hyper.WriteProblem(w, hyper.Refuse(http.StatusRequestEntityTooLarge, "transcript-too-large",
				"That transcript is too large",
				fmt.Sprintf("a delivery may carry at most %d bytes of WebVTT", maxTranscriptBytes)).
				WithRemedy("post the cues alone; a transcript this size is a mistake", nil))
			return
		}
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	// ParseVTT is the only judge of the body: a delivery that says nothing is
	// not a transcript, whatever content type it arrived under.
	cues := pipeline.ParseVTT(bytes.NewReader(body))
	if len(cues) == 0 {
		hyper.WriteProblem(w, hyper.Refuse(http.StatusUnprocessableEntity, "empty-transcript",
			"That body holds no cues",
			fmt.Sprintf("%d bytes parsed as WebVTT and yielded no lines", len(body))).
			WithRemedy("post the WebVTT whisper wrote, timing lines and all", nil))
		return
	}

	// The file first, the row second, exactly as the local stage does it: a
	// row always means a servable track.
	if err := writeFileAtomic(transcriptPath(id), bytes.NewReader(body)); err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	if err := s.recordTranscript(id, item.ETag, lang, whisperModel); err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	s.leases.drop(id)

	by := "an unnamed caller"
	if v, ok := viewerFrom(r.Context()); ok && v.Subject != "" {
		by = v.Subject
	}
	log.Printf("transcripts: item %d (%s) delivered by %s, %d cues", id, item.ObjectKey, by, len(cues))

	hyper.WriteDoc(w, http.StatusOK, hyper.Doc(itemHref(id), "transcript", "").
		Field("item_id", id).
		Field("cues", len(cues)).
		Field("language", lang).
		Field("model", whisperModel).
		Link("item", itemHref(id), itemTitle(*item)).
		Link("queue", transcriptsHref, "Transcription queue"))
}

// transcriptParam reads one of the delivery's three parameters from the query
// or from the header a worker streaming a file may find easier to set
// (X-Transcript-ETag, -Language, -Model).
func transcriptParam(r *http.Request, name string) string {
	if v := strings.TrimSpace(r.URL.Query().Get(name)); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Transcript-" + name))
}
