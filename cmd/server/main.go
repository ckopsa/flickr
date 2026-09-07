// Command server is the PoC media server: library API, playback decision
// endpoint (with trace), HLS streaming, and playback-state tracking.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	_ "time/tzdata" // honor TZ for TRICKPLAY_WINDOW even without host zoneinfo

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"flickr/internal/decision"
	"flickr/internal/model"
	"flickr/internal/pipeline"
	"flickr/internal/scanner"
	"flickr/internal/store"
	"flickr/internal/tmdb"
	"flickr/internal/works"
)

// sessionIdleTimeout is how long a transcode session may go without any
// stream fetch before the reaper stops it (clients that vanish never send
// an explicit stop).
const sessionIdleTimeout = 5 * time.Minute

type server struct {
	library  *store.Library
	state    *store.State
	scanner  *scanner.Scanner
	sessions *pipeline.SessionManager
	enricher *tmdb.Enricher // nil = TMDB enrichment disabled
	s3       *minio.Client
	bucket   string
	policy   model.ServerPolicy
	hw       *pipeline.HWReport
	baseURL  string // LAN-reachable address for devices that can't resolve localhost
	// trickplayBusy guards the background trickplay stage: full-file decodes
	// are expensive, so at most one stage pass runs at a time.
	trickplayBusy atomic.Bool
	// trickplayWindow, when non-nil, confines trickplay generation to a
	// daily clock window (TRICKPLAY_WINDOW) so library-wide backfills don't
	// saturate the node during the day.
	trickplayWindow *clockWindow
}

// trickplayDir is where per-item sprite-sheet sets live (data/trickplay/<id>/).
const trickplayDir = "data/trickplay"

// coversDir is where an audio item's embedded cover art is cached
// (data/covers/<id>.jpg), extracted on first request; <id>.none records a
// file that was asked and has no picture, so it is not asked again.
const coversDir = "data/covers"

func main() {
	loadDotEnv(".env")
	endpoint := envOr("MINIO_ENDPOINT", "192.168.1.40:9000")
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")
	bucket := envOr("MINIO_BUCKET", "jellyfin-media")
	addr := envOr("LISTEN_ADDR", ":8080")
	if accessKey == "" || secretKey == "" {
		log.Fatal("MINIO_ACCESS_KEY and MINIO_SECRET_KEY must be set (env or .env file)")
	}

	s3, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal(err)
	}

	if err := os.MkdirAll("data/streams", 0o755); err != nil {
		log.Fatal(err)
	}
	library, err := store.OpenLibrary("data/library.db")
	if err != nil {
		log.Fatal(err)
	}
	state, err := store.OpenState("data/state.db")
	if err != nil {
		log.Fatal(err)
	}

	hw := pipeline.DetectHW(context.Background())
	for _, step := range hw.Trace {
		log.Printf("hwdetect %s: ok=%v %s", step.Candidate, step.OK, step.Detail)
	}
	if hw.Chosen != nil {
		log.Printf("hardware encoding: %s (device %s)", hw.Chosen.Kind, hw.Chosen.Device)
	} else {
		log.Printf("hardware encoding: none verified, using software")
	}

	sessions := pipeline.NewSessionManager("data/streams", hw.Chosen)
	srv := &server{
		library: library,
		state:   state,
		// Two probe workers, and scanning pauses entirely while playback
		// sessions are live — the MinIO box can't feed both.
		scanner: &scanner.Scanner{
			Client: s3, Bucket: bucket, Library: library, Workers: 2,
			Yield: func() bool { return sessions.ActiveCount() > 0 },
		},
		sessions: sessions,
		s3:       s3,
		bucket:   bucket,
		policy:   model.DefaultPolicy(),
		hw:       hw,
		baseURL:  envOr("ADVERTISE_URL", lanBaseURL(endpoint, addr)),
	}
	srv.trickplayWindow, err = parseClockWindow(os.Getenv("TRICKPLAY_WINDOW"))
	if err != nil {
		log.Fatalf("bad TRICKPLAY_WINDOW: %v", err)
	}
	log.Printf("advertising as %s (cast devices fetch streams here)", srv.baseURL)

	if key := os.Getenv("TMDB_API_KEY"); key != "" {
		srv.enricher = &tmdb.Enricher{
			Client: tmdb.NewHTTPClient(key), Library: library,
			PostersDir: "data/posters", StillsDir: "data/stills",
		}
	} else {
		log.Printf("TMDB enrichment disabled (TMDB_API_KEY not set)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", srv.handleListItems)
	mux.HandleFunc("GET /api/works", srv.handleWorks)
	mux.HandleFunc("GET /api/works/{key}/items", srv.handleWorkItems)
	mux.HandleFunc("GET /api/artists", srv.handleArtists)
	mux.HandleFunc("GET /api/continue", srv.handleContinue)
	mux.HandleFunc("GET /api/feed/media", srv.handleFeed)
	mux.HandleFunc("POST /api/items/{id}/decision", srv.handleDecision)
	mux.HandleFunc("POST /api/items/{id}/play", srv.handlePlay)
	mux.HandleFunc("POST /api/items/{id}/identity", srv.handleOverrideIdentity)
	mux.HandleFunc("POST /api/items/{id}/reprobe", srv.handleReprobe)
	mux.HandleFunc("POST /api/items/{id}/enrich", srv.handleEnrich)
	mux.HandleFunc("GET /api/items/{id}/subtitles/{file}", srv.handleSubtitle)
	mux.HandleFunc("GET /api/items/{id}/poster", srv.handlePoster)
	mux.HandleFunc("GET /api/items/{id}/cover", srv.handleCover)
	mux.HandleFunc("GET /api/items/{id}/still", srv.handleStill)
	mux.HandleFunc("GET /api/items/{id}/book", srv.handleBook)
	mux.HandleFunc("GET /api/items/{id}/trickplay.json", srv.handleTrickplayIndex)
	mux.HandleFunc("GET /api/items/{id}/trickplay/{file}", srv.handleTrickplaySheet)
	mux.HandleFunc("POST /api/items/{id}/trickplay", srv.handleGenerateTrickplay)
	mux.HandleFunc("POST /api/scan", srv.handleScan)
	mux.HandleFunc("GET /api/scan", srv.handleScanStatus)
	mux.HandleFunc("GET /api/system", srv.handleSystem)
	mux.HandleFunc("DELETE /api/sessions/{id}", srv.handleStopSession)
	mux.HandleFunc("POST /api/progress", srv.handleSetProgress)
	mux.HandleFunc("GET /api/progress", srv.handleGetProgress)
	mux.HandleFunc("GET /api/users", srv.handleListUsers)
	mux.HandleFunc("POST /api/users", srv.handleCreateUser)
	mux.HandleFunc("POST /api/telemetry", srv.handleTelemetry)
	// Log stream fetches: which client asked for which segment with what
	// Range — a poor man's receiver-side network tab.
	streamFiles := http.StripPrefix("/streams/", http.FileServer(http.Dir("data/streams")))
	mux.Handle("GET /streams/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The first path segment is the session id: any fetch inside a
		// session counts as liveness for the idle-session reaper.
		if rest := strings.TrimPrefix(r.URL.Path, "/streams/"); rest != "" {
			id, _, _ := strings.Cut(rest, "/")
			srv.sessions.Touch(id)
		}
		log.Printf("stream %s %s range=%q ua=%.40q", r.RemoteAddr, r.URL.Path, r.Header.Get("Range"), r.UserAgent())
		// Go's sniffer has no idea what .m3u8/.m4s are and labels a playlist
		// "text/plain", which players are entitled to reject. Name them.
		switch path.Ext(r.URL.Path) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		case ".m4s":
			w.Header().Set("Content-Type", "video/iso.segment")
		}
		streamFiles.ServeHTTP(w, r)
	}))
	mux.Handle("GET /", http.FileServer(http.Dir("web")))

	// Permissive CORS: the Cast receiver fetches playlists/segments from a
	// different origin and preflights Range requests.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	// Session reaper: a vanished client (closed tab, unplugged cast device)
	// never sends a stop; without this, ffmpeg transcodes to file-end.
	go func() {
		for range time.Tick(60 * time.Second) {
			for _, id := range srv.sessions.ReapIdle(sessionIdleTimeout) {
				log.Printf("reaped session %s: no stream fetch for %s (client gone)", id, sessionIdleTimeout)
			}
		}
	}()

	// Scan scheduling: an initial scan when the library is empty, then a
	// periodic rescan. Scan itself refuses concurrent runs, so overlap with
	// a manually-triggered scan is harmless.
	scanInterval, err := time.ParseDuration(envOr("SCAN_INTERVAL", "12h"))
	if err != nil {
		log.Fatalf("bad SCAN_INTERVAL: %v", err)
	}
	if n, err := library.Count(); err == nil && n == 0 {
		log.Printf("library is empty — starting initial scan")
		go srv.runScan(context.Background())
	}
	if scanInterval > 0 {
		log.Printf("scheduled scans every %s (SCAN_INTERVAL)", scanInterval)
		go func() {
			for range time.Tick(scanInterval) {
				srv.runScan(context.Background())
			}
		}()
	} else {
		log.Printf("scheduled scans disabled (SCAN_INTERVAL=0)")
	}

	// With a trickplay window, scan-triggered passes outside it no-op, so
	// run a pass at each window opening to work through the backlog.
	if w := srv.trickplayWindow; w != nil {
		log.Printf("trickplay generation confined to %02d:%02d-%02d:%02d (%s) (TRICKPLAY_WINDOW)",
			w.start/60, w.start%60, w.end/60, w.end%60, time.Now().Format("MST"))
		go func() {
			for {
				if d := w.untilOpen(time.Now()); d > 0 {
					time.Sleep(d + time.Minute) // +1m: land inside the edge minute
				}
				srv.runTrickplay(context.Background())
				time.Sleep(time.Minute) // retry gap while open (pass may have yielded)
			}
		}()
	}

	httpSrv := &http.Server{Addr: addr, Handler: handler}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		srv.sessions.StopAll()
		httpSrv.Shutdown(context.Background())
	}()

	log.Printf("listening on %s (bucket %s at %s)", addr, bucket, endpoint)
	if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (s *server) handleListItems(w http.ResponseWriter, r *http.Request) {
	items, err := s.library.ListItems()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	if items == nil {
		items = []store.Item{}
	}
	writeJSON(w, items)
}

// buildWorks derives the works projection fresh from the library on every
// request — pure derivation, nothing cached, nothing stored.
func (s *server) buildWorks() ([]works.Work, error) {
	items, err := s.library.ListItems()
	if err != nil {
		return nil, err
	}
	return works.Build(items), nil
}

// handleWorks lists the library as works (movies, whole shows, audiobooks,
// albums, books, stray files); each carries its kind, medium and author.
func (s *server) handleWorks(w http.ResponseWriter, r *http.Request) {
	ws, err := s.buildWorks()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	if ws == nil {
		ws = []works.Work{}
	}
	writeJSON(w, ws)
}

// handleWorkItems lists one work's member items (episodes in season/episode
// order, parts and tracks in part order), in the same JSON shape as
// /api/items.
func (s *server) handleWorkItems(w http.ResponseWriter, r *http.Request) {
	ws, err := s.buildWorks()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	wk := works.ByKey(ws)[r.PathValue("key")]
	if wk == nil {
		httpErr(w, 404, fmt.Errorf("no such work"))
		return
	}
	writeJSON(w, wk.Items)
}

// handleArtists lists the album works shelved by artist: name, album count,
// albums in year order, and the item whose cover is the artist's picture.
func (s *server) handleArtists(w http.ResponseWriter, r *http.Request) {
	ws, err := s.buildWorks()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, works.Artists(ws))
}

// handleContinue is a profile's resume list: most-recent first, capped,
// finished and sub-5s positions dropped, one entry per work.
func (s *server) handleContinue(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		httpErr(w, 400, fmt.Errorf("client_id is required"))
		return
	}
	ws, err := s.buildWorks()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	positions, err := s.state.PositionsFor(clientID)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	entries := works.ContinueList(ws, positions, 20)
	if entries == nil {
		entries = []works.ContinueEntry{}
	}
	writeJSON(w, entries)
}

// handleFeed is the change feed: works changed since the `since` cursor
// (absent = everything), each with per-audience progress, plus the cursor to
// pass next time. Cursors are monotonic sequence pairs ("l<lib>.s<state>"),
// never timestamps.
func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	var since *works.Cursor
	if raw := r.URL.Query().Get("since"); raw != "" {
		c, err := works.ParseCursor(raw)
		if err != nil {
			httpErr(w, 400, err)
			return
		}
		since = &c
	}
	// Read both counters BEFORE the item/position reads: writes that land
	// mid-derivation then reappear after the next cursor (duplicates are
	// harmless, gaps would not be).
	libSeq, err := s.library.FeedSeq()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	stateSeq, err := s.state.FeedSeq()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	deletionSeq, err := s.library.DeletionSeq()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	ws, err := s.buildWorks()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	positions, err := s.state.AllPositions()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	changed := works.Feed(ws, positions, since, deletionSeq)
	if changed == nil {
		changed = []works.FeedWork{}
	}
	writeJSON(w, map[string]any{
		"cursor": works.Cursor{Lib: libSeq, State: stateSeq}.String(),
		"works":  changed,
	})
}

// decisionInput is the request body for decision/play: the client's
// capability manifest plus optional playback parameters.
type decisionInput struct {
	Capabilities model.ClientCapabilities `json:"capabilities"`
	ClientID     string                   `json:"client_id"`
	SeekSeconds  float64                  `json:"seek_seconds"`
	// AudioTrack is the ordinal (into media_info.audio_tracks) of the audio
	// stream to play; nil = first/default track.
	AudioTrack *int `json:"audio_track"`
	// SubtitleBurn is the ordinal (into media_info.subtitles) of an EMBEDDED
	// subtitle track to burn into the video — intended for bitmap tracks
	// (PGS/VobSub) that cannot be served as WebVTT. Forces a video re-encode.
	SubtitleBurn *int `json:"subtitle_burn"`
}

func (s *server) itemAndDecision(w http.ResponseWriter, r *http.Request) (*store.Item, *decisionInput, *model.PlayDecision, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return nil, nil, nil, false
	}
	item, err := s.library.GetItem(id)
	if err != nil {
		httpErr(w, 500, err)
		return nil, nil, nil, false
	}
	if item == nil {
		httpErr(w, 404, fmt.Errorf("no such item"))
		return nil, nil, nil, false
	}
	if item.MediaInfo == nil {
		httpErr(w, 409, fmt.Errorf("item has no media info (probe failed: %s)", item.ProbeError))
		return nil, nil, nil, false
	}
	// A book has no stream to decide about: the reader (bead flickr-9au)
	// opens it, not the player. Audio items take the audio branch of Decide.
	if m := item.MediaInfo.MediumOrVideo(); m == model.MediumText {
		httpErr(w, 415, fmt.Errorf("text items do not play (medium %q) — they are read, not streamed", m))
		return nil, nil, nil, false
	}
	var in decisionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, err)
		return nil, nil, nil, false
	}
	// Validate playback selections against the probed streams before they
	// reach the (pure) decision engine; bad ordinals are a client error.
	if in.AudioTrack != nil {
		if n := *in.AudioTrack; n < 0 || n >= len(item.MediaInfo.AudioTracks) {
			httpErr(w, 400, fmt.Errorf("audio_track %d out of range (item has %d audio tracks)",
				n, len(item.MediaInfo.AudioTracks)))
			return nil, nil, nil, false
		}
	}
	if in.SubtitleBurn != nil {
		tr := findSubtitle(item.MediaInfo, *in.SubtitleBurn)
		if tr == nil {
			httpErr(w, 400, fmt.Errorf("subtitle_burn %d: item has no such subtitle track", *in.SubtitleBurn))
			return nil, nil, nil, false
		}
		if tr.External {
			httpErr(w, 400, fmt.Errorf("subtitle_burn %d: track is an external sidecar, only embedded tracks can be burned in", *in.SubtitleBurn))
			return nil, nil, nil, false
		}
	}
	// DecideWith takes media by value and substitutes the selected track's
	// codec/channels into its own copy — the stored MediaInfo is never mutated.
	d := decision.DecideWith(*item.MediaInfo, in.Capabilities, s.policy,
		decision.Options{AudioTrack: in.AudioTrack, BurnSubtitle: in.SubtitleBurn})
	return item, &in, &d, true
}

func (s *server) handleDecision(w http.ResponseWriter, r *http.Request) {
	_, _, d, ok := s.itemAndDecision(w, r)
	if !ok {
		return
	}
	writeJSON(w, d)
}

func (s *server) handlePlay(w http.ResponseWriter, r *http.Request) {
	item, in, d, ok := s.itemAndDecision(w, r)
	if !ok {
		return
	}
	resp := map[string]any{"decision": d}

	switch d.Method {
	case model.DirectPlay:
		u, err := s.s3.PresignedGetObject(r.Context(), s.bucket, item.ObjectKey, 6*time.Hour, url.Values{})
		if err != nil {
			httpErr(w, 500, err)
			return
		}
		resp["url"] = u.String()

	case model.Transcode:
		u, err := s.s3.PresignedGetObject(r.Context(), s.bucket, item.ObjectKey, 6*time.Hour, url.Values{})
		if err != nil {
			httpErr(w, 500, err)
			return
		}
		// Seeking into a stream whose audio must be re-encoded needs an
		// output-side trim (see SeekAudioPrerollSeconds), which requires
		// decoded video too — upgrade a copy-video plan and say so.
		if in.SeekSeconds > 0 && d.Target.AudioCodec != "" && d.Target.VideoCodec == "" && !d.Target.AudioOnly {
			d.Target.VideoCodec = s.policy.TranscodeVideoCodec
			d.Target.VideoBitrateBps = s.policy.TranscodeVideoBitrateBps
			d.Trace = append(d.Trace, model.TraceStep{
				Check: "seek_adjustment", Passed: true,
				Detail: fmt.Sprintf(
					"re-encoding video (was copy): seeking to %.0fs with re-encoded audio needs a decoded pre-roll trim",
					in.SeekSeconds),
			})
		}
		sess, err := s.sessions.Create(u.String(), *d.Target, in.SeekSeconds)
		if err != nil {
			httpErr(w, 500, err)
			return
		}
		// Generous timeout: seeks with audio pre-roll must download and
		// decode ~10s of media first, and the storage box may be slow.
		if err := sess.WaitForPlaylist(45 * time.Second); err != nil {
			// Grab the ffmpeg log before Stop deletes the session dir, so
			// the client sees why instead of a bare timeout.
			tail := readTail(filepath.Join("data/streams", sess.ID, "ffmpeg.log"), 500)
			s.sessions.Stop(sess.ID)
			if tail != "" {
				err = fmt.Errorf("%s; ffmpeg log tail: %s", err, tail)
			}
			httpErr(w, 500, err)
			return
		}
		// ABR sessions hand the client the master playlist; hls.js and cast
		// receivers both speak master playlists natively.
		resp["url"] = "/streams/" + sess.ID + "/" + sess.PlaylistName()
		resp["session_id"] = sess.ID
		if d.Target.VideoCodec != "" {
			resp["video_encoder"] = pipeline.ResolveEncoder(s.sessions.HW, d.Target.VideoCodec)
		}

	case model.Deny:
		w.WriteHeader(http.StatusForbidden)
	}
	writeJSON(w, resp)
}

func (s *server) handleOverrideIdentity(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	var ident model.Identity
	if err := json.NewDecoder(r.Body).Decode(&ident); err != nil {
		httpErr(w, 400, err)
		return
	}
	if err := s.library.OverrideIdentity(id, ident); err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *server) handleReprobe(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil || item == nil {
		httpErr(w, 404, fmt.Errorf("no such item"))
		return
	}
	updated, err := s.scanner.ReprobeKey(r.Context(), item.ObjectKey)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, updated)
}

func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	go s.runScan(context.Background())
	writeJSON(w, map[string]string{"status": "started"})
}

// runScan performs one scan, then the TMDB enrichment pass (if enabled),
// then the trickplay generation pass. Used by the manual endpoint, the
// startup scan, and the interval scheduler.
func (s *server) runScan(ctx context.Context) {
	if err := s.scanner.Scan(ctx); err != nil {
		log.Printf("scan error: %v", err)
		return
	}
	if st := s.scanner.Status(); st.IdentityRefreshed > 0 {
		log.Printf("scan: refreshed identity for %d items without re-probing (identity v%d)",
			st.IdentityRefreshed, scanner.IdentityVersion)
	}
	if s.enricher != nil {
		n, err := s.enricher.EnrichAll(ctx)
		if err != nil {
			log.Printf("enrichment error: %v", err)
		} else if n > 0 {
			log.Printf("enriched %d items from TMDB", n)
		}
	}
	s.runTrickplay(ctx)
}

// clockWindow is a daily wall-clock interval, possibly wrapping midnight
// (start == end would mean the empty window and is rejected at parse).
type clockWindow struct {
	start, end int // minutes since local midnight
}

// parseClockWindow parses "HH:MM-HH:MM" (e.g. "22:00-06:00"). Empty input
// means no window: nil, nil.
func parseClockWindow(s string) (*clockWindow, error) {
	if s == "" {
		return nil, nil
	}
	parse := func(hhmm string) (int, error) {
		t, err := time.Parse("15:04", hhmm)
		if err != nil {
			return 0, fmt.Errorf("bad time %q (want HH:MM): %w", hhmm, err)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return nil, fmt.Errorf("bad window %q (want HH:MM-HH:MM)", s)
	}
	start, err := parse(from)
	if err != nil {
		return nil, err
	}
	end, err := parse(to)
	if err != nil {
		return nil, err
	}
	if start == end {
		return nil, fmt.Errorf("bad window %q: start equals end", s)
	}
	return &clockWindow{start: start, end: end}, nil
}

func (w *clockWindow) open(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end // wraps midnight
}

// untilOpen is the duration from t to the next window start (zero if open).
func (w *clockWindow) untilOpen(t time.Time) time.Duration {
	if w.open(t) {
		return 0
	}
	next := time.Date(t.Year(), t.Month(), t.Day(), w.start/60, w.start%60, 0, 0, t.Location())
	if !next.After(t) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(t)
}

// runTrickplay is the post-enrichment trickplay stage: for every VIDEO item
// with media info and a meaningful duration that lacks sprite sheets,
// generate them — strictly one item at a time (a full-file decode each),
// yielding to live playback between items exactly like the scanner does.
// Audio and text items have no frames to preview and are skipped.
func (s *server) runTrickplay(ctx context.Context) {
	if os.Getenv("TRICKPLAY") == "0" {
		return
	}
	if s.trickplayWindow != nil && !s.trickplayWindow.open(time.Now()) {
		return // outside the window; the window scheduler will run the pass
	}
	if !s.trickplayBusy.CompareAndSwap(false, true) {
		return // a previous pass is still running
	}
	defer s.trickplayBusy.Store(false)

	items, err := s.library.ListItems()
	if err != nil {
		log.Printf("trickplay: list items: %v", err)
		return
	}
	done := 0
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		if it.MediaInfo == nil || it.MediaInfo.DurationSeconds <= 120 {
			continue
		}
		if it.MediaInfo.MediumOrVideo() != model.MediumVideo {
			continue // nothing to draw frames from
		}
		dest := filepath.Join(trickplayDir, strconv.FormatInt(it.ID, 10))
		if pipeline.HasTrickplay(dest) {
			continue
		}
		// Stop (not sleep) when the window closes mid-pass: the daily
		// window scheduler starts a fresh pass at the next opening, and
		// holding trickplayBusy for hours would block manual runs.
		if s.trickplayWindow != nil && !s.trickplayWindow.open(time.Now()) {
			log.Printf("trickplay: window closed, pausing pass (%d generated, resumes at next window)", done)
			return
		}
		// Same yield condition as scans: active playback sessions own the
		// storage bandwidth and the decode budget.
		for s.scanner.Yield != nil && s.scanner.Yield() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if err := s.generateTrickplay(ctx, &it); err != nil {
			log.Printf("trickplay: item %d (%s): %v", it.ID, it.ObjectKey, err)
			continue
		}
		done++
		log.Printf("trickplay: generated sheets for item %d (%s)", it.ID, it.ObjectKey)
	}
	if done > 0 {
		log.Printf("trickplay: pass complete, %d items generated", done)
	}
}

// generateTrickplay produces the sprite-sheet set for one item.
func (s *server) generateTrickplay(ctx context.Context, item *store.Item) error {
	u, err := s.s3.PresignedGetObject(ctx, s.bucket, item.ObjectKey, 6*time.Hour, url.Values{})
	if err != nil {
		return err
	}
	dest := filepath.Join(trickplayDir, strconv.FormatInt(item.ID, 10))
	return pipeline.GenerateTrickplay(ctx, u.String(), item.MediaInfo.DurationSeconds, dest)
}

func (s *server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.scanner.Status())
}

func (s *server) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"hardware": s.hw, "base_url": s.baseURL})
}

func (s *server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	s.sessions.Stop(r.PathValue("id"))
	writeJSON(w, map[string]string{"status": "stopped"})
}

// progressInput is a POST /api/progress body. Video and audio send seconds;
// the reader sends the text fields — the CFI of the page it shows, the
// book's own percentage, and the 1-based spine section (derived from the CFI
// when omitted) — and the row keeps whichever unit was sent last.
type progressInput struct {
	ItemID   int64    `json:"item_id"`
	ClientID string   `json:"client_id"`
	Position float64  `json:"position_seconds"`
	Locator  string   `json:"locator"`
	Fraction *float64 `json:"fraction"`
	Section  int      `json:"section"`
}

// progressOutput is GET /api/progress: the seconds always, the text fields
// only when the row holds a locator (fraction is then present even at 0).
type progressOutput struct {
	PositionSeconds float64  `json:"position_seconds"`
	Locator         string   `json:"locator,omitempty"`
	Fraction        *float64 `json:"fraction,omitempty"`
	Section         int      `json:"section,omitempty"`
}

// placeOf turns the text fields of a progress write into a Locator — nil
// when none was sent, so the write is a plain clock position. A fraction
// outside [0,1] or a negative section is a client error; a locator string
// that is not a CFI is stored as sent (the reader owns that grammar), just
// without a derived section.
func placeOf(in progressInput) (*model.Locator, error) {
	if in.Locator == "" && in.Fraction == nil && in.Section == 0 {
		return nil, nil
	}
	loc := &model.Locator{CFI: in.Locator, Section: in.Section}
	if in.Fraction != nil {
		f := *in.Fraction
		if !(f >= 0 && f <= 1) { // also rejects NaN
			return nil, fmt.Errorf("fraction %v out of range (want 0..1)", f)
		}
		loc.Fraction = f
	}
	if loc.Section < 0 {
		return nil, fmt.Errorf("section %d out of range (want a 1-based spine index)", loc.Section)
	}
	if loc.Section == 0 {
		loc.Section = model.SectionFromCFI(loc.CFI)
	}
	return loc, nil
}

func (s *server) handleSetProgress(w http.ResponseWriter, r *http.Request) {
	var in progressInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, err)
		return
	}
	loc, err := placeOf(in)
	if err != nil {
		httpErr(w, 400, err)
		return
	}
	if err := s.state.SetPlace(in.ItemID, in.ClientID, in.Position, loc); err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *server) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	itemID, _ := strconv.ParseInt(r.URL.Query().Get("item_id"), 10, 64)
	clientID := r.URL.Query().Get("client_id")
	p, err := s.state.GetPosition(itemID, clientID)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	out := progressOutput{PositionSeconds: p.PositionSeconds}
	if p.Locator != nil {
		f := p.Locator.Fraction
		out.Locator, out.Fraction, out.Section = p.Locator.CFI, &f, p.Locator.Section
	}
	writeJSON(w, out)
}

// handleBook streams a text item's bytes to the reader. The MinIO object is
// a seekable reader, so http.ServeContent answers Range requests (and
// If-Modified-Since) itself and only the bytes asked for leave the bucket.
// 415 for anything that is not text — a film is not a book to open.
func (s *server) handleBook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil || item == nil {
		httpErr(w, 404, fmt.Errorf("no such item"))
		return
	}
	if item.MediaInfo.MediumOrVideo() != model.MediumText {
		httpErr(w, 415, fmt.Errorf("item %d is not a text item", id))
		return
	}
	obj, err := s.s3.GetObject(r.Context(), s.bucket, item.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		httpErr(w, 502, err)
		return
	}
	defer obj.Close()
	st, err := obj.Stat()
	if err != nil {
		httpErr(w, 502, fmt.Errorf("object %s: %w", item.ObjectKey, err))
		return
	}
	w.Header().Set("Content-Type", "application/epub+zip")
	http.ServeContent(w, r, path.Base(item.ObjectKey), st.LastModified, obj)
}

// handleSubtitle serves one subtitle track as WebVTT, extracting it with
// ffmpeg on first request and from data/subs/ cache thereafter.
func (s *server) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	name, isVTT := strings.CutSuffix(r.PathValue("file"), ".vtt")
	ordinal, ordErr := strconv.Atoi(name)
	if !isVTT || ordErr != nil || ordinal < 0 {
		httpErr(w, 404, fmt.Errorf("no such subtitle"))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil || item == nil {
		httpErr(w, 404, fmt.Errorf("no such item"))
		return
	}
	track := findSubtitle(item.MediaInfo, ordinal)
	if track == nil {
		httpErr(w, 404, fmt.Errorf("item has no subtitle track %d", ordinal))
		return
	}
	if !track.Supported {
		httpErr(w, 415, fmt.Errorf("subtitle track %d is %s (bitmap), not convertible to WebVTT", ordinal, track.Codec))
		return
	}
	cachePath := filepath.Join("data/subs", fmt.Sprintf("%d-%d.vtt", id, ordinal))
	if _, err := os.Stat(cachePath); err != nil {
		if track.External {
			// External sidecar: the subtitle is its own object, not a stream
			// inside the video container.
			if err := s.cacheExternalSubtitle(r.Context(), track, cachePath); err != nil {
				httpErr(w, 500, err)
				return
			}
		} else {
			u, err := s.s3.PresignedGetObject(r.Context(), s.bucket, item.ObjectKey, time.Hour, url.Values{})
			if err != nil {
				httpErr(w, 500, err)
				return
			}
			// Embedded ordinals count subtitle streams inside the container;
			// external tracks never reach here, so the ordinal maps 1:1.
			if err := pipeline.ExtractSubtitle(r.Context(), u.String(), ordinal, cachePath); err != nil {
				httpErr(w, 500, err)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "text/vtt")
	http.ServeFile(w, r, cachePath)
}

// cacheExternalSubtitle fills the .vtt cache from a sidecar object: .vtt
// files pass through unchanged, .srt/.ass/.ssa are converted with ffmpeg.
func (s *server) cacheExternalSubtitle(ctx context.Context, track *model.SubtitleTrack, cachePath string) error {
	if track.ObjectKey == "" {
		return fmt.Errorf("external subtitle has no object key (re-scan needed)")
	}
	if track.Codec == "webvtt" {
		obj, err := s.s3.GetObject(ctx, s.bucket, track.ObjectKey, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer obj.Close()
		return writeFileAtomic(cachePath, obj)
	}
	u, err := s.s3.PresignedGetObject(ctx, s.bucket, track.ObjectKey, time.Hour, url.Values{})
	if err != nil {
		return err
	}
	return pipeline.ConvertSubtitle(ctx, u.String(), cachePath)
}

// writeFileAtomic streams src to path via temp file + rename, so a failed
// download never leaves a truncated file to be served from cache forever.
func writeFileAtomic(path string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// findSubtitle returns the probed subtitle track with the given ordinal, or nil.
func findSubtitle(info *model.MediaInfo, ordinal int) *model.SubtitleTrack {
	if info == nil {
		return nil
	}
	for i := range info.Subtitles {
		if info.Subtitles[i].Ordinal == ordinal {
			return &info.Subtitles[i]
		}
	}
	return nil
}

// handleTrickplayIndex serves the trickplay sidecar; 404 (never an error
// page) when the item has no generated sheets — clients probe this to decide
// whether to show hover previews at all.
func (s *server) handleTrickplayIndex(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	path := filepath.Join(trickplayDir, id, "index.json")
	if _, err := os.Stat(path); err != nil {
		httpErr(w, 404, fmt.Errorf("no trickplay for item %s", id))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	http.ServeFile(w, r, path)
}

// handleTrickplaySheet serves one sprite sheet ({n}.jpg -> sheet<n>.jpg).
func (s *server) handleTrickplaySheet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	name, isJpg := strings.CutSuffix(r.PathValue("file"), ".jpg")
	n, numErr := strconv.Atoi(name)
	if !isJpg || numErr != nil || n < 0 {
		httpErr(w, 404, fmt.Errorf("no such sheet"))
		return
	}
	path := filepath.Join(trickplayDir, id, fmt.Sprintf("sheet%d.jpg", n))
	if _, err := os.Stat(path); err != nil {
		httpErr(w, 404, fmt.Errorf("no such sheet"))
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, path)
}

// handleGenerateTrickplay force-generates the sprite sheets for one item,
// synchronously (useful for testing and for pre-warming a single title
// without waiting for the next scan). Regenerates even if sheets exist.
func (s *server) handleGenerateTrickplay(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil || item == nil {
		httpErr(w, 404, fmt.Errorf("no such item"))
		return
	}
	if item.MediaInfo == nil || item.MediaInfo.DurationSeconds <= 0 {
		httpErr(w, 409, fmt.Errorf("item has no usable media info"))
		return
	}
	if err := s.generateTrickplay(r.Context(), item); err != nil {
		httpErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	http.ServeFile(w, r, filepath.Join(trickplayDir, r.PathValue("id"), "index.json"))
}

func (s *server) handlePoster(w http.ResponseWriter, r *http.Request) {
	s.serveItemImage(w, r, "data/posters", "no poster")
}

// handleCover serves an item's OWN cover art — an audio item's attached
// picture, extracted with ffmpeg; a book's OPF cover, read out of the epub
// — on first request and cached under data/covers/ thereafter, the same
// on-demand discipline as subtitles and trickplay. An item with no cover
// of its own (recorded once, as <id>.none) and every video item fall back
// to the TMDB poster route, so a tile can ask for /cover without knowing
// the item's medium and still get whatever artwork there is. The cache
// file is named .jpg as a key, not a promise: the bytes are sniffed.
func (s *server) handleCover(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	if s.ensureCover(r.Context(), id) {
		f, err := os.Open(coverPath(id))
		if err != nil {
			httpErr(w, 500, err)
			return
		}
		defer f.Close()
		head := make([]byte, 512)
		n, _ := f.Read(head)
		w.Header().Set("Content-Type", http.DetectContentType(head[:n]))
		st, err := f.Stat()
		if err != nil {
			httpErr(w, 500, err)
			return
		}
		http.ServeContent(w, r, "cover", st.ModTime(), f)
		return
	}
	s.serveItemImage(w, r, "data/posters", "no cover")
}

func coverPath(id int64) string {
	return filepath.Join(coversDir, strconv.FormatInt(id, 10)+".jpg")
}

// ensureCover reports whether a cover image exists for the item, extracting
// it by medium if the item has not been asked before: ffmpeg for an audio
// item's attached picture, the OPF's cover image for a book. Video has no
// cover of its own (its artwork is the poster).
func (s *server) ensureCover(ctx context.Context, id int64) bool {
	if _, err := os.Stat(coverPath(id)); err == nil {
		return true
	}
	none := filepath.Join(coversDir, strconv.FormatInt(id, 10)+".none")
	if _, err := os.Stat(none); err == nil {
		return false
	}
	item, err := s.library.GetItem(id)
	if err != nil || item == nil {
		return false
	}
	remember := func(why error) bool {
		// No picture (or a broken one): remember, so the next tile render
		// does not download the file again for the same answer.
		log.Printf("cover: item %d (%s): none: %v", id, item.ObjectKey, why)
		if mkErr := os.MkdirAll(coversDir, 0o755); mkErr == nil {
			os.WriteFile(none, nil, 0o644)
		}
		return false
	}
	switch item.MediaInfo.MediumOrVideo() {
	case model.MediumAudio:
		u, err := s.s3.PresignedGetObject(ctx, s.bucket, item.ObjectKey, time.Hour, url.Values{})
		if err != nil {
			log.Printf("cover: item %d: presign: %v", id, err)
			return false
		}
		if err := pipeline.ExtractCover(ctx, u.String(), coverPath(id)); err != nil {
			return remember(err)
		}
		return true
	case model.MediumText:
		data, _, err := s.scanner.ReadEpubCover(ctx, item.ObjectKey)
		if errors.Is(err, scanner.ErrNoCover) {
			return remember(err)
		}
		if err != nil {
			log.Printf("cover: item %d (%s): %v", id, item.ObjectKey, err)
			return false
		}
		if err := writeFileAtomic(coverPath(id), bytes.NewReader(data)); err != nil {
			log.Printf("cover: item %d: write: %v", id, err)
			return false
		}
		return true
	default:
		return false
	}
}

// handleStill serves the cached TMDB episode still (jpeg or 404) written by
// the enrichment stage.
func (s *server) handleStill(w http.ResponseWriter, r *http.Request) {
	s.serveItemImage(w, r, "data/stills", "no still")
}

func (s *server) serveItemImage(w http.ResponseWriter, r *http.Request, dir, missing string) {
	id := r.PathValue("id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	f, err := os.Open(filepath.Join(dir, id+".jpg"))
	if err != nil {
		httpErr(w, 404, fmt.Errorf("%s", missing))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	io.Copy(w, f)
}

func (s *server) handleEnrich(w http.ResponseWriter, r *http.Request) {
	if s.enricher == nil {
		httpErr(w, 503, fmt.Errorf("enrichment disabled (TMDB_API_KEY not set)"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	item, err := s.library.GetItem(id)
	if err != nil || item == nil {
		httpErr(w, 404, fmt.Errorf("no such item"))
		return
	}
	enr, err := s.enricher.EnrichItem(r.Context(), *item)
	if err != nil {
		httpErr(w, 502, err)
		return
	}
	if enr == nil {
		httpErr(w, 404, fmt.Errorf("no TMDB match for item %d", id))
		return
	}
	writeJSON(w, enr)
}

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	names, err := s.state.ListUsers()
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	out := []map[string]string{} // contract: empty array, never null
	for _, n := range names {
		out = append(out, map[string]string{"name": n})
	}
	writeJSON(w, out)
}

func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		httpErr(w, 400, fmt.Errorf("name required"))
		return
	}
	if err := s.state.CreateUser(in.Name); err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]string{"name": in.Name})
}

// handleTelemetry appends any JSON object to data/telemetry.jsonl. Always
// 200 — telemetry must never break a client.
func (s *server) handleTelemetry(w http.ResponseWriter, r *http.Request) {
	defer writeJSON(w, map[string]string{"status": "ok"})
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		log.Printf("telemetry: unparseable body (%d bytes)", len(body))
		return
	}
	line, _ := json.Marshal(payload)
	if err := appendLine("data/telemetry.jsonl", line); err != nil {
		log.Printf("telemetry: write: %v", err)
		return
	}
	log.Printf("telemetry: %s", telemetrySummary(payload))
}

func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// telemetrySummary is the one-line log form of a telemetry payload.
func telemetrySummary(p map[string]any) string {
	if ev, ok := p["event"].(string); ok {
		return fmt.Sprintf("event=%s keys=%d", ev, len(p))
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "keys=" + strings.Join(keys, ",")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// lanBaseURL finds this machine's LAN-facing address by asking the kernel
// which interface routes toward the storage endpoint — no traffic is sent.
func lanBaseURL(storageEndpoint, listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		port = "8099"
	}
	conn, err := net.Dial("udp", storageEndpoint)
	if err != nil {
		return "http://localhost:" + port
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP.String()
	return "http://" + net.JoinHostPort(ip, port)
}

func readTail(path string, n int64) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if int64(len(b)) > n {
		b = b[int64(len(b))-n:]
	}
	return strings.TrimSpace(string(b))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv sets vars from a KEY=VALUE file without overriding real env.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && os.Getenv(k) == "" {
			os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
}
