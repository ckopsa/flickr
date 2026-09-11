package main

// A whole pass, in milliseconds: an httptest flickr serves a root and four
// items, an in-memory bucket holds their etags, the exec seam plays ffmpeg
// and ffprobe, and the test's sleep ends the worker after one pass. No
// ffmpeg, no MinIO, no Keycloak.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"flickr/internal/bearer"
	"flickr/internal/model"
)

type fakeFlickr struct {
	t     *testing.T
	items []item
	mu    sync.Mutex
	scans []string // Authorization header of every scan request
}

func (f *fakeFlickr) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/house/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "converter" || r.Form.Get("client_secret") != "sssh" {
			f.t.Errorf("token request did not carry the client credentials: %v (%v)", r.Form, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token": "gpu-token", "expires_in": 300}`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer gpu-token" {
			f.t.Errorf("the root was read with %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"self": "/api/", "kind": "root", "links": {"items": {"href": "/api/items"}}, "actions": {"scan": {"method": "POST", "href": "/api/scan"}}}`)
	})
	mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.items)
	})
	mux.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			f.t.Errorf("scan used %s, want POST", r.Method)
		}
		f.mu.Lock()
		f.scans = append(f.scans, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}

// fakeBucket is the objects and their etags; a put records the file's
// size, a remove forgets the key.
type fakeBucket struct {
	mu      sync.Mutex
	etags   map[string]string
	puts    map[string]int64
	removed []string
}

func (b *fakeBucket) Presign(_ context.Context, key string) (string, error) {
	return "http://minio.test/bkt/" + key + "?sig=x", nil
}

func (b *fakeBucket) Stat(_ context.Context, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.etags[key]
	if !ok {
		return "", errGone
	}
	return e, nil
}

func (b *fakeBucket) Put(_ context.Context, key, p, _ string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.etags[key] = "new-" + key
	b.puts[key] = st.Size()
	return nil
}

func (b *fakeBucket) Remove(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.etags, key)
	b.removed = append(b.removed, key)
	return nil
}

// withBitmap gives an item an English PGS track and an unlabelled VobSub
// track: two sidecars to read before it is touched.
func withBitmap(it item) item {
	it.MediaInfo.Subtitles = []model.SubtitleTrack{
		{Ordinal: 0, Codec: "hdmv_pgs_subtitle", Language: "eng"},
		{Ordinal: 1, Codec: "dvd_subtitle"},
	}
	return it
}

func video(id int64, key, container, vc, ac string, ch int) item {
	return item{
		ID: id, ObjectKey: key, ETag: fmt.Sprintf("etag-%d", id), Size: id * 1_000_000_000,
		MediaInfo: &model.MediaInfo{
			Medium: model.MediumVideo, Container: container, VideoCodec: vc, AudioCodec: ac,
			AudioChannels: ch, Width: 1920, Height: 1080, DurationSeconds: 5400,
			AudioTracks: []model.AudioTrack{{Ordinal: 0, Codec: ac, Channels: ch, Default: true}},
		},
	}
}

// seam plays ffmpeg and ffprobe. ffmpeg writes its output file (the last
// argument) and, for the file that ends in "/3.mkv", refuses the card's
// decode once so the software path is taken. ffprobe answers a file shaped
// like the ffmpeg argv that made it.
type seam struct {
	mu       sync.Mutex
	calls    []string
	made     map[string][]string // out path -> the ffmpeg args that wrote it
	refusals int
}

func (s *seam) run(_ context.Context, name string, args []string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	s.mu.Lock()
	s.calls = append(s.calls, line)
	s.mu.Unlock()
	switch name {
	case "ffmpeg":
		if strings.Contains(line, "-f null -") {
			return nil, nil // the card probe
		}
		out := args[len(args)-1]
		if strings.Contains(line, "-f matroska") {
			return nil, os.WriteFile(out, []byte("mkv"), 0o644) // the picture tracks, pulled out
		}
		if strings.Contains(line, "-hwaccel vaapi") && strings.Contains(line, "/3.mkv") {
			s.mu.Lock()
			s.refusals++
			s.mu.Unlock()
			return []byte("[hevc @ 0x1] Failed to create a VAAPI decode context"), errors.New("exit status 1")
		}
		s.mu.Lock()
		if s.made == nil {
			s.made = map[string][]string{}
		}
		s.made[out] = args
		s.mu.Unlock()
		return nil, os.WriteFile(out, bytes.Repeat([]byte("mp4!"), 250), 0o644)
	case "mkvextract":
		for _, a := range args[2:] {
			if i := strings.Index(a, ":"); i > 0 {
				body := "pictures"
				if strings.HasSuffix(a, ".idx") {
					body = "palette: 20D620,35C7EF\n"
				}
				if err := os.WriteFile(a[i+1:], []byte(body), 0o644); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	case "pgsrip":
		in := args[len(args)-1]
		if strings.Contains(in, "/6/") {
			return []byte("0 PGS subtitles ripped"), nil // reads nothing: the file must be left alone
		}
		return nil, os.WriteFile(strings.TrimSuffix(in, ".en.sup")+".en.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nHi.\n\n2\n00:00:03,000 --> 00:00:04,000\nBye.\n"), 0o644)
	case "subtile-ocr":
		var out string
		for i, a := range args {
			if a == "-o" {
				out = args[i+1]
			}
		}
		// the idx reached the tool with its palette spaced the way it reads
		if idx, err := os.ReadFile(args[len(args)-1]); err != nil || !strings.Contains(string(idx), "palette: 20D620, 35C7EF") {
			return []byte("error during palette parsing"), errors.New("exit status 1")
		}
		return nil, os.WriteFile(out, []byte("1\n00:00:01,000 --> 00:00:02,000\nHi.\n"), 0o644)
	case "ffprobe":
		out := args[len(args)-1]
		s.mu.Lock()
		made := s.made[out]
		s.mu.Unlock()
		if made == nil {
			return []byte("No such file"), errors.New("exit status 1")
		}
		var streams []string
		streams = append(streams, `{"codec_type":"video","codec_name":"h264"}`)
		audio, subs := 0, 0
		for _, a := range made {
			if strings.HasPrefix(a, "0:a:") {
				audio++
			}
			if strings.HasPrefix(a, "0:s:") {
				subs++
			}
		}
		for i := 0; i < audio; i++ {
			if i == 0 {
				streams = append(streams, `{"codec_type":"audio","codec_name":"aac","channels":2}`)
			} else {
				streams = append(streams, `{"codec_type":"audio","codec_name":"eac3","channels":6}`)
			}
		}
		for i := 0; i < subs; i++ {
			streams = append(streams, `{"codec_type":"subtitle","codec_name":"mov_text"}`)
		}
		return []byte(`{"streams":[` + strings.Join(streams, ",") + `],"format":{"duration":"5399.8"}}`), nil
	}
	return nil, fmt.Errorf("unexpected command %s", name)
}

// pass runs one worker over the fake flickr and bucket and returns what it
// logged. The sleep after the pass cancels the run.
func pass(t *testing.T, f *fakeFlickr, b *fakeBucket, s *seam, dryRun bool) string {
	t.Helper()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config{
		FlickrURL: srv.URL, WorkDir: t.TempDir(), IdleSleep: 30 * time.Minute,
		DryRun: dryRun, Device: "/dev/dri/renderD128", FFmpeg: "ffmpeg", FFprobe: "ffprobe",
	}
	now := time.Unix(0, 0)
	d := deps{
		HTTP: srv.Client(),
		Tokens: &bearer.ClientCredentials{
			HTTP: srv.Client(), TokenURL: srv.URL + "/realms/house/protocol/openid-connect/token",
			ID: "converter", Secret: "sssh",
		},
		Bucket: b,
		Run:    s.run,
		Sleep:  func(context.Context, time.Duration) { cancel() },
		Now:    func() time.Time { now = now.Add(6 * time.Minute); return now },
	}
	if err := run(ctx, cfg, d); err != nil {
		t.Fatalf("run: %v", err)
	}
	entries, err := os.ReadDir(cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the work dir kept %d directories, want none", len(entries))
	}
	return logs.String()
}

func library() ([]item, *fakeBucket) {
	items := []item{
		video(1, "Movies/One (2001)/1.mp4", "mp4", "h264", "aac", 2),                // already fine
		video(2, "Movies/Two (2002)/2.mkv", "mkv", "h264", "aac", 2),                // a remux
		video(3, "Shows/Three/Season 1/3.mkv", "mkv", "hevc", "eac3", 6),            // an encode, card refuses the decode once
		video(4, "Movies/Four (2004)/4.mkv", "mkv", "hevc", "aac", 2),               // replaced since the scan
		video(5, "Movies/Five (2005)/5.mkv", "mkv", "av1", "opus", 6),               // converted on an earlier pass: gone
		withBitmap(video(6, "Movies/Six (2006)/6.mkv", "mkv", "h264", "aac", 2)),    // OCR reads nothing: left alone
		withBitmap(video(7, "Shows/Seven/Season 1/7.mkv", "mkv", "h264", "ac3", 6)), // OCR'd, then remuxed
	}
	b := &fakeBucket{etags: map[string]string{
		"Movies/One (2001)/1.mp4": "etag-1", "Movies/Two (2002)/2.mkv": "etag-2",
		"Shows/Three/Season 1/3.mkv": "etag-3", "Movies/Four (2004)/4.mkv": "someone-replaced-it",
		"Movies/Six (2006)/6.mkv": "etag-6", "Shows/Seven/Season 1/7.mkv": "etag-7",
	}, puts: map[string]int64{}}
	return items, b
}

func TestRunConvertsWhatNeedsIt(t *testing.T) {
	items, b := library()
	f := &fakeFlickr{t: t, items: items}
	s := &seam{}
	logs := pass(t, f, b, s, false)

	var put []string
	for k := range b.puts {
		put = append(put, k)
	}
	sort.Strings(put)
	want := []string{"Movies/Two (2002)/2.mp4", "Shows/Seven/Season 1/7.eng.2.srt", "Shows/Seven/Season 1/7.eng.srt", "Shows/Seven/Season 1/7.mp4", "Shows/Three/Season 1/3.mp4"}
	if strings.Join(put, ",") != strings.Join(want, ",") {
		t.Errorf("uploaded %v, want %v\n%s", put, want, logs)
	}
	sort.Strings(b.removed)
	if strings.Join(b.removed, ",") != "Movies/Two (2002)/2.mkv,Shows/Seven/Season 1/7.mkv,Shows/Three/Season 1/3.mkv" {
		t.Errorf("removed %v, want the three originals", b.removed)
	}
	// The file whose subtitles would not read is exactly as it was, and
	// says why; the one that read has its two sidecars up before its mp4.
	if b.etags["Movies/Six (2006)/6.mkv"] != "etag-6" || !strings.Contains(logs, "item 6 (Movies/Six (2006)/6.mkv): subtitles not read, file left as it is: pgsrip on track 0 wrote no srt") {
		t.Errorf("the unreadable file:\n%s", logs)
	}
	if !strings.Contains(logs, "item 7 track 0 (hdmv_pgs_subtitle eng): 2 cues read") || !strings.Contains(logs, "item 7 track 1 (dvd_subtitle): 1 cues read") {
		t.Errorf("the read tracks were not said:\n%s", logs)
	}
	if _, ok := b.etags["Movies/One (2001)/1.mp4"]; !ok {
		t.Error("the file that was already fine was touched")
	}
	if b.etags["Movies/Four (2004)/4.mkv"] != "someone-replaced-it" {
		t.Error("the file replaced since the scan was touched")
	}
	if !strings.Contains(logs, "item 4 (Movies/Four (2004)/4.mkv): changed since the scan") {
		t.Errorf("the replaced file was not explained:\n%s", logs)
	}
	if strings.Contains(logs, "item 5") && !strings.Contains(logs, "plan encode item 5") {
		t.Errorf("a file gone from the bucket is not news:\n%s", logs)
	}
	// The remux went first (cheap), then the encode; the encode fell back
	// to software when the card refused the decode, and still landed.
	if s.refusals != 1 || !strings.Contains(logs, "decoding in software") {
		t.Errorf("refusals = %d; logs:\n%s", s.refusals, logs)
	}
	var order []string
	for _, c := range s.calls {
		if strings.HasPrefix(c, "ffmpeg") && strings.Contains(c, "-i http") && !strings.Contains(c, "-f matroska") {
			order = append(order, c)
		}
	}
	// Remuxes by size (2, then 6 whose subtitles fail before its ffmpeg, then 7), then the encode: card, then software.
	if len(order) != 4 || !strings.Contains(order[0], "/2.mkv") || !strings.Contains(order[1], "/7.mkv") || !strings.Contains(order[2], "/3.mkv") || !strings.Contains(order[3], "/3.mkv") {
		t.Errorf("ffmpeg ran %d times: %v", len(order), order)
	}
	if !strings.Contains(order[2], "-hwaccel vaapi") || strings.Contains(order[3], "-hwaccel") || !strings.Contains(order[3], "hwupload") {
		t.Errorf("the second try must decode in software: %v", order[2:])
	}
	// A scan, with the bearer, as soon as the first file lands (the clock
	// starts long past the last one), then every quarter hour of the fake
	// clock, and once more for what landed after.
	if len(f.scans) < 2 {
		t.Errorf("scans = %v, want at least two", f.scans)
	}
	for _, sc := range f.scans {
		if sc != "Bearer gpu-token" {
			t.Errorf("a scan went without the bearer: %q", sc)
		}
	}
	for _, want := range []string{
		"h264_vaapi on /dev/dri/renderD128 is live",
		"7 video files: 1 already play everywhere, 3 to remux (15.0 GB), 3 to re-encode (12.0 GB, 4.5 hours of film)",
		"plan remux item 2",
		"plan encode item 3",
		`remux item 2 "2.mkv": 2.mkv → 2.mp4`,
		`encode item 3 "3.mkv": 3.mkv → 3.mp4`,
		"15.0x realtime",
		"file(s) converted, scan requested",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	items, b := library()
	f := &fakeFlickr{t: t, items: items}
	s := &seam{}
	logs := pass(t, f, b, s, true)

	if len(b.puts) != 0 || len(b.removed) != 0 || len(f.scans) != 0 {
		t.Errorf("a dry run touched the world: puts %v removed %v scans %v", b.puts, b.removed, f.scans)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "-i http") {
			t.Errorf("a dry run ran ffmpeg: %s", c)
		}
	}
	for _, want := range []string{
		"7 video files: 1 already play everywhere, 3 to remux",
		"plan remux item 7 \"Shows/Seven/Season 1/7.mkv\": mkv → mp4, aac stereo first, 2 bitmap subtitle track(s) OCR'd to sidecars",
		"plan remux item 2 \"Movies/Two (2002)/2.mkv\": mkv → mp4",
		"plan encode item 3 \"Shows/Three/Season 1/3.mkv\": hevc → h264, aac stereo first",
		"dry run — nothing written",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

// A file the card cannot encode is not uploaded: ffprobe says what came
// out, and Verify refuses it before the bucket is touched.
func TestAWrongFileIsNotUploaded(t *testing.T) {
	items, b := library()
	f := &fakeFlickr{t: t, items: items[:2]} // the one plain remux only
	s := &seam{}
	// ffmpeg writes the file; ffprobe says it is not what was asked for.
	d := func(ctx context.Context, name string, args []string) ([]byte, error) {
		if name == "ffprobe" {
			return []byte(`{"streams":[{"codec_type":"video","codec_name":"hevc"}],"format":{"duration":"5400"}}`), nil
		}
		return s.run(ctx, name, args)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := config{FlickrURL: srv.URL, WorkDir: t.TempDir(), IdleSleep: time.Hour, Device: "/dev/dri/renderD128", FFmpeg: "ffmpeg", FFprobe: "ffprobe"}
	deps := deps{
		HTTP: srv.Client(), Tokens: bearer.Static("gpu-token"), Bucket: b, Run: d,
		Sleep: func(context.Context, time.Duration) { cancel() }, Now: time.Now,
	}
	if err := run(ctx, cfg, deps); err != nil {
		t.Fatal(err)
	}
	if len(b.puts) != 0 || len(b.removed) != 0 || len(f.scans) != 0 {
		t.Errorf("a wrong file reached the bucket: puts %v removed %v scans %v", b.puts, b.removed, f.scans)
	}
	if !strings.Contains(logs.String(), "the new file is wrong, not uploaded: video is hevc, want h264") {
		t.Errorf("log:\n%s", logs.String())
	}
	if entries, _ := os.ReadDir(cfg.WorkDir); len(entries) != 0 {
		t.Errorf("the work dir kept %d directories", len(entries))
	}
}
