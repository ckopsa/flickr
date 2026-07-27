// Command server is the PoC media server: library API, playback decision
// endpoint (with trace), HLS streaming, and playback-state tracking.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
)

type server struct {
	library  *store.Library
	state    *store.State
	scanner  *scanner.Scanner
	sessions *pipeline.SessionManager
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items", srv.handleListItems)
	mux.HandleFunc("POST /api/items/{id}/decision", srv.handleDecision)
	mux.HandleFunc("POST /api/items/{id}/play", srv.handlePlay)
	mux.HandleFunc("POST /api/items/{id}/identity", srv.handleOverrideIdentity)
	mux.HandleFunc("POST /api/items/{id}/reprobe", srv.handleReprobe)
	mux.HandleFunc("POST /api/scan", srv.handleScan)
	mux.HandleFunc("GET /api/scan", srv.handleScanStatus)
	mux.HandleFunc("GET /api/system", srv.handleSystem)
	mux.HandleFunc("DELETE /api/sessions/{id}", srv.handleStopSession)
	mux.HandleFunc("POST /api/progress", srv.handleSetProgress)
	mux.HandleFunc("GET /api/progress", srv.handleGetProgress)
	// Log stream fetches: which client asked for which segment with what
	// Range — a poor man's receiver-side network tab.
	streamFiles := http.StripPrefix("/streams/", http.FileServer(http.Dir("data/streams")))
	mux.Handle("GET /streams/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	go func() {
		if err := s.scanner.Scan(context.Background()); err != nil {
			log.Printf("scan error: %v", err)
		}
	}()
	writeJSON(w, map[string]string{"status": "started"})
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
