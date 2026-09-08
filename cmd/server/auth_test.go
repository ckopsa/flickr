package main

// The walk: anonymous refusal, sign-in, the session, a bearer, sign-out —
// against the fake Keycloak in fakeissuer_test.go and the real routing
// table, so the gate is tested where it actually stands.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// authFixture is the fixture library with a relying party in front of it.
func authFixture(t *testing.T, iss *fakeIssuer, tweak func(*authConfig)) (*server, http.Handler) {
	t.Helper()
	cfg := authConfig{
		Issuer:          iss.URL(),
		ClientID:        iss.ClientID,
		ClientSecret:    iss.ClientSecret,
		AppURL:          "http://flickr.example",
		SessionSecret:   []byte("a household session secret, 32+ bytes"),
		RequireAuth:     true,
		DelegateClients: []string{"agent-x"},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	rp, err := newRelyingParty(context.Background(), cfg, iss.srv.Client())
	if err != nil {
		t.Fatalf("newRelyingParty: %v", err)
	}
	srv, _ := fixtureServer(t)
	srv.rp = rp
	return srv, srv.guarded(srv.routes())
}

// ask is one request, with whatever cookies and headers it carries.
func ask(t *testing.T, h http.Handler, method, target string, cookies []*http.Cookie, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for k, vs := range header {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func cookieNamed(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, w.Header().Values("Set-Cookie"))
	return nil
}

func TestAuthWalk(t *testing.T) {
	iss := newFakeIssuer(t)
	_, h := authFixture(t, iss, nil)

	// ── anonymous, at a gated address ───────────────────────────────────
	w := ask(t, h, "GET", "/api/library", nil, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/library = %d, want 401", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type %q, want application/problem+json", ct)
	}
	var problem struct {
		Type, Title, Detail string
		Status              int
		Remedy              struct {
			Text string
			Link struct{ Href, Title string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Type != "unauthenticated" || problem.Status != 401 {
		t.Fatalf("problem = %+v", problem)
	}
	if problem.Remedy.Link.Href != "/auth/login" {
		t.Fatalf("remedy link = %q, want /auth/login", problem.Remedy.Link.Href)
	}

	// ── the login door ──────────────────────────────────────────────────
	w = ask(t, h, "GET", "/auth/login?return_to=/%23/x", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("/auth/login = %d, want 302", w.Code)
	}
	sent, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := sent.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != iss.ClientID ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("nonce") == "" ||
		q.Get("redirect_uri") != "http://flickr.example/auth/callback" ||
		q.Get("scope") != "openid profile email" {
		t.Fatalf("authorize query = %v", q)
	}
	pending := cookieNamed(t, w, authCookie)
	if !pending.HttpOnly || pending.MaxAge != 600 {
		t.Fatalf("login cookie = %+v", pending)
	}

	// ── the provider, and back ──────────────────────────────────────────
	back := iss.Visit(w.Header().Get("Location"))
	if back.Get("state") != q.Get("state") {
		t.Fatalf("the callback carries state %q, want %q", back.Get("state"), q.Get("state"))
	}
	if got := iss.LastAuthorize().Get("code_challenge"); got != q.Get("code_challenge") {
		t.Fatalf("the issuer saw challenge %q, want %q", got, q.Get("code_challenge"))
	}
	w = ask(t, h, "GET", "/auth/callback?code="+url.QueryEscape(back.Get("code"))+
		"&state="+url.QueryEscape(back.Get("state")), []*http.Cookie{pending}, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("/auth/callback = %d (%s), want 302", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/#/x" {
		t.Fatalf("callback sent the browser to %q, want /#/x", loc)
	}
	session := cookieNamed(t, w, sessionCookie)
	if !session.HttpOnly || session.Value == "" || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v", session)
	}
	if session.Secure {
		t.Fatal("session cookie is Secure over an http app URL")
	}
	signedIn := []*http.Cookie{session}

	// ── signed in ───────────────────────────────────────────────────────
	w = ask(t, h, "GET", "/api/library", signedIn, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("signed-in /api/library = %d, want 200", w.Code)
	}
	w = ask(t, h, "GET", "/api/", signedIn, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("signed-in /api/ = %d, want 200", w.Code)
	}
	var root struct {
		Viewer struct{ Subject, Name string }
		Links  map[string]struct{ Href, Title string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root.Viewer.Subject != "u-chris" || root.Viewer.Name != "Chris" {
		t.Fatalf("root viewer = %+v", root.Viewer)
	}
	if root.Links["logout"].Href != "/auth/logout" {
		t.Fatalf("root links = %+v", root.Links)
	}
	if _, ok := root.Links["login"]; ok {
		t.Fatal("root offers sign-in to somebody already signed in")
	}

	w = ask(t, h, "GET", "/auth/me", signedIn, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/auth/me = %d, want 200", w.Code)
	}
	var me struct{ Self, Subject, Name string }
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Self != "/auth/me" || me.Subject != "u-chris" || me.Name != "Chris" {
		t.Fatalf("/auth/me = %+v", me)
	}
	if w := ask(t, h, "GET", "/auth/me", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /auth/me = %d, want 401", w.Code)
	}

	// ── signing out ─────────────────────────────────────────────────────
	w = ask(t, h, "GET", "/auth/logout", signedIn, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("/auth/logout = %d, want 302", w.Code)
	}
	if cleared := cookieNamed(t, w, sessionCookie); cleared.Value != "" || cleared.MaxAge != -1 {
		t.Fatalf("logout left the session cookie as %+v", cleared)
	}
	end, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Scheme+"://"+end.Host+end.Path != iss.URL()+"/logout" ||
		end.Query().Get("post_logout_redirect_uri") != "http://flickr.example/" ||
		end.Query().Get("client_id") != iss.ClientID {
		t.Fatalf("logout sent the browser to %q", w.Header().Get("Location"))
	}
}

func TestAuthBearer(t *testing.T) {
	iss := newFakeIssuer(t)
	_, h := authFixture(t, iss, nil)

	for _, tc := range []struct {
		name, audience string
		want           int
	}{
		// The agent's own client id, named in OIDC_DELEGATE_CLIENTS.
		{"a delegate client", "agent-x", http.StatusOK},
		{"flickr itself", "flickr", http.StatusOK},
		{"somebody else", "nobody", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{"Authorization": {"Bearer " + iss.Bearer(tc.audience, "u-agent")}}
			if w := ask(t, h, "GET", "/api/library", nil, header); w.Code != tc.want {
				t.Fatalf("bearer for %q = %d, want %d (%s)", tc.audience, w.Code, tc.want, w.Body)
			}
		})
	}
	t.Run("nonsense", func(t *testing.T) {
		header := http.Header{"Authorization": {"Bearer not-a-token"}}
		if w := ask(t, h, "GET", "/api/library", nil, header); w.Code != http.StatusUnauthorized {
			t.Fatalf("a forged bearer = %d, want 401", w.Code)
		}
	})
	t.Run("the viewer rides on the request", func(t *testing.T) {
		header := http.Header{"Authorization": {"Bearer " + iss.Bearer("agent-x", "u-agent")}}
		w := ask(t, h, "GET", "/api/", nil, header)
		var root struct {
			Viewer struct{ Subject, Name string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
			t.Fatal(err)
		}
		if root.Viewer.Subject != "u-agent" || root.Viewer.Name != "u-agent" {
			t.Fatalf("root viewer = %+v", root.Viewer)
		}
	})
}

func TestAuthOpenDoors(t *testing.T) {
	iss := newFakeIssuer(t)
	_, h := authFixture(t, iss, nil)

	// The Nomad health check has no cookie and never will.
	if w := ask(t, h, "GET", "/api/system", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("anonymous /api/system = %d, want 200", w.Code)
	}
	// A share link is a navigation: it goes to the sign-in, not to a problem
	// document a browser cannot show.
	w := ask(t, h, "GET", "/s/item/4?t=10", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("anonymous /s/… = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/auth/login?return_to=%2Fs%2Fitem%2F4%3Ft%3D10" {
		t.Fatalf("share redirect = %q", loc)
	}
	// The preflight carries no cookie by definition.
	if w := ask(t, h, "OPTIONS", "/api/library", nil, nil); w.Code == http.StatusUnauthorized {
		t.Fatal("the gate refused a CORS preflight")
	}
}

func TestAuthReturnTo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/#/x", "/#/x"},
		{"/library", "/library"},
		{"", "/"},
		{"https://evil.example", "/"},
		{"//evil.example", "/"},
		{`/\evil.example`, "/"},
		{"evil.example", "/"},
	} {
		if got := safeReturnTo(tc.in); got != tc.want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A callback whose state is not the one this browser started with is the
// CSRF check doing its job.
func TestAuthCallbackRefusals(t *testing.T) {
	iss := newFakeIssuer(t)
	_, h := authFixture(t, iss, nil)

	w := ask(t, h, "GET", "/auth/login", nil, nil)
	pending := cookieNamed(t, w, authCookie)
	back := iss.Visit(w.Header().Get("Location"))

	for _, tc := range []struct {
		name, target string
		cookies      []*http.Cookie
	}{
		{"wrong state", "/auth/callback?code=" + back.Get("code") + "&state=somebody-elses", []*http.Cookie{pending}},
		{"no state", "/auth/callback?code=" + back.Get("code"), []*http.Cookie{pending}},
		{"no login in progress", "/auth/callback?code=x&state=y", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ask(t, h, "GET", tc.target, tc.cookies, nil)
			if w.Code < 400 || w.Code >= 500 {
				t.Fatalf("callback = %d, want a 4xx (%s)", w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("content-type %q, want application/problem+json", ct)
			}
			if !strings.Contains(w.Body.String(), `"/auth/login"`) {
				t.Fatalf("no remedy link home: %s", w.Body)
			}
		})
	}
}

// OIDC_REQUIRE_AUTH=0 is the staged rollout: sign-in is offered, nothing is
// gated, and the root says so.
func TestAuthNotRequired(t *testing.T) {
	iss := newFakeIssuer(t)
	_, h := authFixture(t, iss, func(c *authConfig) { c.RequireAuth = false })

	w := ask(t, h, "GET", "/api/library", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous /api/library with the gate off = %d, want 200", w.Code)
	}
	w = ask(t, h, "GET", "/api/", nil, nil)
	var root struct {
		Viewer *struct{ Subject string }
		Links  map[string]struct{ Href string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root.Viewer != nil {
		t.Fatalf("root names a viewer nobody signed in as: %+v", root.Viewer)
	}
	if root.Links["login"].Href != "/auth/login" {
		t.Fatalf("root does not offer sign-in: %+v", root.Links)
	}
}

// With no issuer there is no relying party: the routes are what they were,
// the goldens are what they were, and /auth/login is a 404.
func TestAuthOffByDefault(t *testing.T) {
	srv, h := fixtureServer(t)
	if srv.rp != nil {
		t.Fatal("the fixture server has a relying party")
	}
	if got := srv.guarded(h); got == nil {
		t.Fatal("guarded answered nil")
	}
	if w := get(t, h, "/api/library"); w.Code != http.StatusOK {
		t.Fatalf("/api/library = %d, want 200", w.Code)
	}
	if w := get(t, h, "/api/"); strings.Contains(w.Body.String(), "viewer") {
		t.Fatalf("the root names a viewer with sign-in off: %s", w.Body)
	}
	if w := get(t, h, "/auth/login"); w.Code == http.StatusFound {
		t.Fatal("/auth/login redirects with no issuer configured")
	}
}

func TestAuthConfigFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want *authConfig // nil: an error, or (with the name "off") no config
		off  bool
	}{
		{name: "off", env: map[string]string{}, off: true},
		{
			name: "no secret",
			env:  map[string]string{"OIDC_ISSUER": "https://kc.example/realms/r", "OIDC_CLIENT_ID": "flickr"},
		},
		{
			name: "short secret",
			env: map[string]string{"OIDC_ISSUER": "https://kc.example/realms/r",
				"OIDC_CLIENT_ID": "flickr", "OIDC_SESSION_SECRET": "too short"},
		},
		{
			name: "no client id",
			env: map[string]string{"OIDC_ISSUER": "https://kc.example/realms/r",
				"OIDC_SESSION_SECRET": strings.Repeat("k", 32)},
		},
		{
			name: "the whole thing",
			env: map[string]string{
				"OIDC_ISSUER": "https://kc.example/realms/r/", "OIDC_CLIENT_ID": "flickr",
				"OIDC_CLIENT_SECRET": "s3cret", "OIDC_SESSION_SECRET": strings.Repeat("k", 32),
				"OIDC_REQUIRE_AUTH": "0", "OIDC_DELEGATE_CLIENTS": "agent-x, mcp ,",
			},
			want: &authConfig{
				Issuer: "https://kc.example/realms/r", ClientID: "flickr", ClientSecret: "s3cret",
				AppURL: "http://box.lan:8080", SessionSecret: []byte(strings.Repeat("k", 32)),
				RequireAuth: false, DelegateClients: []string{"agent-x", "mcp"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET",
				"OIDC_APP_URL", "OIDC_SESSION_SECRET", "OIDC_REQUIRE_AUTH", "OIDC_DELEGATE_CLIENTS"} {
				t.Setenv(k, tc.env[k])
			}
			cfg, err := authConfigFromEnv("http://box.lan:8080")
			switch {
			case tc.off:
				if cfg != nil || err != nil {
					t.Fatalf("cfg = %+v, err = %v; want both nil", cfg, err)
				}
			case tc.want == nil:
				if err == nil {
					t.Fatalf("a half-configured gate started anyway: %+v", cfg)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Issuer != tc.want.Issuer || cfg.ClientID != tc.want.ClientID ||
					cfg.ClientSecret != tc.want.ClientSecret || cfg.AppURL != tc.want.AppURL ||
					string(cfg.SessionSecret) != string(tc.want.SessionSecret) ||
					cfg.RequireAuth != tc.want.RequireAuth ||
					strings.Join(cfg.DelegateClients, ",") != strings.Join(tc.want.DelegateClients, ",") {
					t.Fatalf("cfg = %+v, want %+v", cfg, tc.want)
				}
			}
		})
	}
}
