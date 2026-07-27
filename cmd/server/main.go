// Command server is the PoC media server: library API, playback decision
// endpoint (with trace), HLS streaming, and playback-state tracking.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"flickr/internal/decision"
	"flickr/internal/model"
	"flickr/internal/pipeline"
	"flickr/internal/scanner"
	"flickr/internal/store"
	"flickr/internal/tmdb"
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
}

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
	log.Printf("advertising as %s (cast devices fetch streams here)", srv.baseURL)

	if key := os.Getenv("TMDB_API_KEY"); key != "" {
		srv.enricher = &tmdb.Enricher{
			Client: tmdb.NewHTTPClient(key), Library: library, PostersDir: "data/posters",
		}
	} else {
		log.Printf("TMDB enrichment disabled (TMDB_API_KEY not set)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", srv.handleListItems)
	mux.HandleFunc("POST /api/items/{id}/decision", srv.handleDecision)
	mux.HandleFunc("POST /api/items/{id}/play", srv.handlePlay)
	mux.HandleFunc("POST /api/items/{id}/identity", srv.handleOverrideIdentity)
	mux.HandleFunc("POST /api/items/{id}/reprobe", srv.handleReprobe)
	mux.HandleFunc("POST /api/items/{id}/enrich", srv.handleEnrich)
	mux.HandleFunc("GET /api/items/{id}/subtitles/{file}", srv.handleSubtitle)
	mux.HandleFunc("GET /api/items/{id}/poster", srv.handlePoster)
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

// decisionInput is the request body for decision/play: the client's
// capability manifest plus optional playback parameters.
type decisionInput struct {
	Capabilities model.ClientCapabilities `json:"capabilities"`
	ClientID     string                   `json:"client_id"`
	SeekSeconds  float64                  `json:"seek_seconds"`
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
	var in decisionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, err)
		return nil, nil, nil, false
	}
	d := decision.Decide(*item.MediaInfo, in.Capabilities, s.policy)
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
		if in.SeekSeconds > 0 && d.Target.AudioCodec != "" && d.Target.VideoCodec == "" {
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
		resp["url"] = "/streams/" + sess.ID + "/index.m3u8"
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

// runScan performs one scan and then the TMDB enrichment pass (if enabled).
// Used by the manual endpoint, the startup scan, and the interval scheduler.
func (s *server) runScan(ctx context.Context) {
	if err := s.scanner.Scan(ctx); err != nil {
		log.Printf("scan error: %v", err)
		return
	}
	if st := s.scanner.Status(); st.IdentityRefreshed > 0 {
		log.Printf("scan: refreshed identity for %d items without re-probing (identity v%d)",
			st.IdentityRefreshed, scanner.IdentityVersion)
	}
	if s.enricher == nil {
		return
	}
	n, err := s.enricher.EnrichAll(ctx)
	if err != nil {
		log.Printf("enrichment error: %v", err)
	} else if n > 0 {
		log.Printf("enriched %d items from TMDB", n)
	}
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

func (s *server) handleSetProgress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ItemID   int64   `json:"item_id"`
		ClientID string  `json:"client_id"`
		Position float64 `json:"position_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, err)
		return
	}
	if err := s.state.SetPosition(in.ItemID, in.ClientID, in.Position); err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *server) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	itemID, _ := strconv.ParseInt(r.URL.Query().Get("item_id"), 10, 64)
	clientID := r.URL.Query().Get("client_id")
	pos, err := s.state.GetPosition(itemID, clientID)
	if err != nil {
		httpErr(w, 500, err)
		return
	}
	writeJSON(w, map[string]float64{"position_seconds": pos})
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
		u, err := s.s3.PresignedGetObject(r.Context(), s.bucket, item.ObjectKey, time.Hour, url.Values{})
		if err != nil {
			httpErr(w, 500, err)
			return
		}
		if err := pipeline.ExtractSubtitle(r.Context(), u.String(), ordinal, cachePath); err != nil {
			httpErr(w, 500, err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/vtt")
	http.ServeFile(w, r, cachePath)
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

func (s *server) handlePoster(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		httpErr(w, 400, fmt.Errorf("bad id"))
		return
	}
	f, err := os.Open(filepath.Join("data/posters", id+".jpg"))
	if err != nil {
		httpErr(w, 404, fmt.Errorf("no poster"))
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
