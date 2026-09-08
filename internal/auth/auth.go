// Package auth is the pure half of flickr's OpenID Connect relying party:
// the parts that are arithmetic over bytes rather than a conversation with a
// browser.
//
// Four things live here, each table-tested:
//
//   - PKCE (RFC 7636): NewVerifier, Challenge, and NewState for the state
//     and nonce that ride beside it.
//   - Signed claims: Sign and Verify are an HMAC-SHA256 over any payload,
//     and SessionClaims/EncodeSession/DecodeSession are the household
//     session on top of them. The same two functions carry the ten-minute
//     login cookie and, later, a device token: one signature format, not
//     three.
//   - VerifyJWT: an RS256 id_token or bearer token checked against the
//     issuer's JWKS, with the issuer, audience, nonce and expiry the caller
//     says to expect.
//   - ParseDiscovery: the provider document, refused when its issuer is not
//     the one asked for.
//
// Deliberately NOT here: token storage, cookies, redirects, the token
// exchange — the relying party's conversation is cmd/server/auth.go's, and
// this package holds no state and reads no request. The one exception is
// Discover, a convenience wrapper around the single GET that fetches the
// document ParseDiscovery parses; the parse is what the tests pin. Fetching
// the JWKS is the server's job too, for the same reason: VerifyJWT is handed
// the keys, so a test needs no network and the server may cache them and
// refetch on its own clock.
//
// Standard library only: this is crypto in a media server, and a dependency
// here would be one more thing to keep patched.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── PKCE ────────────────────────────────────────────────────────────────

// NewVerifier is one PKCE code verifier: 32 random bytes as base64url
// without padding, which is 43 characters — inside RFC 7636's 43..128 and
// already in the unreserved alphabet the spec asks for.
//
// crypto/rand.Read cannot fail (it panics if the kernel's source is gone),
// so this returns no error and a caller has nothing to handle.
func NewVerifier() string { return randomToken(32) }

// NewState is the same randomness under another name: the `state` that ties
// a callback to the browser that started it, and the `nonce` that ties an
// id_token to this login.
func NewState() string { return randomToken(32) }

// Challenge is the S256 code challenge for a verifier: base64url, no
// padding, of the SHA-256 of the verifier's ASCII bytes.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── signed claims ───────────────────────────────────────────────────────

// Sign is a payload and its MAC, both base64url without padding, joined by
// a dot: `<payload>.<HMAC-SHA256(secret, payload)>`. It is not a JWT — there
// is no header to lie about an algorithm in, which is the whole point.
func Sign(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify checks a token Sign made and answers the payload. The comparison is
// constant time, and a token whose payload or MAC has been touched is an
// error rather than a payload the caller has to second-guess.
func Verify(secret []byte, token string) ([]byte, error) {
	encPayload, encMAC, ok := strings.Cut(token, ".")
	if !ok {
		return nil, errors.New("auth: token has no dot")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return nil, fmt.Errorf("auth: token payload: %w", err)
	}
	sum, err := base64.RawURLEncoding.DecodeString(encMAC)
	if err != nil {
		return nil, fmt.Errorf("auth: token signature: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return nil, errors.New("auth: bad signature")
	}
	return payload, nil
}

// SessionClaims is who the household session belongs to: the provider's
// stable subject, the name a screen greets them by, and the window the
// cookie is good for.
type SessionClaims struct {
	Subject   string
	Name      string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// sessionJSON is the wire form: short names, unix seconds, so the cookie
// stays small and reads the way a JWT's claims do.
type sessionJSON struct {
	Sub  string `json:"sub"`
	Name string `json:"name,omitempty"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

// EncodeSession is the claims as one signed token.
func EncodeSession(secret []byte, c SessionClaims) string {
	payload, _ := json.Marshal(sessionJSON{ // two strings and two ints
		Sub: c.Subject, Name: c.Name,
		Iat: c.IssuedAt.Unix(), Exp: c.ExpiresAt.Unix(),
	})
	return Sign(secret, payload)
}

// DecodeSession reads a token back, refusing a bad signature and an expired
// session. There is no skew here on purpose: this cookie is minted by this
// server and read by this server, so the only clock involved is one.
func DecodeSession(secret []byte, token string, now time.Time) (SessionClaims, error) {
	payload, err := Verify(secret, token)
	if err != nil {
		return SessionClaims{}, err
	}
	var s sessionJSON
	if err := json.Unmarshal(payload, &s); err != nil {
		return SessionClaims{}, fmt.Errorf("auth: session claims: %w", err)
	}
	if s.Sub == "" {
		return SessionClaims{}, errors.New("auth: session has no subject")
	}
	c := SessionClaims{
		Subject: s.Sub, Name: s.Name,
		IssuedAt:  time.Unix(s.Iat, 0).UTC(),
		ExpiresAt: time.Unix(s.Exp, 0).UTC(),
	}
	if !now.Before(c.ExpiresAt) {
		return SessionClaims{}, errors.New("auth: session expired")
	}
	return c, nil
}
