// Package bearer is the token a worker carries through flickr's household
// gate, in the three shapes a worker actually meets: a Keycloak client the
// cluster gave it, a static token somebody pasted into a dev box's env, and
// nothing at all on a LAN with no gate. The transcriber and the converter
// are two such workers; the gate is one, so the token is one.
package bearer

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

// Source answers the bearer to send, or "" for no Authorization header.
type Source interface {
	Token(ctx context.Context) (string, error)
}

// None is a box behind nothing: no Authorization header is sent at all.
type None struct{}

// Token answers the empty string: no header.
func (None) Token(context.Context) (string, error) { return "", nil }

// Static is FLICKR_TOKEN, a bearer somebody pasted in.
type Static string

// Token answers the pasted bearer as it is.
func (t Static) Token(context.Context) (string, error) { return string(t), nil }

// ClientCredentials is the real one: the worker is a Keycloak client of its
// own and trades its secret for a short-lived access token, holding it until
// a minute before it expires.
type ClientCredentials struct {
	HTTP     *http.Client
	TokenURL string
	ID       string
	Secret   string
	Now      func() time.Time

	mu      sync.Mutex
	tok     string
	expires time.Time
}

// TokenURL is where Keycloak answers client credentials for a realm.
func TokenURL(issuer string) string {
	return strings.TrimSuffix(issuer, "/") + "/protocol/openid-connect/token"
}

// Token answers the held token, or trades the secret for a new one when the
// held one is gone or nearly out.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
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

// Authorize sets the Authorization header on req from src, or leaves it
// unset when there is no token to send.
func Authorize(ctx context.Context, src Source, req *http.Request) error {
	tok, err := src.Token(ctx)
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return nil
}
