package main

// A Keycloak, small enough to keep in a test: discovery, a JWKS, an
// authorize endpoint that mints a code against the challenge it was handed,
// and a token endpoint that will not trade that code for anything unless the
// verifier hashes back to it.
//
// It lives in its own file with a small API — newFakeIssuer, Visit, Bearer,
// LastAuthorize — so the browser smoke can stand one up later without
// borrowing anything from the walk in auth_test.go.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeKid = "fake-key-1"

type fakeIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	// ClientID and ClientSecret are what the token endpoint insists on.
	ClientID     string
	ClientSecret string

	mu        sync.Mutex
	authorize url.Values        // the parameters the last /authorize saw
	codes     map[string]string // code → the challenge and nonce it was minted against
	nonces    map[string]string
}

// URL is the issuer, which is also this server's origin.
func (f *fakeIssuer) URL() string { return f.srv.URL }

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{
		t: t, key: key,
		ClientID: "flickr", ClientSecret: "s3cret",
		codes: map[string]string{}, nonces: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("GET /jwks", f.handleJWKS)
	mux.HandleFunc("GET /authorize", f.handleAuthorize)
	mux.HandleFunc("POST /token", f.handleToken)
	mux.HandleFunc("GET /logout", f.handleLogout)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"issuer":                 f.URL(),
		"authorization_endpoint": f.URL() + "/authorize",
		"token_endpoint":         f.URL() + "/token",
		"jwks_uri":               f.URL() + "/jwks",
		"end_session_endpoint":   f.URL() + "/logout",
		"userinfo_endpoint":      f.URL() + "/userinfo",
	})
}

func (f *fakeIssuer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": fakeKid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(f.key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}}})
}

// handleAuthorize is the sign-in page a person would see, minus the person:
// it remembers the challenge and the nonce, and sends the browser back to
// the redirect_uri with a code.
//
// A REAL browser — the smoke's Chromium, which asks for HTML — is given the
// page instead, with one button on it. An immediate redirect would be a door
// nobody can be seen walking through: the smoke needs somewhere to stand and
// something to press. Anything else, the unit test's client included, sends
// no Accept and gets the redirect it always got.
func (f *fakeIssuer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.authorize = q
	code := "code-" + strconv.Itoa(len(f.codes)+1)
	f.codes[code] = q.Get("code_challenge")
	f.nonces[code] = q.Get("nonce")
	f.mu.Unlock()

	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := back.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	back.RawQuery = rq.Encode()
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Sign in</title></head>
<body style="font:16px sans-serif;padding:40px">
<h1>The household account</h1>
<a id="approve" href="%s">Sign in as Chris</a>
</body></html>`, html.EscapeString(back.String()))
		return
	}
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// handleLogout is the end-session endpoint: the SSO session goes (there is
// none to keep here), and the browser goes back where the caller asked, as
// Keycloak's does. With nowhere named it is a blank page — which is what
// flickr's own logout works around by sending the browser home itself.
func (f *fakeIssuer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if back := r.URL.Query().Get("post_logout_redirect_uri"); back != "" {
		http.Redirect(w, r, back, http.StatusFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleToken trades a code for an id_token, and refuses everything a real
// one refuses: another client, another secret, a code it never minted, and a
// verifier that does not hash to the challenge it saw.
func (f *fakeIssuer) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if got := r.Form.Get("grant_type"); got != "authorization_code" {
		http.Error(w, "grant_type "+got, http.StatusBadRequest)
		return
	}
	if r.Form.Get("client_id") != f.ClientID || r.Form.Get("client_secret") != f.ClientSecret {
		http.Error(w, "bad client credentials", http.StatusUnauthorized)
		return
	}
	code := r.Form.Get("code")
	f.mu.Lock()
	challenge, known := f.codes[code]
	nonce := f.nonces[code]
	delete(f.codes, code) // a code is good once
	f.mu.Unlock()
	if !known {
		http.Error(w, "unknown code", http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		http.Error(w, "code_verifier does not match the challenge", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"token_type": "Bearer", "expires_in": 300,
		"access_token": "opaque-access-token",
		"id_token": f.mint(map[string]any{
			"aud": f.ClientID, "azp": f.ClientID, "sub": "u-chris",
			"name": "Chris", "preferred_username": "chris", "email": "chris@example",
			"nonce": nonce,
		}),
	})
}

// Bearer is one token for an audience — how an agent or the MCP surface
// speaks to flickr.
func (f *fakeIssuer) Bearer(audience, subject string) string {
	return f.mint(map[string]any{
		"aud": audience, "azp": audience, "sub": subject,
		"preferred_username": subject,
	})
}

// mint signs claims with the issuer, expiry and key id filled in.
func (f *fakeIssuer) mint(claims map[string]any) string {
	f.t.Helper()
	now := time.Now()
	claims["iss"] = f.URL()
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(time.Hour).Unix()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": fakeKid, "typ": "JWT"})
	if err != nil {
		f.t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// LastAuthorize is the query the last /authorize was asked with: the
// challenge, the state and the nonce flickr sent.
func (f *fakeIssuer) LastAuthorize() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorize
}

// Visit is the browser's half of the redirect: it fetches the authorization
// URL flickr sent and answers the callback query that comes back — the code
// and the state, without following the callback itself.
func (f *fakeIssuer) Visit(location string) url.Values {
	f.t.Helper()
	if !strings.HasPrefix(location, f.URL()+"/authorize") {
		f.t.Fatalf("login sent the browser to %q, want the issuer's authorize endpoint", location)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(location)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		f.t.Fatalf("authorize answered %s, want a redirect back", resp.Status)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	return back.Query()
}
