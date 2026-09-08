package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const realm = "https://keycloak.example/realms/domestic-realm"

func discoveryBody(issuer string) string {
	return `{
		"issuer": "` + issuer + `",
		"authorization_endpoint": "` + issuer + `/protocol/openid-connect/auth",
		"token_endpoint": "` + issuer + `/protocol/openid-connect/token",
		"jwks_uri": "` + issuer + `/protocol/openid-connect/certs",
		"end_session_endpoint": "` + issuer + `/protocol/openid-connect/logout",
		"userinfo_endpoint": "` + issuer + `/protocol/openid-connect/userinfo"
	}`
}

func TestParseDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, body, issuer string
		ok                 bool
	}{
		{name: "good", body: discoveryBody(realm), issuer: realm, ok: true},
		{name: "trailing slash on what we asked for", body: discoveryBody(realm), issuer: realm + "/", ok: true},
		{name: "another realm", body: discoveryBody(realm + "-staging"), issuer: realm},
		{name: "no issuer", body: `{"token_endpoint":"x"}`, issuer: realm},
		{
			name:   "no token endpoint",
			body:   `{"issuer":"` + realm + `","authorization_endpoint":"a","jwks_uri":"j"}`,
			issuer: realm,
		},
		{name: "not json", body: `<html>`, issuer: realm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParseDiscovery([]byte(tc.body), tc.issuer)
			if !tc.ok {
				if err == nil {
					t.Fatalf("ParseDiscovery accepted it: %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDiscovery: %v", err)
			}
			if p.Issuer != realm ||
				p.AuthorizationEndpoint != realm+"/protocol/openid-connect/auth" ||
				p.TokenEndpoint != realm+"/protocol/openid-connect/token" ||
				p.JWKSURI != realm+"/protocol/openid-connect/certs" ||
				p.EndSessionEndpoint != realm+"/protocol/openid-connect/logout" ||
				p.UserinfoEndpoint != realm+"/protocol/openid-connect/userinfo" {
				t.Fatalf("provider = %+v", p)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(discoveryBody("http://" + r.Host)))
	}))
	defer srv.Close()

	p, err := Discover(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if asked != "/.well-known/openid-configuration" {
		t.Fatalf("fetched %q", asked)
	}
	if p.Issuer != srv.URL {
		t.Fatalf("issuer = %q, want %q", p.Issuer, srv.URL)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no realm here", http.StatusNotFound)
	}))
	defer down.Close()
	if _, err := Discover(context.Background(), down.Client(), down.URL); err == nil {
		t.Fatal("Discover accepted a 404")
	}
}
