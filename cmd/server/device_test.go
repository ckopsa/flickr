package main

// The device capability (device.go): what a Cast device may fetch with no
// cookie, and what it may not.
//
// The arithmetic is table-tested on its own; the walk runs over the fixture
// library with a relying party in front of it — the same fake issuer the
// sign-in walk uses, so no network, and no ffmpeg — and asks the questions a
// receiver asks: the playlist, the session document, the telemetry.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flickr/internal/model"
	"flickr/internal/pipeline"
)

var deviceSecret = []byte("a household session secret, 32+ bytes")

func TestDeviceTokenRoundTrip(t *testing.T) {
	now := time.Date(2026, time.September, 8, 20, 0, 0, 0, time.UTC)
	good := deviceToken(deviceSecret, "a1c9f2", 8, now.Add(deviceTokenTTL))

	claims, err := parseDeviceToken(deviceSecret, good, now)
	if err != nil {
		t.Fatalf("a token this server minted: %v", err)
	}
	if claims.Session != "a1c9f2" || claims.Item != 8 {
		t.Fatalf("claims = %+v", claims)
	}

	for _, tc := range []struct {
		name   string
		secret []byte
		token  string
		now    time.Time
	}{
		{"one character changed", deviceSecret, tamper(good), now},
		{"another household's secret", []byte("a different household secret 32"), good, now},
		{"expired", deviceSecret, deviceToken(deviceSecret, "a1c9f2", 8, now.Add(-time.Second)), now},
		{"a day and a second later", deviceSecret, good, now.Add(deviceTokenTTL + time.Second)},
		{"naming no session", deviceSecret, deviceToken(deviceSecret, "", 8, now.Add(time.Hour)), now},
		{"not a token at all", deviceSecret, "hello", now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDeviceToken(tc.secret, tc.token, tc.now); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// tamper changes one character of the MAC — the smallest forgery there is.
// The FIRST character of it rather than the last: base64url spells a 32-byte
// digest in 43 characters, two bits of which nothing uses, so the last
// character has spellings that decode to the very same MAC.
func tamper(token string) string {
	i := strings.LastIndex(token, ".") + 1
	c := "A"
	if token[i] == 'A' {
		c = "B"
	}
	return token[:i] + c + token[i+1:]
}

func TestDeviceCovers(t *testing.T) {
	c := deviceClaims{Session: "a1c9f2", Item: 8}
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/streams/a1c9f2/master.m3u8", true},
		{"GET", "/streams/a1c9f2/v0/seg-00001.m4s", true},
		{"GET", "/streams/b2b2b2/master.m3u8", false},
		{"GET", "/streams/a1c9f2extra/master.m3u8", false},
		{"GET", "/api/sessions/a1c9f2", true},
		{"POST", "/api/sessions/a1c9f2/progress", true},
		{"POST", "/api/sessions/b2b2b2/progress", false},
		{"GET", "/api/items/8/subtitles/2.vtt", true},
		{"GET", "/api/items/8/still", true},
		{"GET", "/api/items/8/poster", true},
		{"GET", "/api/items/8/backdrop", true},
		{"GET", "/api/items/8/cover", true},
		{"GET", "/api/items/8", false},
		{"GET", "/api/items/8/book", false},
		{"GET", "/api/items/9/still", false},
		{"POST", "/api/telemetry", true},
		{"GET", "/api/telemetry", false},
		{"GET", "/api/library", false},
		{"GET", "/api/", false},
		{"GET", "/", false},
	} {
		if got := deviceCovers(c, tc.method, tc.path); got != tc.want {
			t.Errorf("deviceCovers(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestSplitDevicePath(t *testing.T) {
	for _, tc := range []struct{ in, token, rest string }{
		{"/d/tok.mac/api/sessions/a1", "tok.mac", "/api/sessions/a1"},
		{"/d/tok.mac/streams/a1/master.m3u8", "tok.mac", "/streams/a1/master.m3u8"},
		{"/d/tok.mac/", "tok.mac", "/"},
		{"/d/tok.mac", "", ""},
		{"/d//api/library", "", ""},
		{"/api/library", "", ""},
	} {
		token, rest, ok := splitDevicePath(tc.in)
		if tc.token == "" {
			if ok {
				t.Errorf("splitDevicePath(%q) = %q, %q, want refused", tc.in, token, rest)
			}
			continue
		}
		if !ok || token != tc.token || rest != tc.rest {
			t.Errorf("splitDevicePath(%q) = %q, %q, %v", tc.in, token, rest, ok)
		}
	}
}

// ── the walk ────────────────────────────────────────────────────────────

// deviceFixture is the fixture library behind the gate, with the storage
// presigner stubbed the way fixturePlayer stubs it: a play needs a URL to
// hand back, not bytes. The working directory moves to a temporary one
// because the stream files, and the telemetry line, are written under it.
func deviceFixture(t *testing.T) (*server, http.Handler, *fakeIssuer) {
	t.Helper()
	t.Chdir(t.TempDir())
	iss := newFakeIssuer(t)
	srv, h := authFixture(t, iss, nil)
	srv.presign = func(_ context.Context, objectKey string) (string, error) {
		return "https://storage.test/" + objectKey + "?signed", nil
	}
	srv.sessions = pipeline.NewSessionManager(t.TempDir(), nil)
	return srv, h, iss
}

// signedIn is the header a household member carries. A bearer is the cheap
// half of the sign-in — no browser, no cookie jar — and the gate reads it
// through the same viewerOf a cookie goes through.
func signedIn(t *testing.T, iss *fakeIssuer) http.Header {
	t.Helper()
	return http.Header{"Authorization": {"Bearer " + iss.Bearer(iss.ClientID, "chris")}}
}

// postAs is one POST with a body and whatever the caller carries.
func postAs(t *testing.T, h http.Handler, target string, body any, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, target, &buf)
	r.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// tokenIn is the capability out of one of the document's own addresses.
func tokenIn(t *testing.T, href string) string {
	t.Helper()
	token, _, ok := splitDevicePath(href)
	if !ok {
		t.Fatalf("%q carries no device capability", href)
	}
	return token
}

// The whole of it: a signed-in member starts a play, the document's
// device-facing addresses carry the capability, and a device holding nothing
// else fetches them.
func TestDeviceCapabilityWalk(t *testing.T) {
	srv, h, iss := deviceFixture(t)
	member := signedIn(t, iss)

	// ── a play, signed in ───────────────────────────────────────────────
	w := postAs(t, h, fmt.Sprintf("/api/items/%d/play", idBeach), map[string]any{
		"capabilities": directPlayCaps(), "client_id": "chris",
	}, member)
	if w.Code != http.StatusOK {
		t.Fatalf("POST play = %d: %s", w.Code, w.Body)
	}
	doc := decode(t, w)

	self, _ := doc["self"].(string)
	if !strings.HasPrefix(self, "/d/") {
		t.Fatalf("self = %q, want the device capability in the path", self)
	}
	if art := href(t, doc, "links", "artwork"); !strings.HasPrefix(art, "/d/") {
		t.Errorf("artwork = %q: the device draws it and holds no cookie", art)
	}
	if tel := href(t, doc, "links", "telemetry"); !strings.HasPrefix(tel, "/d/") {
		t.Errorf("telemetry = %q", tel)
	}
	// A presigned storage URL is already absolute and never comes past this
	// server: there is no gate on it to get through.
	if u, _ := doc["url"].(string); !strings.HasPrefix(u, "https://storage.test/") {
		t.Errorf("url = %q, want the presigned URL untouched", u)
	}
	// The item stays plain: it is the SENDER's link, and the browser follows
	// it with the household's own credential.
	if it := href(t, doc, "links", "item"); strings.HasPrefix(it, "/d/") {
		t.Errorf("links.item = %q: a device has no use for the item document", it)
	}

	// ── the document itself, fetched with nothing at all ────────────────
	got := ask(t, h, "GET", self, nil, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("GET %s with no credential = %d: %s", self, got.Code, got.Body)
	}
	sid, _ := doc["id"].(string)
	if again := decode(t, got); again["id"] != sid {
		t.Errorf("the tokened address answered session %v, want %s", again["id"], sid)
	}
	// The same address without the prefix is the gate's business again.
	if bare := ask(t, h, "GET", "/api/sessions/"+sid, nil, nil); bare.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/sessions/%s with no credential = %d, want 401", sid, bare.Code)
	}
	token := tokenIn(t, self)

	// ── the stream ──────────────────────────────────────────────────────
	// A transcode's playlist is this server's own file. The row is put in
	// hand rather than played, because starting one needs ffmpeg.
	srv.plays.put(playSession{
		ID: "castsess", ItemID: idBeach, ClientID: "chris",
		Method: string(model.Transcode), URL: "/streams/castsess/master.m3u8",
		StartedAt: time.Now(),
	})
	playlist := "#EXTM3U\n#EXT-X-VERSION:6\nv0/index.m3u8\n"
	if err := os.MkdirAll(filepath.Join("data", "streams", "castsess"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("data", "streams", "castsess", "master.m3u8"),
		[]byte(playlist), 0o644); err != nil {
		t.Fatal(err)
	}
	sdoc := decode(t, ask(t, h, "GET", "/api/sessions/castsess", nil, member))
	streamHref, _ := sdoc["url"].(string)
	if !strings.HasPrefix(streamHref, "/d/") {
		t.Fatalf("the stream a cast device is loaded with = %q", streamHref)
	}
	pl := ask(t, h, "GET", streamHref, nil, nil)
	if pl.Code != http.StatusOK || pl.Body.String() != playlist {
		t.Fatalf("GET %s = %d: %q", streamHref, pl.Code, pl.Body)
	}
	streamToken := tokenIn(t, streamHref)

	// ── and what a capability does NOT reach ────────────────────────────
	for _, tc := range []struct{ name, target string }{
		{"a touched token", "/d/" + tamper(token) + "/api/sessions/" + sid},
		{"an expired one", "/d/" +
			deviceToken(srv.rp.cfg.SessionSecret, sid, idBeach, time.Now().Add(-time.Minute)) +
			"/api/sessions/" + sid},
		{"another session's stream", "/d/" + streamToken + "/streams/notmine/master.m3u8"},
		{"the library", "/d/" + token + "/api/library"},
		{"another item's picture", "/d/" + token + "/api/items/1/poster"},
		{"telemetry as a read", "/d/" + token + "/api/telemetry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ask(t, h, "GET", tc.target, nil, nil)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s = %d, want 401: %s", tc.target, w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("content-type %q, want a problem", ct)
			}
		})
	}

	// ── the telemetry the receiver posts back ───────────────────────────
	tel := href(t, sdoc, "links", "telemetry")
	post := postAs(t, h, tel, map[string]any{
		"source": "cast-receiver", "event": "state_change", "state": "PLAYING",
	}, nil)
	if post.Code < 200 || post.Code > 299 {
		t.Fatalf("POST %s = %d: %s", tel, post.Code, post.Body)
	}
	if _, err := os.Stat(filepath.Join("data", "telemetry.jsonl")); err != nil {
		t.Fatalf("the receiver's line never landed: %v", err)
	}
}

// With no issuer there is no relying party and no capability to mint: every
// address in the document is the plain one. The goldens pin this already —
// testdata/hyper/session-play.json is written by a fixture server with no
// gate, so a "/d/" would fail TestPlaySessionGolden — and this says it out
// loud rather than by absence.
func TestSessionDocumentIsPlainWithoutAGate(t *testing.T) {
	_, h := fixturePlayer(t)
	doc := play(t, h, idBeach, nil)
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("/d/")) {
		t.Errorf("a document with no gate in front of it carries a capability: %s", body)
	}
	if has(doc, "links", "telemetry") {
		t.Error("links.telemetry is for a device that cannot get through a gate; there is none")
	}
}
