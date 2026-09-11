package main

// The pass: read flickr's item list, plan every file, convert the ones that
// need it one at a time, kick a scan so flickr sees what changed, sleep, ask
// again. Everything that touches the world arrives in deps — the HTTP
// client, the bucket, the exec seam, the clock, the sleep — so a test drives
// a whole pass in milliseconds without an ffmpeg, a MinIO or a flickr.
//
// The worker composes exactly one address, /api/, and follows a relation for
// every other: the root's links.items is the list, and its actions.scan is
// how flickr is told. Same rule as the browser's.

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
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"flickr/internal/bearer"
	"flickr/internal/model"
	"flickr/internal/pipeline"
)

// convertTimeout bounds one file end to end: a long film through the card
// is minutes, through a remux a few more; a wedged run must not hold the
// worker for a day.
const convertTimeout = 6 * time.Hour

// backoffStart and backoffMax bound the retry when flickr cannot be reached.
const (
	backoffStart = time.Minute
	backoffMax   = 15 * time.Minute
)

// scanEvery is how long converted files wait to be seen: a pass over the
// library is a day of the card, an original is gone the moment its
// replacement is up, and flickr's own scan is twelve hours apart — so a
// scan is asked for this often while files are landing, and once more when
// the pass ends. The scan is incremental (list, diff etags), so it is cheap.
const scanEvery = 15 * time.Minute

// deps is the world.
type deps struct {
	HTTP     *http.Client
	Tokens   bearer.Source
	Bucket   bucket
	Run      func(ctx context.Context, name string, args []string) ([]byte, error)
	Sleep    func(ctx context.Context, d time.Duration)
	Now      func() time.Time
	Stopping <-chan struct{}
}

type link struct {
	Href string `json:"href"`
}

type action struct {
	Method string `json:"method"`
	Href   string `json:"href"`
}

type rootDoc struct {
	Links   map[string]link   `json:"links"`
	Actions map[string]action `json:"actions"`
}

// item is what the worker reads of a library row: the key, the etag the
// probe was made against, and the probe.
type item struct {
	ID        int64            `json:"id"`
	ObjectKey string           `json:"object_key"`
	ETag      string           `json:"etag"`
	Size      int64            `json:"size"`
	MediaInfo *model.MediaInfo `json:"media_info"`
	Identity  *struct {
		Title string `json:"title"`
	} `json:"identity"`
}

func (it item) title() string {
	if it.Identity != nil && it.Identity.Title != "" {
		return it.Identity.Title
	}
	return path.Base(it.ObjectKey)
}

func (it item) seconds() float64 {
	if it.MediaInfo == nil {
		return 0
	}
	return it.MediaInfo.DurationSeconds
}

// job is one file and what the plan says about it.
type job struct {
	item item
	plan pipeline.Plan
}

// run is the loop. It returns when the context is cancelled or a stop signal
// has arrived; nothing else ends it, because a library that has been
// converted is a library that will get a new file tomorrow.
func run(ctx context.Context, cfg config, d deps) error {
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

	card := d.probeCard(ctx, cfg)

	backoff := time.Duration(0)
	reported := false
	for {
		if stopping(ctx, d.Stopping) {
			return nil
		}
		root, items, err := d.library(ctx, cfg.FlickrURL)
		if err != nil {
			backoff = nextBackoff(backoff)
			log.Printf("converter: reading the library: %v (asking again in %s)", err, backoff)
			d.Sleep(wait, backoff)
			continue
		}
		backoff = 0

		jobs := plan(items)
		log.Printf("converter: %s", totals(items, jobs))
		if !reported {
			// The whole plan, once: a dry run's answer, and the record of
			// what a live worker is about to do.
			for _, j := range jobs {
				log.Printf("converter: plan %s item %d %q: %s", j.plan.Action, j.item.ID, j.item.ObjectKey, j.plan.Reason)
			}
			reported = true
		}
		if cfg.DryRun || len(jobs) == 0 {
			if cfg.DryRun {
				log.Printf("converter: dry run — nothing written; asking again in %s", cfg.IdleSleep)
			} else {
				log.Printf("converter: nothing to convert, asking again in %s", cfg.IdleSleep)
			}
			d.Sleep(wait, cfg.IdleSleep)
			continue
		}

		// unseen counts the files converted since flickr was last asked to
		// look; lastScan is when that was.
		unseen := 0
		var lastScan time.Time
		ask := func() {
			if err := d.scan(ctx, cfg.FlickrURL, root); err != nil {
				log.Printf("converter: asking for a scan after %d file(s): %v", unseen, err)
				return
			}
			log.Printf("converter: %d file(s) converted, scan requested", unseen)
			unseen, lastScan = 0, d.Now()
		}
		for _, j := range jobs {
			if stopping(ctx, d.Stopping) {
				break
			}
			if j.plan.Action == pipeline.ConvertEncode && !card {
				log.Printf("converter: item %d (%s): no card for the encode, left as it is", j.item.ID, j.item.ObjectKey)
				continue
			}
			if d.convert(ctx, cfg, j) {
				unseen++
				if d.Now().Sub(lastScan) >= scanEvery {
					ask()
				}
			}
		}
		if unseen > 0 {
			ask()
		}
		if stopping(ctx, d.Stopping) {
			return nil
		}
		d.Sleep(wait, cfg.IdleSleep)
	}
}

// plan decides every item and orders the work: remuxes first, because a
// remux is minutes of disk and no card, and inside each kind the smaller
// file first so a stopped worker has finished as many as it could. Skips
// are not work and are not returned.
func plan(items []item) []job {
	var jobs []job
	for _, it := range items {
		p := pipeline.ConvertPlan(it.ObjectKey, it.MediaInfo)
		if p.Action == pipeline.ConvertSkip {
			continue
		}
		jobs = append(jobs, job{item: it, plan: p})
	}
	sort.SliceStable(jobs, func(i, k int) bool {
		a, b := jobs[i], jobs[k]
		if a.plan.Action != b.plan.Action {
			return a.plan.Action == pipeline.ConvertRemux
		}
		return a.item.Size < b.item.Size
	})
	return jobs
}

// totals is the one line that says how much of the library is in question.
func totals(items []item, jobs []job) string {
	var videos, remux, encode int
	var remuxBytes, encodeBytes int64
	var encodeSeconds float64
	for _, it := range items {
		if it.MediaInfo != nil && it.MediaInfo.MediumOrVideo() == model.MediumVideo && it.MediaInfo.VideoCodec != "" {
			videos++
		}
	}
	for _, j := range jobs {
		switch j.plan.Action {
		case pipeline.ConvertRemux:
			remux++
			remuxBytes += j.item.Size
		case pipeline.ConvertEncode:
			encode++
			encodeBytes += j.item.Size
			encodeSeconds += j.item.seconds()
		}
	}
	return fmt.Sprintf("%d video files: %d already play everywhere, %d to remux (%.1f GB), %d to re-encode (%.1f GB, %.1f hours of film)",
		videos, videos-remux-encode, remux, gb(remuxBytes), encode, gb(encodeBytes), encodeSeconds/3600)
}

func gb(n int64) float64 { return float64(n) / 1e9 }

// convert does one file and reports whether the bucket changed. Every
// failure is this file's failure: it is logged and the pass moves on.
func (d deps) convert(ctx context.Context, cfg config, j job) bool {
	it, p := j.item, j.plan
	ctx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()

	// The guard: the scan this plan was made from must still describe the
	// object. A key that is gone was converted on an earlier pass and the
	// scan has not caught up; an etag that moved is a file somebody
	// replaced, and its next probe is the plan to trust.
	etag, err := d.Bucket.Stat(ctx, it.ObjectKey)
	if err == errGone {
		log.Printf("converter: item %d (%s): already gone from the bucket — converted on an earlier pass, the scan has not caught up", it.ID, it.ObjectKey)
		return false
	}
	if err != nil {
		log.Printf("converter: item %d (%s): %v", it.ID, it.ObjectKey, err)
		return false
	}
	if etag != it.ETag {
		log.Printf("converter: item %d (%s): changed since the scan, left for the next one", it.ID, it.ObjectKey)
		return false
	}

	dir := filepath.Join(cfg.WorkDir, strconv.FormatInt(it.ID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("converter: item %d: %v", it.ID, err)
		return false
	}
	defer os.RemoveAll(dir) // the file lives in the bucket now, not here
	out := filepath.Join(dir, "out.mp4")

	src, err := d.Bucket.Presign(ctx, it.ObjectKey)
	if err != nil {
		log.Printf("converter: item %d (%s): %v", it.ID, it.ObjectKey, err)
		return false
	}
	// The picture subtitles first: read into sidecars and uploaded before
	// the file they belong to changes, so a file that cannot be read stays
	// exactly as it was, subtitles and all.
	if len(p.Bitmap) > 0 {
		if err := d.ocr(ctx, cfg, it, p, dir, src); err != nil {
			log.Printf("converter: item %d (%s): subtitles not read, file left as it is: %v", it.ID, it.ObjectKey, err)
			return false
		}
	}
	started := d.Now()
	if err := d.ffmpeg(ctx, cfg, p, src, out); err != nil {
		log.Printf("converter: item %d (%s): %v", it.ID, it.ObjectKey, err)
		return false
	}
	probe, err := d.Run(ctx, cfg.FFprobe, pipeline.ProbeArgs(out))
	if err != nil {
		log.Printf("converter: item %d (%s): ffprobe: %v: %s", it.ID, it.ObjectKey, err, tail(probe))
		return false
	}
	if err := pipeline.Verify(probe, p, it.seconds()); err != nil {
		log.Printf("converter: item %d (%s): the new file is wrong, not uploaded: %v", it.ID, it.ObjectKey, err)
		return false
	}
	wall := d.Now().Sub(started)
	st, err := os.Stat(out)
	if err != nil {
		log.Printf("converter: item %d: %v", it.ID, err)
		return false
	}

	if err := d.Bucket.Put(ctx, p.OutKey, out, "video/mp4"); err != nil {
		log.Printf("converter: item %d (%s): uploading %s: %v", it.ID, it.ObjectKey, p.OutKey, err)
		return false
	}
	if p.OutKey != it.ObjectKey && !cfg.KeepOriginal {
		if err := d.Bucket.Remove(ctx, it.ObjectKey); err != nil {
			// The new file is up; the old one lingers until somebody
			// removes it, and flickr will show both until then.
			log.Printf("converter: item %d: %s uploaded but %s not removed: %v", it.ID, p.OutKey, it.ObjectKey, err)
		}
	}
	log.Printf("converter: %s item %d %q: %s → %s in %s (%s, %.2f → %.2f GB)",
		p.Action, it.ID, it.title(), path.Base(it.ObjectKey), path.Base(p.OutKey),
		wall.Round(time.Second), realtime(it.seconds(), wall), gb(it.Size), gb(st.Size()))
	return true
}

// ocr turns every picture track of the plan into a .srt sidecar in the
// bucket. Nothing is uploaded until every track has read into at least one
// cue: half a file's subtitles is not a result, it is a file to look at.
func (d deps) ocr(ctx context.Context, cfg config, it item, p pipeline.Plan, dir, src string) error {
	mkv := filepath.Join(dir, "subs.mkv")
	if out, err := d.Run(ctx, cfg.FFmpeg, pipeline.ExtractSubsArgs(src, p.Bitmap, mkv)); err != nil {
		return fmt.Errorf("extracting the picture tracks: %v: %s", err, tail(out))
	}
	if out, err := d.Run(ctx, "mkvextract", pipeline.MKVExtractArgs(mkv, dir, p.Bitmap)); err != nil {
		return fmt.Errorf("mkvextract: %v: %s", err, tail(out))
	}
	keys := pipeline.SidecarKeys(it.ObjectKey, p.Bitmap)
	srts := make([]string, len(p.Bitmap))
	for n, t := range p.Bitmap {
		in := pipeline.BitmapFile(dir, n, t)
		srts[n] = pipeline.SRTFile(in)
		if t.Codec == "dvd_subtitle" {
			idx, err := os.ReadFile(in)
			if err != nil {
				return fmt.Errorf("mkvextract wrote no idx for track %d: %v", t.Ordinal, err)
			}
			if err := os.WriteFile(in, pipeline.NormalizeIDX(idx), 0o644); err != nil {
				return err
			}
		}
		name, args := pipeline.OCRCommand(t, in, srts[n])
		started := d.Now()
		if out, err := d.Run(ctx, name, args); err != nil {
			return fmt.Errorf("%s on track %d: %v: %s", name, t.Ordinal, err, tail(out))
		}
		b, err := os.ReadFile(srts[n])
		if err != nil {
			return fmt.Errorf("%s on track %d wrote no srt: %v", name, t.Ordinal, err)
		}
		cues := strings.Count(string(b), "-->")
		if cues == 0 {
			return fmt.Errorf("%s on track %d read no cues", name, t.Ordinal)
		}
		log.Printf("converter: item %d track %d (%s%s): %d cues read in %s → %s",
			it.ID, t.Ordinal, t.Codec, langSuffix(t.Language), cues, d.Now().Sub(started).Round(time.Second), path.Base(keys[n]))
	}
	for n := range p.Bitmap {
		if err := d.Bucket.Put(ctx, keys[n], srts[n], "application/x-subrip"); err != nil {
			return fmt.Errorf("uploading %s: %v", keys[n], err)
		}
	}
	return nil
}

func langSuffix(lang string) string {
	if lang == "" {
		return ""
	}
	return " " + lang
}

// ffmpeg runs the plan, on the card first. A card that refuses the DECODE —
// a profile it does not know, a stream it will not parse — is answered by
// decoding in software and uploading the frames; a file the upload cannot
// carry either — frames that change shape mid-stream, which ffmpeg answers
// by rebuilding a filter graph the card cannot rebuild — is answered by
// libx264 on the cores. A remux has one chain and no fallback.
func (d deps) ffmpeg(ctx context.Context, cfg config, p pipeline.Plan, src, out string) error {
	chains := []struct {
		chain pipeline.Chain
		name  string
	}{{pipeline.CardChain, "on the card"}}
	if !p.VideoCopy {
		if p.HWDecode {
			chains = append(chains, struct {
				chain pipeline.Chain
				name  string
			}{pipeline.UploadChain, "decoding in software"})
		}
		chains = append(chains, struct {
			chain pipeline.Chain
			name  string
		}{pipeline.SoftwareChain, "libx264, no card"})
	}
	var lastErr error
	for i, c := range chains {
		if i > 0 {
			log.Printf("converter: %v; trying %s", lastErr, c.name)
			os.Remove(out)
		}
		outp, err := d.Run(ctx, cfg.FFmpeg, pipeline.ConvertArgs(p, src, out, cfg.Device, c.chain))
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("ffmpeg %s: %v: %s", c.name, err, tail(outp))
		if ctx.Err() != nil {
			break // a cancelled run is not a chain to fall through
		}
	}
	return lastErr
}

// probeCard encodes one second on the card and says whether it worked. A
// box with no card still remuxes; it just does not encode.
func (d deps) probeCard(ctx context.Context, cfg config) bool {
	out, err := d.Run(ctx, cfg.FFmpeg, pipeline.CardProbeArgs(cfg.Device))
	if err != nil {
		log.Printf("converter: %s cannot encode h264_vaapi on %s (%v: %s) — remuxes only, no re-encodes", cfg.FFmpeg, cfg.Device, err, tail(out))
		return false
	}
	log.Printf("converter: h264_vaapi on %s is live", cfg.Device)
	return true
}

// library reads the root and follows links.items.
func (d deps) library(ctx context.Context, base string) (rootDoc, []item, error) {
	var root rootDoc
	if err := d.get(ctx, base+"/api/", &root); err != nil {
		return root, nil, err
	}
	l, ok := root.Links["items"]
	if !ok {
		return root, nil, fmt.Errorf("%s/api/ offers no items link", base)
	}
	addr, err := resolve(base, l.Href)
	if err != nil {
		return root, nil, err
	}
	var items []item
	if err := d.get(ctx, addr, &items); err != nil {
		return root, nil, err
	}
	return root, items, nil
}

// scan follows the root's scan action.
func (d deps) scan(ctx context.Context, base string, root rootDoc) error {
	a, ok := root.Actions["scan"]
	if !ok {
		return fmt.Errorf("the root offers no scan action")
	}
	addr, err := resolve(base, a.Href)
	if err != nil {
		return err
	}
	method := a.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, addr, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := bearer.Authorize(ctx, d.Tokens, req); err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusConflict {
		return nil // a scan is already running: it will see the bucket
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s said %s: %s", addr, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func (d deps) get(ctx context.Context, addr string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if err := bearer.Authorize(ctx, d.Tokens, req); err != nil {
		return err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("%s said %s: %s", addr, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
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

// stopping reports whether this worker should stop between files.
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

// realtime is how many seconds of film went by for every second spent.
func realtime(seconds float64, wall time.Duration) string {
	if seconds <= 0 || wall <= 0 {
		return "realtime unknown"
	}
	return fmt.Sprintf("%.1fx realtime", seconds/wall.Seconds())
}

// tail is the last few hundred bytes of what a command printed: the error,
// not the banner.
func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 500 {
		s = "…" + s[len(s)-500:]
	}
	return s
}
