package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Provider is the handful of addresses out of the issuer's discovery
// document that a relying party actually uses.
type Provider struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// ParseDiscovery reads an OpenID provider configuration and refuses one
// whose `issuer` is not the issuer we asked about: a document fetched from
// one realm that names another is either a misconfiguration or somebody
// standing in the middle, and every token check afterwards compares against
// this string.
func ParseDiscovery(body []byte, wantIssuer string) (Provider, error) {
	var p Provider
	if err := json.Unmarshal(body, &p); err != nil {
		return Provider{}, fmt.Errorf("auth: discovery document: %w", err)
	}
	if p.Issuer == "" {
		return Provider{}, fmt.Errorf("auth: discovery document names no issuer")
	}
	if want := strings.TrimRight(wantIssuer, "/"); want != "" && p.Issuer != want {
		return Provider{}, fmt.Errorf("auth: discovery document is for issuer %q, want %q", p.Issuer, want)
	}
	if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" || p.JWKSURI == "" {
		return Provider{}, fmt.Errorf("auth: discovery document for %q is missing an endpoint", p.Issuer)
	}
	return p, nil
}

// Discover fetches that document from the issuer's well-known address. It is
// the one HTTP call in this package, here for the server's convenience: the
// parse above is what is tested, and what everything else is built on.
func Discover(ctx context.Context, client *http.Client, issuer string) (Provider, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Provider{}, fmt.Errorf("auth: discovery: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Provider{}, fmt.Errorf("auth: discovery: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Provider{}, fmt.Errorf("auth: discovery: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Provider{}, fmt.Errorf("auth: discovery: %s answered %s", url, resp.Status)
	}
	return ParseDiscovery(body, issuer)
}
