package main

// Device capabilities: a signed token in the PATH.
//
// A Chromecast holds no cookie. With the gate on (auth.go), everything the
// receiver fetches — the HLS playlist and its segments, the session document
// it is handed in the load's customData, the picture it draws behind the
// title, the telemetry it posts back — would be 401, and there is nowhere on
// a cast load to put an Authorization header.
//
// So the capability travels in the address: /d/{token}/<the plain path>. In
// the PATH rather than the query because an HLS playlist names its segments
// RELATIVELY, and a player resolves them against the playlist's own URL — a
// prefix is inherited by every segment for free, where a query string is
// dropped at the first hop.
//
// The token says three things and nothing else: which session, which item,
// and until when. What it may reach is not written into it — deviceCovers is
// the whole of that, so the shape of the permission lives in one readable
// switch rather than in a claim a token could carry too much of.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"flickr/internal/auth"
	"flickr/internal/hyper"
)

// deviceTokenTTL is how long a minted capability is good for. A day: an HLS
// session outliving one is not a thing (the reaper ends idle ones within
// minutes), and a token that has outlived its session reaches a 404 anyway.
const deviceTokenTTL = 24 * time.Hour

// deviceClaims is what the token carries, in the short names a signed
// payload is written in.
type deviceClaims struct {
	Session string `json:"sid"`
	Item    int64  `json:"item"`
	Expires int64  `json:"exp"`
}

// deviceToken signs one capability. It is auth.Sign — the same HMAC that
// carries the login cookie and the household session — over that small JSON,
// which comes out base64url and a dot, and so is a path segment as it stands.
func deviceToken(secret []byte, sid string, item int64, exp time.Time) string {
	payload, _ := json.Marshal(deviceClaims{Session: sid, Item: item, Expires: exp.Unix()})
	return auth.Sign(secret, payload)
}

// parseDeviceToken reads one back: the signature first, then the clock. A
// touched token and an expired one are the same answer to a caller — no —
// and the difference is not something to tell a stranger.
func parseDeviceToken(secret []byte, token string, now time.Time) (deviceClaims, error) {
	payload, err := auth.Verify(secret, token)
	if err != nil {
		return deviceClaims{}, err
	}
	var c deviceClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return deviceClaims{}, err
	}
	if c.Session == "" {
		return deviceClaims{}, errors.New("device token names no session")
	}
	if c.Expires <= 0 || !now.Before(time.Unix(c.Expires, 0)) {
		return deviceClaims{}, errors.New("device token expired")
	}
	return c, nil
}

// deviceCovers is what one capability reaches: this session's stream and
// document, this item's subtitles and pictures, and the telemetry every
// player posts. Everything else — the library, another session's segments,
// somebody else's poster — is outside it, so a token that leaks is a token
// for one sitting rather than for the house.
func deviceCovers(c deviceClaims, method, path string) bool {
	if strings.HasPrefix(path, "/streams/"+c.Session+"/") {
		return true
	}
	if path == "/api/sessions/"+c.Session ||
		strings.HasPrefix(path, "/api/sessions/"+c.Session+"/") {
		return true
	}
	if c.Item > 0 {
		item := "/api/items/" + strconv.FormatInt(c.Item, 10)
		if strings.HasPrefix(path, item+"/subtitles/") {
			return true
		}
		switch path {
		case item + "/poster", item + "/still", item + "/backdrop", item + "/cover":
			return true
		}
	}
	// Telemetry is the receiver saying what it did with the bytes. It writes
	// nothing anyone can read back, which is why it is the one address here
	// that is not this session's own.
	return method == http.MethodPost && path == "/api/telemetry"
}

// splitDevicePath cuts /d/{token}/<rest> into the two halves, both as they
// were ESCAPED: the mux matches on the escaped path, so the remainder has to
// be handed back to it exactly as it arrived.
func splitDevicePath(escaped string) (token, rest string, ok bool) {
	after, found := strings.CutPrefix(escaped, "/d/")
	if !found {
		return "", "", false
	}
	token, rest, found = strings.Cut(after, "/")
	if !found || token == "" {
		return "", "", false
	}
	return token, "/" + rest, true
}

// deviceRoutes registers the one door. It goes on the mux whether or not
// there is a gate in front of it — the address is the same either way — and
// with sign-in off nothing links it and there is no secret to check a token
// against, so it refuses exactly as the gate would.
// GET (which is HEAD too) and POST are named one at a time rather than the
// door taking every method: a methodless pattern would be more general than
// the shell's own "GET /" and the mux refuses that pair outright. They are
// also the only two a device makes — it reads its stream and its document,
// and it posts what it did with them.
func (s *server) deviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /d/{token}/", s.handleDevice(mux))
	mux.HandleFunc("POST /d/{token}/", s.handleDevice(mux))
}

// handleDevice checks the capability and dispatches the plain path back onto
// the mux with a viewer on the context. The gate lets a request that already
// carries one through (auth.go), so the re-dispatch needs no second opinion —
// and this is the only place a device token is ever read.
func (s *server) handleDevice(mux http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.rp == nil {
			hyper.WriteProblem(w, unauthenticated())
			return
		}
		token, escaped, ok := splitDevicePath(r.URL.EscapedPath())
		if !ok {
			hyper.WriteProblem(w, unauthenticated())
			return
		}
		claims, err := parseDeviceToken(s.rp.cfg.SessionSecret, token, time.Now())
		if err != nil {
			hyper.WriteProblem(w, unauthenticated())
			return
		}
		plain, err := url.PathUnescape(escaped)
		if err != nil {
			hyper.WriteProblem(w, unauthenticated())
			return
		}
		if !deviceCovers(claims, r.Method, plain) {
			hyper.WriteProblem(w, unauthenticated())
			return
		}
		// The device is a viewer of its own: the household's, but neither a
		// person nor a profile — the sitting it was handed is who it is.
		r2 := r.Clone(withViewer(r.Context(), viewer{
			Subject: "device:" + claims.Session, Name: "Cast device"}))
		r2.URL.Path = plain
		r2.URL.RawPath = ""
		if plain != escaped {
			r2.URL.RawPath = escaped
		}
		mux.ServeHTTP(w, r2)
	}
}

// deviceHref is how a document writes one of this session's addresses so a
// device with no cookie can fetch it. With no gate it is the address
// unchanged — the documents, and their goldens, are exactly as they were.
//
// An address that is already absolute (a presigned storage URL) is left
// alone: those bytes never come past this server, so there is no gate on
// them to get through.
func (s *server) deviceHref(row playSession) func(string) string {
	if s.rp == nil {
		return func(href string) string { return href }
	}
	token := deviceToken(s.rp.cfg.SessionSecret, row.ID, row.ItemID, time.Now().Add(deviceTokenTTL))
	return func(href string) string {
		if href == "" || href[0] != '/' {
			return href
		}
		return "/d/" + token + href
	}
}
