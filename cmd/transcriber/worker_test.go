package main

// A whole pass, in milliseconds: an httptest flickr hands out two items and
// then an empty queue, the exec seam writes the WebVTT a whisper would have
// written, and the test's sleep ends the worker when the queue runs dry. No
// ffmpeg, no whisper, no Keycloak.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"flickr/internal/pipeline"
)

// delivery is one POST as the fake flickr saw it.
type delivery struct {
	path        string
	auth        string
	contentType string
	query       map[string]string
	body        string
}

// fakeFlickr is the root, the queue and the two delivery addresses. The first
// read of the queue hands out the entries; every read after it is empty,
// which is what ends the pass.
type fakeFlickr struct {
	t          *testing.T
	entries    []queueItem
	stale      map[string]bool // path -> answer 409 rather than 200
	mu         sync.Mutex
	reads      int
	deliveries []delivery
	tokens     int
}

func (f *fakeFlickr) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/house/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" {
			f.t.Errorf("token request was %v (%v)", r.Form, err)
		}
		if r.Form.Get("client_id") != "transcriber" || r.Form.Get("client_secret") != "sssh" {
			f.t.Errorf("token request did not carry the client credentials: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token": "gpu-token", "expires_in": 300}`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"self": "/api/", "kind": "root", "links": {"transcripts": {"href": "/api/transcripts"}}}`)
	})
	mux.HandleFunc("/api/transcripts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		first := f.reads == 0
		f.reads++
		f.mu.Unlock()
		doc := queueDoc{Queue: []queueItem{}, Count: 0, Leased: 1}
		if first {
			doc.Queue, doc.Count = f.entries, len(f.entries)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/api/items/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		f.mu.Lock()
		f.deliveries = append(f.deliveries, delivery{
			path: r.URL.Path, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), query: q, body: string(body),
		})
		stale := f.stale[r.URL.Path]
		f.mu.Unlock()
		if r.Method != http.MethodPost {
			f.t.Errorf("delivery used %s, want POST", r.Method)
		}
		if stale {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"title": "stale-transcript", "detail": "the file changed since it was handed out"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func entry(id int64, title string) queueItem {
	return queueItem{
		ID: id, ObjectKey: fmt.Sprintf("Movies/%d.mkv", id), ETag: fmt.Sprintf("etag-%d", id),
		Medium: "video", DurationSeconds: 5400, Title: title,
		URL: fmt.Sprintf("http://minio.example.test/bkt/Movies/%d.mkv?sig=x", id),
		Actions: map[string]action{"transcript": {
			Method: "POST", Href: fmt.Sprintf("/api/items/%d/transcript", id),
		}},
	}
}

// seam is the exec seam of internal/pipeline: ffmpeg writes a wav, whisper
// writes the cues at <prefix>.vtt and says what it heard.
func seam(_ context.Context, name string, args []string) ([]byte, error) {
	if name == "ffmpeg" {
		return nil, os.WriteFile(args[len(args)-1], []byte("RIFF"), 0o644)
	}
	prefix := ""
	for i, a := range args {
		if a == "-of" && i+1 < len(args) {
			prefix = args[i+1]
		}
	}
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nHe took the job in New York.\n"
	if err := os.WriteFile(prefix+".vtt", []byte(vtt), 0o644); err != nil {
		return nil, err
	}
	return []byte("auto-detected language: en (p = 0.98)\n"), nil
}

// pass runs one worker over the fake flickr and returns when the queue is
// empty: the test's sleep cancels the context instead of waiting.
func pass(t *testing.T, f *fakeFlickr) (*httptest.Server, []time.Duration) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config{FlickrURL: srv.URL, WorkDir: t.TempDir(), IdleSleep: 5 * time.Minute}
	var slept []time.Duration
	now := time.Unix(0, 0)
	d := deps{
		HTTP: srv.Client(),
		Tokens: &clientCredentials{
			HTTP: srv.Client(), TokenURL: srv.URL + "/realms/house/protocol/openid-connect/token",
			ID: "transcriber", Secret: "sssh",
		},
		Trans: &pipeline.Transcriber{Bin: "whisper-cli", Model: "/models/ggml-large-v3.bin", Run: seam},
		// An empty queue is the end of the test: the sleep it would take
		// cancels the run instead.
		Sleep: func(_ context.Context, dur time.Duration) { slept = append(slept, dur); cancel() },
		Now:   func() time.Time { now = now.Add(90 * time.Second); return now },
	}
	if err := run(ctx, cfg, d); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Nothing is left behind: the cues live at flickr now.
	entries, err := os.ReadDir(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the work dir kept %d directories, want none", len(entries))
	}
	return srv, slept
}

func TestRunDeliversEveryItem(t *testing.T) {
	f := &fakeFlickr{t: t, entries: []queueItem{entry(12, "Heat"), entry(13, "The Insider")}}
	_, slept := pass(t, f)

	if len(f.deliveries) != 2 {
		t.Fatalf("%d deliveries, want 2: %+v", len(f.deliveries), f.deliveries)
	}
	for i, want := range []struct{ path, etag string }{
		{"/api/items/12/transcript", "etag-12"},
		{"/api/items/13/transcript", "etag-13"},
	} {
		got := f.deliveries[i]
		if got.path != want.path {
			t.Errorf("delivery %d went to %s, want %s", i, got.path, want.path)
		}
		if got.query["etag"] != want.etag {
			t.Errorf("delivery %d carried etag %q, want %q", i, got.query["etag"], want.etag)
		}
		if got.query["language"] != "en" {
			t.Errorf("delivery %d carried language %q, want the detected en", i, got.query["language"])
		}
		if got.query["model"] != "ggml-large-v3.bin" {
			t.Errorf("delivery %d carried model %q, want the model file's base name", i, got.query["model"])
		}
		if got.contentType != "text/vtt" {
			t.Errorf("delivery %d was %q, want text/vtt", i, got.contentType)
		}
		if got.auth != "Bearer gpu-token" {
			t.Errorf("delivery %d carried %q, want the bearer from Keycloak", i, got.auth)
		}
		if !strings.HasPrefix(got.body, "WEBVTT") || !strings.Contains(got.body, "New York") {
			t.Errorf("delivery %d body = %q", i, got.body)
		}
	}
	// One token for the whole pass: it is good for five minutes and the
	// worker holds it rather than asking per request.
	if f.tokens != 1 {
		t.Errorf("asked Keycloak %d times, want once", f.tokens)
	}
	// An empty queue ends the pass with exactly one idle sleep.
	if len(slept) != 1 || slept[0] != 5*time.Minute {
		t.Errorf("slept %v, want one IDLE_SLEEP", slept)
	}
}

// A file that changed while it was being transcribed is refused, and that is
// this item's business only: the next one still gets delivered.
func TestRunMovesOnPastAStaleFile(t *testing.T) {
	f := &fakeFlickr{
		t:       t,
		entries: []queueItem{entry(12, "Heat"), entry(13, "The Insider")},
		stale:   map[string]bool{"/api/items/12/transcript": true},
	}
	pass(t, f)

	if len(f.deliveries) != 2 {
		t.Fatalf("%d deliveries, want both attempted: %+v", len(f.deliveries), f.deliveries)
	}
	if f.deliveries[1].path != "/api/items/13/transcript" {
		t.Errorf("the item after the stale one went to %s", f.deliveries[1].path)
	}
}

// The three shapes of the gate: nothing, a pasted bearer, and a Keycloak
// client that holds its token until it is nearly out.
func TestTokenSources(t *testing.T) {
	ctx := context.Background()
	if tok, err := (noToken{}).token(ctx); tok != "" || err != nil {
		t.Errorf("noToken = %q, %v; want no header at all", tok, err)
	}
	if tok, err := staticToken("pasted").token(ctx); tok != "pasted" || err != nil {
		t.Errorf("staticToken = %q, %v", tok, err)
	}

	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		fmt.Fprint(w, `{"access_token": "t", "expires_in": 300}`)
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	cc := &clientCredentials{
		HTTP: srv.Client(), TokenURL: srv.URL, ID: "transcriber", Secret: "sssh",
		Now: func() time.Time { return now },
	}
	for i := 0; i < 3; i++ {
		if tok, err := cc.token(ctx); tok != "t" || err != nil {
			t.Fatalf("token = %q, %v", tok, err)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times for a token good for five minutes, want once", asked)
	}
	// A minute before it expires the worker asks again, rather than finding
	// out at the end of an hour of transcription.
	now = now.Add(4*time.Minute + 30*time.Second)
	if _, err := cc.token(ctx); err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("asked %d times, want a refresh a minute before expiry", asked)
	}
}
