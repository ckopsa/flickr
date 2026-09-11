package main

// The pass: read the queue flickr hands out, transcribe one item at a time,
// deliver the WebVTT back, forget it. Everything that touches the world
// arrives in deps — the HTTP client, the transcriber's exec seam, the clock,
// the sleep — so a test drives a whole pass in milliseconds without an
// ffmpeg, a whisper or a flickr.
//
// The worker composes exactly one address, /api/, and follows a relation for
// every other: the root's links.transcripts is the queue, and an entry's
// actions.transcript is where its cues go. Same rule as the browser's.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"flickr/internal/bearer"
	"flickr/internal/pipeline"
)

// backoffStart and backoffMax bound the retry when flickr cannot be reached:
// a worker on a flaky link should not hammer the door, and should not give up
// on it either.
const (
	backoffStart = time.Minute
	backoffMax   = 15 * time.Minute
)

// deps is the world. A nil Presign means the audio is read from the URL the
// queue signed; a nil Stopping channel never fires, which is what a test wants.
type deps struct {
	HTTP     *http.Client
	Tokens   bearer.Source
	Trans    *pipeline.Transcriber
	Presign  func(ctx context.Context, objectKey string) (string, error)
	Sleep    func(ctx context.Context, d time.Duration)
	Now      func() time.Time
	Stopping <-chan struct{}
}

// link and action are the two pieces of a document this worker reads: an
// address, and an address with a method on it.
type link struct {
	Href string `json:"href"`
}

type action struct {
	Method string `json:"method"`
	Href   string `json:"href"`
}

type rootDoc struct {
	Links map[string]link `json:"links"`
}

type queueDoc struct {
	Queue  []queueItem `json:"queue"`
	Count  int         `json:"count"`
	Leased int         `json:"leased"`
}

type queueItem struct {
	ID              int64             `json:"id"`
	ObjectKey       string            `json:"object_key"`
	ETag            string            `json:"etag"`
	Medium          string            `json:"medium"`
	DurationSeconds float64           `json:"duration_seconds"`
	Title           string            `json:"title"`
	URL             string            `json:"url"`
	Actions         map[string]action `json:"actions"`
}

// run is the loop. It returns when the context is cancelled or a stop signal
// has arrived; nothing else ends it, because a worker with no work is a
// worker that sleeps and asks again.
func run(ctx context.Context, cfg config, d deps) error {
	// A stop signal ends a sleep as well as the pass: an idle worker exits
	// now rather than five minutes from now.
	wait := ctx
	if d.Stopping != nil {
		var cancel context.CancelFunc
		wait, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-d.Stopping:
			case <-wait.Done():
			}
			cancel()
		}()
	}

	// Once, before any work: which card whisper loads the model onto. After
	// this it is only said again when it CHANGES, so a steady worker stays
	// one line an item.
	backend := probeBackend(ctx, cfg, d)

	backoff := time.Duration(0)
	for {
		if stopping(ctx, d.Stopping) {
			return nil
		}
		q, err := d.readQueue(ctx, cfg.FlickrURL)
		if err != nil {
			backoff = nextBackoff(backoff)
			log.Printf("transcriber: reading the queue: %v (asking again in %s)", err, backoff)
			d.Sleep(wait, backoff)
			continue
		}
		backoff = 0
		if len(q.Queue) == 0 {
			log.Printf("transcriber: nothing to transcribe, asking again in %s", cfg.IdleSleep)
			d.Sleep(wait, cfg.IdleSleep)
			continue
		}
		log.Printf("transcriber: %d handed out, %d waiting, %d leased elsewhere", len(q.Queue), q.Count, q.Leased)
		// The queue leases what it hands out for an hour: this list is the
		// pass, and re-reading it mid-pass would only take out more leases.
		for _, it := range q.Queue {
			if stopping(ctx, d.Stopping) {
				return nil
			}
			d.do(ctx, cfg, it)
			if b := pipeline.WhisperBackend(d.Trans.LastOutput); b != "" && b != backend {
				log.Printf("whisper backend: %s", b)
				backend = b
			}
		}
	}
}

// nextBackoff doubles from a minute and stops at a quarter of an hour.
func nextBackoff(d time.Duration) time.Duration {
	if d == 0 {
		return backoffStart
	}
	if d *= 2; d > backoffMax {
		return backoffMax
	}
	return d
}

// stopping reports whether this worker should stop between items: a cancelled
// context, or the signal that says finish what you started and go.
func stopping(ctx context.Context, stop <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-stop:
		return true
	default:
		return false
	}
}

// do transcribes one item and delivers it. Every failure is this item's
// failure: it is logged and the pass moves on, because the next item is
// hours of perfectly good work.
func (d deps) do(ctx context.Context, cfg config, it queueItem) {
	dir := filepath.Join(cfg.WorkDir, strconv.FormatInt(it.ID, 10))
	defer os.RemoveAll(dir) // the cues live at flickr now, not here
	dest := filepath.Join(dir, "transcript.vtt")

	src, err := d.audio(ctx, it)
	if err != nil {
		log.Printf("transcriber: item %d (%s): %v", it.ID, it.ObjectKey, err)
		return
	}
	started := d.Now()
	lang, err := d.Trans.Transcribe(ctx, src, dest)
	if err != nil {
		log.Printf("transcriber: item %d (%s): %v", it.ID, it.ObjectKey, err)
		return
	}
	wall := d.Now().Sub(started)
	if err := d.deliver(ctx, cfg, it, dest, lang); err != nil {
		log.Printf("transcriber: item %d (%s): %v", it.ID, it.ObjectKey, err)
		return
	}
	log.Printf("transcriber: item %d %q in %s (%s)", it.ID, it.Title, wall.Round(time.Second), realtime(it.DurationSeconds, wall))
}

// realtime is how many seconds of film went by for every second spent, which
// is the number that says whether this box is worth transcribing on.
func realtime(durationSeconds float64, wall time.Duration) string {
	if durationSeconds <= 0 || wall <= 0 {
		return "realtime unknown"
	}
	return fmt.Sprintf("%.1fx realtime", durationSeconds/wall.Seconds())
}

// audio is where this worker reads the sound from: its own presigned URL
// against the LAN endpoint when it has the bucket's keys, and otherwise the
// one the queue signed for the public host.
func (d deps) audio(ctx context.Context, it queueItem) (string, error) {
	if d.Presign == nil {
		if it.URL == "" {
			return "", fmt.Errorf("the queue signed no url for %s", it.ObjectKey)
		}
		return it.URL, nil
	}
	return d.Presign(ctx, it.ObjectKey)
}

// readQueue follows the root's links.transcripts. /api/ is the one address
// the worker knows; the queue's own is the server's to move.
func (d deps) readQueue(ctx context.Context, base string) (*queueDoc, error) {
	rootURL, err := resolve(base, "/api/")
	if err != nil {
		return nil, err
	}
	var root rootDoc
	if err := d.get(ctx, rootURL, &root); err != nil {
		return nil, err
	}
	rel, ok := root.Links["transcripts"]
	if !ok || rel.Href == "" {
		return nil, fmt.Errorf("%s offers no transcripts queue (transcription is off there)", rootURL)
	}
	queueURL, err := resolve(base, rel.Href)
	if err != nil {
		return nil, err
	}
	var q queueDoc
	if err := d.get(ctx, queueURL, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// deliver POSTs the finished WebVTT to the item's own transcript action. The
// etag is what makes the delivery safe: a file that changed while we were
// listening is refused with a 409, and that transcript is simply dropped.
func (d deps) deliver(ctx context.Context, cfg config, it queueItem, path, language string) error {
	act, ok := it.Actions["transcript"]
	if !ok || act.Href == "" {
		return fmt.Errorf("the queue entry offers no transcript action")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	raw, err := resolve(cfg.FlickrURL, act.Href)
	if err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("etag", it.ETag)
	if language != "" {
		q.Set("language", language)
	}
	q.Set("model", filepath.Base(d.Trans.Model))
	u.RawQuery = q.Encode()

	method := act.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/vtt")
	if err := d.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("the file changed while it was being transcribed, dropping it (%v)", readProblem(resp))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readProblem(resp)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return nil
}

func (d deps) get(ctx context.Context, addr string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := d.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readProblem(resp)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

func (d deps) authorize(ctx context.Context, req *http.Request) error {
	if d.Tokens == nil {
		return nil
	}
	tok, err := d.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}

// resolve turns an href out of a document into an absolute address against
// the flickr base.
func resolve(base, href string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("bad FLICKR_URL %q: %w", base, err)
	}
	h, err := url.Parse(href)
	if err != nil {
		return "", fmt.Errorf("bad href %q: %w", href, err)
	}
	return b.ResolveReference(h).String(), nil
}

// readProblem reads a refusal: application/problem+json says what happened in
// words, and those words are the whole point of logging it.
func readProblem(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &p) == nil && p.Title != "" {
		if p.Detail != "" {
			return fmt.Errorf("%s: %s: %s", resp.Status, p.Title, p.Detail)
		}
		return fmt.Errorf("%s: %s", resp.Status, p.Title)
	}
	return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(body))
}
