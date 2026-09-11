package main

// A whole pass, in milliseconds: an httptest flickr hands out two items and
// then an empty queue, the exec seam writes the WebVTT a whisper would have
// written, and the test's sleep ends the worker when the queue runs dry. No
// ffmpeg, no whisper, no Keycloak.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"flickr/internal/bearer"
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

// device is what a Vulkan build prints when it loads the model — the lines
// the start-up probe exists to catch.
const device = "ggml_vulkan: found 1 Vulkan devices:\nVulkan0: Fake GPU (RADV) | uma: 0 | fp16: 1\n"

// seam is the exec seam of internal/pipeline: ffmpeg writes a wav, whisper
// writes the cues at <prefix>.vtt and says what it heard. The start-up probe
// comes through here too, over the silence the worker generated itself.
func seam(_ context.Context, name string, args []string) ([]byte, error) {
	if name == "ffmpeg" {
		return nil, os.WriteFile(args[len(args)-1], []byte("RIFF"), 0o644)
	}
	prefix, wav := "", ""
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "-of":
			prefix = args[i+1]
		case "-f":
			wav = args[i+1]
		}
	}
	vtt := "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nHe took the job in New York.\n"
	if err := os.WriteFile(prefix+".vtt", []byte(vtt), 0o644); err != nil {
		return nil, err
	}
	if strings.HasSuffix(wav, "silence.wav") {
		return []byte(device), nil // the probe: the card, and nothing heard
	}
	return []byte(device + "auto-detected language: en (p = 0.98)\n"), nil
}

// pass runs one worker over the fake flickr and returns when the queue is
// empty: the test's sleep cancels the context instead of waiting. What the
// worker logged comes back with it — the start-up probe answers in the log
// and nowhere else.
func pass(t *testing.T, f *fakeFlickr) (*httptest.Server, []time.Duration, string) {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config{FlickrURL: srv.URL, WorkDir: t.TempDir(), IdleSleep: 5 * time.Minute}
	var slept []time.Duration
	now := time.Unix(0, 0)
	d := deps{
		HTTP: srv.Client(),
		Tokens: &bearer.ClientCredentials{
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
	return srv, slept, logs.String()
}

func TestRunDeliversEveryItem(t *testing.T) {
	f := &fakeFlickr{t: t, entries: []queueItem{entry(12, "Heat"), entry(13, "The Insider")}}
	_, slept, logs := pass(t, f)

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
	// The card whisper loaded the model onto is said once, at start-up, and
	// not again while it stays the same card.
	if !strings.Contains(logs, "whisper backend: ") || !strings.Contains(logs, "Fake GPU") {
		t.Errorf("the log never named the GPU whisper found:\n%s", logs)
	}
	if n := strings.Count(logs, "whisper backend: "); n != 1 {
		t.Errorf("said the backend %d times over two items, want once", n)
	}
}

// The probe writes its own audio: half a second of silence, in the format
// whisper reads, with the 44-byte header ffmpeg would have written.
func TestSilentWAV(t *testing.T) {
	b := silentWAV()
	data := 16000 // 0.5 s of 16 kHz mono at 2 bytes a sample
	if len(b) != 44+data {
		t.Fatalf("wav is %d bytes, want %d", len(b), 44+data)
	}
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"RIFF", string(b[0:4]), "RIFF"},
		{"WAVE", string(b[8:12]), "WAVE"},
		{"fmt ", string(b[12:16]), "fmt "},
		{"data", string(b[36:40]), "data"},
	} {
		if c.got != c.want {
			t.Errorf("%s field = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"riff size", binary.LittleEndian.Uint32(b[4:8]), uint32(36 + data)},
		{"fmt size", binary.LittleEndian.Uint32(b[16:20]), 16},
		{"sample rate", binary.LittleEndian.Uint32(b[24:28]), 16000},
		{"byte rate", binary.LittleEndian.Uint32(b[28:32]), 32000},
		{"data size", binary.LittleEndian.Uint32(b[40:44]), uint32(data)},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		got  uint16
		want uint16
	}{
		{"format", binary.LittleEndian.Uint16(b[20:22]), 1}, // uncompressed PCM
		{"channels", binary.LittleEndian.Uint16(b[22:24]), 1},
		{"block align", binary.LittleEndian.Uint16(b[32:34]), 2},
		{"bits a sample", binary.LittleEndian.Uint16(b[34:36]), 16},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	// Silence, so whisper hears nothing and spends no time on it.
	for i, v := range b[44:] {
		if v != 0 {
			t.Fatalf("sample byte %d = %d, want silence", i, v)
		}
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
