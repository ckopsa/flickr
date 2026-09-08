package main

// The relying party: flickr behind Keycloak, served publicly.
//
// internal/auth holds the arithmetic — PKCE, the signed cookie, an RS256
// token checked against the issuer's JWKS. This file is the conversation
// that uses it: four open doors under /auth/, the gate that stands in front
// of everything else, and the viewer it puts in the request context.
//
// It is OFF unless OIDC_ISSUER is set. A household running flickr on its own
// LAN keeps every route exactly as it was, and so does every test and every
// golden: with no issuer there is no relying party, no /auth/ route and no
// gate to wrap the mux in.
//
// A viewer is the HOUSEHOLD, not a profile. flickr's profiles (the client_id
// cookie, profileOf) are who is watching in the living room; the sign-in is
// which household the box belongs to. One signs in, the other picks a face.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"flickr/internal/auth"
	"flickr/internal/hyper"
)

const (
	// authCookie carries the ten minutes between /auth/login and the
	// callback: the PKCE verifier, the state, the nonce and where to go
	// back to. It is signed rather than stored, so a restart mid-login is
	// not a failed login.
	authCookie = "flickr_auth"
	// sessionCookie is the household session itself.
	sessionCookie = "flickr_session"

	loginWindow    = 10 * time.Minute
	sessionWindow  = 30 * 24 * time.Hour
	jwksMinRefetch = time.Minute
)

// authConfig is the relying party's environment, read once in main().
type authConfig struct {
	Issuer          string   // OIDC_ISSUER — "" is sign-in switched off
	ClientID        string   // OIDC_CLIENT_ID
	ClientSecret    string   // OIDC_CLIENT_SECRET
	AppURL          string   // OIDC_APP_URL: the PUBLIC origin the callback is under
	SessionSecret   []byte   // OIDC_SESSION_SECRET
	RequireAuth     bool     // OIDC_REQUIRE_AUTH — "0" offers sign-in and gates nothing
	DelegateClients []string // OIDC_DELEGATE_CLIENTS: other audiences a bearer may carry
}

// authConfigFromEnv reads that environment. It answers a nil config when
// OIDC_ISSUER is unset — sign-in off — and an error when the issuer IS set
// and something else is missing: a half-configured gate is worse than none,
// so main() refuses to start rather than serving the library to the world.
func authConfigFromEnv(appURLFallback string) (*authConfig, error) {
	issuer := strings.TrimSpace(os.Getenv("OIDC_ISSUER"))
	if issuer == "" {
		return nil, nil
	}
	cfg := &authConfig{
		Issuer:       strings.TrimRight(issuer, "/"),
		ClientID:     os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		// The callback is an address Keycloak redirects a BROWSER to, so it
		// is the public origin, not the LAN one a cast device fetches from —
		// they are the same string on the household's box, which is why
		// ADVERTISE_URL is the fallback.
		AppURL:        strings.TrimRight(envOr("OIDC_APP_URL", appURLFallback), "/"),
		SessionSecret: []byte(os.Getenv("OIDC_SESSION_SECRET")),
		RequireAuth:   os.Getenv("OIDC_REQUIRE_AUTH") != "0",
	}
	for _, c := range strings.Split(os.Getenv("OIDC_DELEGATE_CLIENTS"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			cfg.DelegateClients = append(cfg.DelegateClients, c)
		}
	}
	if cfg.ClientID == "" {
		return nil, errors.New("OIDC_CLIENT_ID must be set when OIDC_ISSUER is")
	}
	if len(cfg.SessionSecret) < 32 {
		return nil, errors.New("OIDC_SESSION_SECRET must be set to at least 32 bytes when OIDC_ISSUER is")
	}
	if cfg.AppURL == "" {
		return nil, errors.New("OIDC_APP_URL (or ADVERTISE_URL) must be set when OIDC_ISSUER is")
	}
	return cfg, nil
}

// relyingParty is the runtime: the config, what discovery said, and the
// issuer's signing keys as they stand.
type relyingParty struct {
	cfg      authConfig
	provider auth.Provider
	client   *http.Client

	mu      sync.Mutex
	keys    auth.JWKS
	fetched time.Time
}

// newRelyingParty fetches the discovery document once, at boot. It is the
// one thing here that must not fail quietly: a box that cannot reach its
// issuer cannot check a single token, and starting anyway would either lock
// the household out or let everybody in.
func newRelyingParty(ctx context.Context, cfg authConfig, client *http.Client) (*relyingParty, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	p, err := auth.Discover(ctx, client, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	return &relyingParty{cfg: cfg, provider: p, client: client}, nil
}

// ── the viewer ──────────────────────────────────────────────────────────

// viewer is the household member a request is signed in as: the provider's
// stable subject and the name a screen greets them by.
type viewer struct {
	Subject string
	Name    string
	// Bearer is true when they arrived with a token rather than the session
	// cookie — an agent or the MCP surface rather than a browser.
	Bearer bool
}

type viewerKeyType struct{}

var viewerKey viewerKeyType

// withViewer puts one on a request's context; viewerFrom reads it back.
// Handlers ask, they never resolve: the gate is the only thing that checks
// a signature.
func withViewer(ctx context.Context, v viewer) context.Context {
	return context.WithValue(ctx, viewerKey, v)
}

func viewerFrom(ctx context.Context) (viewer, bool) {
	v, ok := ctx.Value(viewerKey).(viewer)
	return v, ok
}

// ── the routes ──────────────────────────────────────────────────────────

// authRoutes registers the four open doors. It is called from routes() only
// when there is a relying party, so with sign-in off /auth/login is a 404
// like any other address flickr does not have.
func (s *server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", s.rp.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.rp.handleCallback)
	mux.HandleFunc("GET /auth/logout", s.rp.handleLogout)
	mux.HandleFunc("GET /auth/me", s.rp.handleMe)
}

// guarded wraps the mux in the sign-in gate, or hands it back untouched when
// there is no relying party. main() and the tests both go through here so
// the served handler and the tested one cannot drift.
func (s *server) guarded(mux http.Handler) http.Handler {
	if s.rp == nil {
		return mux
	}
	return s.rp.wrap(mux)
}

// handleLogin is GET /auth/login?return_to=<path> — the door every refusal's
// remedy points at. It mints the PKCE verifier, the state and the nonce,
// signs them into a ten-minute cookie and sends the browser to Keycloak.
func (rp *relyingParty) handleLogin(w http.ResponseWriter, r *http.Request) {
	pending := pendingLogin{
		Verifier: auth.NewVerifier(),
		State:    auth.NewState(),
		Nonce:    auth.NewState(),
		ReturnTo: safeReturnTo(r.URL.Query().Get("return_to")),
		Expires:  time.Now().Add(loginWindow).Unix(),
	}
	payload, err := json.Marshal(pending)
	if err != nil {
		hyper.WriteProblem(w, serverProblem(err))
		return
	}
	rp.setCookie(w, authCookie, auth.Sign(rp.cfg.SessionSecret, payload), loginWindow)

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {rp.cfg.ClientID},
		"redirect_uri":          {rp.redirectURI()},
		"scope":                 {"openid profile email"},
		"state":                 {pending.State},
		"nonce":                 {pending.Nonce},
		"code_challenge":        {auth.Challenge(pending.Verifier)},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, rp.provider.AuthorizationEndpoint+"?"+q.Encode(), http.StatusFound)
}

// pendingLogin is what the ten-minute cookie carries between the two halves
// of a login. It never reaches the provider and never leaves this server
// unsigned.
type pendingLogin struct {
	Verifier string `json:"v"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	ReturnTo string `json:"r"`
	Expires  int64  `json:"exp"`
}

// handleCallback is GET /auth/callback?code&state — the other half. Every
// failure here is a problem with the login door as its remedy: the usual
// cause is a cookie that expired while somebody made coffee, and the answer
// is to start again rather than to read a stack trace.
func (rp *relyingParty) handleCallback(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, typ, detail string) {
		rp.clearCookie(w, authCookie)
		hyper.WriteProblem(w, hyper.Refuse(status, typ, "That sign-in could not be finished", detail).
			WithRemedy("Start the sign-in again.", &hyper.Link{Href: "/auth/login", Title: "Sign in"}))
	}
	c, err := r.Cookie(authCookie)
	if err != nil {
		refuse(http.StatusBadRequest, "no-login-in-progress",
			"this browser is not part-way through a sign-in, or it took longer than ten minutes")
		return
	}
	payload, err := auth.Verify(rp.cfg.SessionSecret, c.Value)
	if err != nil {
		refuse(http.StatusBadRequest, "bad-login-state", "the login cookie was not signed by this server")
		return
	}
	var pending pendingLogin
	if err := json.Unmarshal(payload, &pending); err != nil {
		refuse(http.StatusBadRequest, "bad-login-state", "the login cookie is unreadable")
		return
	}
	if time.Now().Unix() > pending.Expires {
		refuse(http.StatusBadRequest, "login-expired", "that sign-in took longer than ten minutes")
		return
	}
	// The state is the whole of the CSRF check: a callback that did not come
	// from the browser that started this login carries somebody else's.
	if got := r.URL.Query().Get("state"); got == "" || got != pending.State {
		refuse(http.StatusBadRequest, "bad-login-state", "the callback's state is not this browser's")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		refuse(http.StatusForbidden, "provider-refused", "the identity provider answered "+e)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		refuse(http.StatusBadRequest, "no-code", "the callback carries no authorization code")
		return
	}
	idToken, err := rp.exchange(r.Context(), code, pending.Verifier)
	if err != nil {
		refuse(http.StatusBadGateway, "token-exchange-failed", err.Error())
		return
	}
	claims, err := rp.verifyToken(r.Context(), idToken, auth.Expect{
		Issuer: rp.cfg.Issuer, Audiences: []string{rp.cfg.ClientID}, Nonce: pending.Nonce,
	})
	if err != nil {
		refuse(http.StatusForbidden, "bad-id-token", err.Error())
		return
	}

	rp.clearCookie(w, authCookie)
	now := time.Now()
	rp.setCookie(w, sessionCookie, auth.EncodeSession(rp.cfg.SessionSecret, auth.SessionClaims{
		Subject: claims.Subject, Name: displayName(claims),
		IssuedAt: now, ExpiresAt: now.Add(sessionWindow),
	}), sessionWindow)
	http.Redirect(w, r, pending.ReturnTo, http.StatusFound)
}

// handleLogout is GET /auth/logout: the cookie goes first — whatever the
// provider does with the request after it, this box is signed out — and then
// the browser is sent to the provider's own end-session page so the SSO
// session goes too. A provider with none is a plain trip home.
func (rp *relyingParty) handleLogout(w http.ResponseWriter, r *http.Request) {
	rp.clearCookie(w, sessionCookie)
	rp.clearCookie(w, authCookie)
	if rp.provider.EndSessionEndpoint == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := url.Values{
		"post_logout_redirect_uri": {rp.cfg.AppURL + "/"},
		"client_id":                {rp.cfg.ClientID},
	}
	http.Redirect(w, r, rp.provider.EndSessionEndpoint+"?"+q.Encode(), http.StatusFound)
}

// handleMe is GET /auth/me — the viewer as a document, so `curl -b` can ask
// "who does this box think I am" without reading a shelf.
func (rp *relyingParty) handleMe(w http.ResponseWriter, r *http.Request) {
	v, ok := viewerFrom(r.Context())
	if !ok {
		v, ok = rp.viewerOf(r)
	}
	if !ok {
		hyper.WriteProblem(w, unauthenticated())
		return
	}
	hyper.WriteDoc(w, http.StatusOK, hyper.Doc("/auth/me", "viewer", v.Name).
		Field("subject", v.Subject).
		Field("name", v.Name).
		Link("root", "/api/", "flickr").
		Link("logout", "/auth/logout", "Sign out"))
}

// ── the gate ────────────────────────────────────────────────────────────

// wrap is the gate. It resolves the viewer on EVERY request — the root
// document says who is signed in whether or not anything is gated — and
// refuses only what gatedPath names, and only when OIDC_REQUIRE_AUTH is on.
func (rp *relyingParty) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A viewer already on the context is one a device-authenticated
		// request carried in when it was re-dispatched through the mux: it
		// has been checked once already, and checking it again would mean
		// minting a cookie for it.
		if _, ok := viewerFrom(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		if v, ok := rp.viewerOf(r); ok {
			next.ServeHTTP(w, r.WithContext(withViewer(r.Context(), v)))
			return
		}
		// CORS preflight carries no cookie by definition, so gating it would
		// refuse the request that asks whether the real one may be made.
		if !rp.cfg.RequireAuth || r.Method == http.MethodOptions || !gatedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// A share link is a NAVIGATION: somebody tapped it in a chat window,
		// and a problem document is not something a browser can show them.
		if strings.HasPrefix(r.URL.Path, "/s/") {
			// Path AND query: a share link's passage is in the query, and
			// coming back without it lands somewhere else.
			http.Redirect(w, r, "/auth/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		hyper.WriteProblem(w, unauthenticated())
	})
}

// unauthenticated is the one refusal the gate makes.
//
// The remedy's href carries NO return_to on purpose: the client appends the
// page it is on, and that page is a HASH — "#/item/2090?t=142" — which never
// reaches this server, so it is the only side that can say where to come
// back to.
func unauthenticated() hyper.Problem {
	return hyper.Refuse(http.StatusUnauthorized, "unauthenticated",
		"Sign in to use flickr",
		"This address answers a signed-in household member.").
		WithRemedy("Sign in with the household account.",
			&hyper.Link{Href: "/auth/login", Title: "Sign in"})
}

// gatedPath is what the household's sign-in stands in front of: the
// documents, the streams and the share pages. Everything else is the shell —
// index.html, its scripts, receiver.html, sw.js, the manifest and the icons —
// which are public files that reveal nothing, and which have to load for
// there to be anything to sign in with.
func gatedPath(p string) bool {
	// /api/system is the Nomad health check: the orchestrator has no cookie
	// and no token, and a job that cannot answer "am I up" is restarted for
	// ever.
	if p == "/api/system" {
		return false
	}
	// The doors themselves. A gate in front of the sign-in is a locked room
	// with the key inside.
	if strings.HasPrefix(p, "/auth/") {
		return false
	}
	// The device door carries its own lock: /d/{token}/… is checked by
	// device.go, which refuses with this same problem and otherwise puts a
	// viewer on the context. Gating it here would 401 every cast device
	// before it could show what it holds.
	if strings.HasPrefix(p, "/d/") {
		return false
	}
	for _, prefix := range []string{"/api/", "/streams/", "/s/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// viewerOf is who a request is, if it is anybody: the session cookie a
// browser carries, or a bearer token from an agent or the MCP surface.
//
// Nothing is logged here. One line per request would be a log of what the
// household watched, and one line per token needs a table of tokens seen —
// the subject is on the session cookie either way if a question is ever
// asked.
func (rp *relyingParty) viewerOf(r *http.Request) (viewer, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if claims, err := auth.DecodeSession(rp.cfg.SessionSecret, c.Value, time.Now()); err == nil {
			return viewer{Subject: claims.Subject, Name: claims.Name}, true
		}
	}
	token, ok := bearerToken(r)
	if !ok {
		return viewer{}, false
	}
	claims, err := rp.verifyToken(r.Context(), token, auth.Expect{
		Issuer: rp.cfg.Issuer, Audiences: rp.audiences(),
	})
	if err != nil {
		return viewer{}, false
	}
	return viewer{Subject: claims.Subject, Name: displayName(claims), Bearer: true}, true
}

// audiences are the client ids a bearer token may be for: flickr itself, and
// whatever OIDC_DELEGATE_CLIENTS names — an MCP client's id, an agent's —
// so a token minted for one of the household's own tools is admitted without
// flickr having to be its audience too.
func (rp *relyingParty) audiences() []string {
	return append([]string{rp.cfg.ClientID}, rp.cfg.DelegateClients...)
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(h[7:])
	return token, token != ""
}

// ── talking to the provider ─────────────────────────────────────────────

func (rp *relyingParty) redirectURI() string { return rp.cfg.AppURL + "/auth/callback" }

// exchange trades the authorization code for tokens (client_secret_post,
// with the PKCE verifier) and answers the id_token. The access token is
// deliberately dropped: flickr calls nothing on the household's behalf, so
// keeping one would be a credential stored for no reason.
func (rp *relyingParty) exchange(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {rp.redirectURI()},
		"client_id":     {rp.cfg.ClientID},
		"code_verifier": {verifier},
	}
	if rp.cfg.ClientSecret != "" {
		form.Set("client_secret", rp.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rp.provider.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := rp.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the token endpoint answered %s", resp.Status)
	}
	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return "", fmt.Errorf("the token endpoint's answer is not JSON: %w", err)
	}
	if tokens.IDToken == "" {
		return "", errors.New("the token endpoint answered no id_token")
	}
	return tokens.IDToken, nil
}

// verifyToken checks one RS256 token against the issuer's keys, refetching
// them when the token names a key id we have never seen — that is what a
// rotation looks like from here. The refetch is capped at one a minute, so
// a stream of forged key ids costs one request a minute and nothing else.
func (rp *relyingParty) verifyToken(ctx context.Context, token string, want auth.Expect) (auth.IDClaims, error) {
	keys, err := rp.jwks(ctx, false)
	if err != nil {
		return auth.IDClaims{}, err
	}
	if kid := tokenKid(token); kid != "" && !hasKid(keys, kid) {
		if fresh, err := rp.jwks(ctx, true); err == nil {
			keys = fresh
		}
	}
	return auth.VerifyJWT(keys, token, want, time.Now())
}

// jwks answers the issuer's signing keys, fetching them the first time they
// are wanted rather than at boot: a provider that is briefly down should
// delay the first sign-in, not stop the server from starting.
func (rp *relyingParty) jwks(ctx context.Context, refetch bool) (auth.JWKS, error) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	have := len(rp.keys.Keys) > 0
	if have && (!refetch || time.Since(rp.fetched) < jwksMinRefetch) {
		return rp.keys, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rp.provider.JWKSURI, nil)
	if err != nil {
		return rp.keys, err
	}
	resp, err := rp.client.Do(req)
	if err != nil {
		return rp.keys, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return rp.keys, err
	}
	if resp.StatusCode != http.StatusOK {
		return rp.keys, fmt.Errorf("the issuer's JWKS answered %s", resp.Status)
	}
	keys, err := auth.ParseJWKS(body)
	if err != nil {
		return rp.keys, err
	}
	rp.keys, rp.fetched = keys, time.Now()
	return keys, nil
}

func hasKid(keys auth.JWKS, kid string) bool {
	for _, k := range keys.Keys {
		if k.Kid == kid {
			return true
		}
	}
	return false
}

// tokenKid is the key id a compact JWS names, "" when it names none or is
// not one. auth.VerifyJWT deliberately answers only yes or no, and the
// difference between "rotated" and "forged" is the server's business.
func tokenKid(token string) string {
	head, _, ok := strings.Cut(token, ".")
	if !ok {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return ""
	}
	var h struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return ""
	}
	return h.Kid
}

// ── small things ────────────────────────────────────────────────────────

// displayName is what a screen greets them by: whichever of the three the
// provider filled in, and the subject if it filled in none.
func displayName(c auth.IDClaims) string {
	for _, s := range []string{c.Name, c.PreferredUsername, c.Email} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return c.Subject
}

// safeReturnTo keeps a login from becoming an open redirect: only a path on
// this origin comes back out. "//evil.example" is protocol-relative and
// "/\evil.example" is what some browsers make of a backslash, so a second
// leading separator is as unwelcome as a scheme.
func safeReturnTo(p string) string {
	if len(p) < 1 || p[0] != '/' {
		return "/"
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return "/"
	}
	return p
}

func (rp *relyingParty) setCookie(w http.ResponseWriter, name, value string, age time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		MaxAge:   int(age / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Lax rather than Strict because the callback ARRIVES from Keycloak:
		// a Strict cookie is not sent on that navigation, and the login
		// would never finish.
		Secure: strings.HasPrefix(rp.cfg.AppURL, "https://"),
	})
}

func (rp *relyingParty) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(rp.cfg.AppURL, "https://"),
	})
}
