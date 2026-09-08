package main

// The transcription queue and the delivery, over the same fixture library
// every other document is read from. No whisper, no GPU and no MinIO: the
// queue is a read of the library, and a delivery is bytes posted at a route.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fixtureQueue is the fixture library with the storage presigner stubbed the
// way fixturePlayer stubs it: the queue hands out a URL, not bytes.
func fixtureQueue(t *testing.T) (*server, http.Handler) {
	t.Helper()
	srv, h := fixtureServer(t)
	srv.presign = func(_ context.Context, objectKey string) (string, error) {
		return "https://storage.test/" + objectKey + "?signed", nil
	}
	return srv, h
}

// queueIDs is the ids the queue is offering, in the order it offers them.
func queueIDs(t *testing.T, h http.Handler, target string) []int64 {
	t.Helper()
	w := get(t, h, target)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body)
	}
	var doc struct {
		Queue []struct {
			ID     int64 `json:"id"`
			Medium string
		} `json:"queue"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range doc.Queue {
		out = append(out, e.ID)
	}
	return out
}

// deliver posts a WebVTT body the way a worker does: the bytes themselves,
// and the etag on the query.
func deliver(t *testing.T, h http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "text/vtt")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const deliveredVTT = "WEBVTT\n\n" +
	"00:00:01.000 --> 00:00:03.500\nSomebody is playing a flugelhorn.\n\n" +
	"00:00:04.000 --> 00:00:06.000\nBadly.\n"

// The queue document as a worker reads it.
func TestTranscriptQueueGolden(t *testing.T) {
	_, h := fixtureQueue(t)
	w := get(t, h, "/api/transcripts")
	if w.Code != http.StatusOK {
		t.Fatalf("GET queue = %d: %s", w.Code, w.Body)
	}
	golden(t, "transcripts", w.Body.Bytes())
}

// The queue is needsTranscript and nothing else: no book, no file carrying
// its own subtitles, and nothing already heard at this etag.
func TestTranscriptQueueListsOnlyWhatNeedsHearing(t *testing.T) {
	_, h := fixtureQueue(t)
	got := queueIDs(t, h, "/api/transcripts?all=1")
	want := []int64{idDunePart1, idDunePart2, idAirbag, idParanoid, idDeleted, idYou, idTelex}
	if len(got) != len(want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue = %v, want %v (oldest id first)", got, want)
		}
	}
	// The three it must never offer, said one at a time: the film with two
	// subtitle tracks, the episode with a sidecar, and the episode already
	// transcribed at this etag.
	for _, id := range []int64{idFrozen, idBeach, idTheJob, idHillHouse, idFlatland} {
		for _, in := range got {
			if in == id {
				t.Errorf("queue offers item %d", id)
			}
		}
	}
}

// An entry handed out is somebody's work for an hour: the next read skips it,
// and a person may still see the whole queue with ?all=1.
func TestTranscriptQueueLeasesWhatItHandsOut(t *testing.T) {
	_, h := fixtureQueue(t)
	first := queueIDs(t, h, "/api/transcripts?limit=2")
	if len(first) != 2 {
		t.Fatalf("first read = %v, want two entries", first)
	}
	second := queueIDs(t, h, "/api/transcripts?limit=2")
	for _, id := range second {
		for _, was := range first {
			if id == was {
				t.Errorf("item %d was handed out twice", id)
			}
		}
	}
	all := queueIDs(t, h, "/api/transcripts?all=1&limit=2")
	if len(all) != 2 || all[0] != first[0] {
		t.Errorf("?all=1 = %v, want the leased entries back (from %v)", all, first)
	}
	// count is the whole backlog whatever the limit; leased is what is out.
	w := get(t, h, "/api/transcripts?limit=1")
	var doc struct {
		Count  int `json:"count"`
		Leased int `json:"leased"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Count != 7 {
		t.Errorf("count = %d, want 7", doc.Count)
	}
	if doc.Leased != 5 {
		t.Errorf("leased = %d, want 5", doc.Leased)
	}
}

// The delivery: the file on disk, the row, the cues behind the dialogue
// search, the track in the item's own document, and the item gone from the
// queue.
func TestTranscriptDelivery(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, h := fixtureQueue(t)

	w := deliver(t, h, "/api/items/7/transcript?etag=e7&language=en&model=/models/ggml-large-v3.bin", deliveredVTT)
	if w.Code != http.StatusOK {
		t.Fatalf("POST transcript = %d: %s", w.Code, w.Body)
	}
	var doc struct {
		Self     string `json:"self"`
		Kind     string `json:"kind"`
		ItemID   int64  `json:"item_id"`
		Cues     int    `json:"cues"`
		Language string `json:"language"`
		Model    string `json:"model"`
		Links    map[string]struct {
			Href string `json:"href"`
		} `json:"links"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Self != "/api/items/7" || doc.Kind != "transcript" || doc.ItemID != idDeleted {
		t.Errorf("answer = %+v", doc)
	}
	if doc.Cues != 2 || doc.Language != "en" || doc.Model != "ggml-large-v3.bin" {
		t.Errorf("answer = %+v", doc)
	}
	if doc.Links["item"].Href != "/api/items/7" || doc.Links["queue"].Href != "/api/transcripts" {
		t.Errorf("links = %+v", doc.Links)
	}

	if b, err := os.ReadFile(transcriptPath(idDeleted)); err != nil || string(b) != deliveredVTT {
		t.Errorf("the WebVTT on disk = %q, %v", b, err)
	}
	row, err := srv.library.Transcript(idDeleted)
	if err != nil || row == nil {
		t.Fatalf("Transcript row = %v, %v", row, err)
	}
	if row.ETag != "e7" || row.Language != "en" || row.Model != "ggml-large-v3.bin" {
		t.Errorf("row = %+v", row)
	}
	cues, _, err := srv.library.SearchCues("flugelhorn", idDeleted, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 1 {
		t.Fatalf("the dialogue search found %d lines, want 1", len(cues))
	}

	// The item's own document now lists it, addressed by the word.
	item := decode(t, get(t, h, "/api/items/7"))
	subs, _ := item["subtitles"].([]any)
	found := false
	for _, s := range subs {
		if m, ok := s.(map[string]any); ok && m["ordinal"] == transcriptOrdinal {
			found = true
			if m["href"] != "/api/items/7/subtitles/transcript.vtt" {
				t.Errorf("transcript href = %v", m["href"])
			}
		}
	}
	if !found {
		t.Errorf("the item document lists no transcript: %v", subs)
	}

	for _, id := range queueIDs(t, h, "/api/transcripts?all=1") {
		if id == idDeleted {
			t.Errorf("item %d is still in the queue after its transcript arrived", id)
		}
	}
}

// A delivery is refused in words, with somewhere to go.
func TestTranscriptDeliveryRefusals(t *testing.T) {
	t.Chdir(t.TempDir())
	_, h := fixtureQueue(t)
	for _, tc := range []struct {
		name, target, body string
		status             int
		typ                string
	}{
		{"a bad id", "/api/items/nope/transcript?etag=e7", deliveredVTT, http.StatusBadRequest, "bad-item-id"},
		{"no such item", "/api/items/9999/transcript?etag=e7", deliveredVTT, http.StatusNotFound, "no-such-item"},
		{"the file has subtitles of its own", "/api/items/4/transcript?etag=e4", deliveredVTT,
			http.StatusConflict, "has-subtitles"},
		{"no etag at all", "/api/items/7/transcript", deliveredVTT, http.StatusBadRequest, "missing-etag"},
		{"the file changed under the id", "/api/items/7/transcript?etag=e0", deliveredVTT,
			http.StatusConflict, "stale-transcript"},
		{"a body with no cues in it", "/api/items/7/transcript?etag=e7", "WEBVTT\n\nNOTE nothing here\n",
			http.StatusUnprocessableEntity, "empty-transcript"},
		{"a language that is not one", "/api/items/7/transcript?etag=e7&language=english", deliveredVTT,
			http.StatusBadRequest, "bad-language"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := deliver(t, h, tc.target, tc.body)
			if w.Code != tc.status {
				t.Fatalf("POST %s = %d: %s", tc.target, w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content-type = %q", ct)
			}
			var p struct {
				Type   string `json:"type"`
				Status int    `json:"status"`
				Remedy *struct {
					Text string `json:"text"`
				} `json:"remedy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.Type != tc.typ || p.Status != tc.status {
				t.Errorf("type=%q status=%d, want %q", p.Type, p.Status, tc.typ)
			}
			if p.Remedy == nil || p.Remedy.Text == "" {
				t.Errorf("%s refused without a remedy", tc.typ)
			}
		})
	}
	// Nothing above wrote a file: a refusal leaves the library as it was.
	if _, err := os.Stat(transcriptPath(idDeleted)); !os.IsNotExist(err) {
		t.Errorf("a refused delivery left %s behind (%v)", transcriptPath(idDeleted), err)
	}
}

// The etag may also ride a header, for a worker streaming a file at the
// route rather than composing a query.
func TestTranscriptDeliveryReadsTheHeader(t *testing.T) {
	t.Chdir(t.TempDir())
	_, h := fixtureQueue(t)
	r := httptest.NewRequest(http.MethodPost, "/api/items/7/transcript", strings.NewReader(deliveredVTT))
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-Transcript-ETag", "e7")
	r.Header.Set("X-Transcript-Language", "en")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST transcript = %d: %s", w.Code, w.Body)
	}
}
