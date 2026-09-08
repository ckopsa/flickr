package main

// The bearer the household gate wants, in the three shapes a worker actually
// meets: a Keycloak client the cluster gave it, a static token somebody
// pasted into a dev box's env, and nothing at all on a LAN with no gate.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// refreshLead is how long before expiry a token is thrown away. A token that
// expires in flight fails an hour of transcription at the last step.
const refreshLead = time.Minute

type tokenSource interface {
	token(ctx context.Context) (string, error)
}

// noToken is a box behind nothing: no Authorization header is sent at all.
type noToken struct{}

func (noToken) token(context.Context) (string, error) { return "", nil }

// staticToken is FLICKR_TOKEN, a bearer somebody pasted in.
type staticToken string

func (t staticToken) token(context.Context) (string, error) { return string(t), nil }

// clientCredentials is the real one: the worker is a Keycloak client of its
// own and trades its secret for a short-lived access token, holding it until
// a minute before it expires.
type clientCredentials struct {
	HTTP     *http.Client
	TokenURL string
	ID       string
	Secret   string
	Now      func() time.Time

	mu      sync.Mutex
	tok     string
	expires time.Time
}

// tokenURL is where Keycloak answers client credentials for a realm.
func tokenURL(issuer string) string {
	return strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/token"
}

func (c *clientCredentials) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if c.tok != "" && now().Before(c.expires) {
		return c.tok, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s said %s: %s", c.TokenURL, resp.Status, strings.TrimSpace(string(body)))
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("%s answered no token: %w", c.TokenURL, err)
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("%s answered no token", c.TokenURL)
	}
	ttl := time.Duration(answer.ExpiresIn) * time.Second
	lead := refreshLead
	if ttl <= lead {
		lead = ttl / 2 // a very short token still gets asked for again early
	}
	c.tok, c.expires = answer.AccessToken, now().Add(ttl-lead)
	return c.tok, nil
}
