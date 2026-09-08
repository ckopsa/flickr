package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// skew is how far a clock may be out before an expiry is believed. A minute
// is the usual allowance and the household's boxes are NTP-synced anyway.
const skew = 60 * time.Second

// JWK is one key out of the issuer's jwks_uri document. Only RSA signing
// keys are of any use here, and `n`/`e` are the modulus and exponent as
// base64url big-endian bytes.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

// JWKS is that whole document. The server fetches it and hands it here.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// ParseJWKS reads a jwks_uri body. A document with no keys at all is an
// error rather than a set that will refuse every token later with a message
// about the kid.
func ParseJWKS(body []byte) (JWKS, error) {
	var keys JWKS
	if err := json.Unmarshal(body, &keys); err != nil {
		return JWKS{}, fmt.Errorf("auth: jwks: %w", err)
	}
	if len(keys.Keys) == 0 {
		return JWKS{}, errors.New("auth: jwks carries no keys")
	}
	return keys, nil
}

// IDClaims is what an id_token (or a bearer token from the same issuer)
// says. Name, PreferredUsername and Email are the three the provider may
// answer a display name in, in that order of preference; AuthorizedParty is
// `azp`, which is where Keycloak puts the client a token was minted for when
// `aud` names somebody else.
type IDClaims struct {
	Issuer            string
	Subject           string
	Name              string
	PreferredUsername string
	Email             string
	Audience          []string
	AuthorizedParty   string
	ExpiresAt         int64
	IssuedAt          int64
	Nonce             string
}

// Expect is what the caller says the token must say: the issuer it must come
// from, the audiences any ONE of which satisfies it (or is its azp), and the
// nonce this login sent — checked only when there is one, since a bearer
// token from an agent carries none.
type Expect struct {
	Issuer    string
	Audiences []string
	Nonce     string
}

// claimsJSON is the wire form. `aud` is a string in some tokens and an array
// in others, so it goes through a type that reads either.
type claimsJSON struct {
	Iss               string   `json:"iss"`
	Sub               string   `json:"sub"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Aud               audience `json:"aud"`
	Azp               string   `json:"azp"`
	Exp               int64    `json:"exp"`
	Iat               int64    `json:"iat"`
	Nonce             string   `json:"nonce"`
}

// audience is `aud` in either of its two legal spellings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("auth: aud is neither a string nor an array: %s", b)
	}
	*a = many
	return nil
}

type headerJSON struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// VerifyJWT checks one compact JWS against the issuer's keys and answers its
// claims.
//
// Only RS256 is accepted, and the algorithm is read from the header only to
// REFUSE anything else: `none` and `HS256` are the two shapes of the
// confusion attack — the second signs with the RSA public key as an HMAC
// secret, which a verifier that trusts `alg` accepts happily — so an
// unexpected alg never reaches a key.
func VerifyJWT(keys JWKS, token string, want Expect, now time.Time) (IDClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return IDClaims{}, errors.New("auth: token is not a JWT")
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return IDClaims{}, fmt.Errorf("auth: token header: %w", err)
	}
	var h headerJSON
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return IDClaims{}, fmt.Errorf("auth: token header: %w", err)
	}
	if h.Alg != "RS256" {
		return IDClaims{}, fmt.Errorf("auth: unsupported token algorithm %q (only RS256)", h.Alg)
	}
	pub, err := publicKey(keys, h.Kid)
	if err != nil {
		return IDClaims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return IDClaims{}, fmt.Errorf("auth: token signature: %w", err)
	}
	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, signed[:], sig); err != nil {
		return IDClaims{}, errors.New("auth: bad token signature")
	}

	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return IDClaims{}, fmt.Errorf("auth: token claims: %w", err)
	}
	var c claimsJSON
	if err := json.Unmarshal(rawClaims, &c); err != nil {
		return IDClaims{}, fmt.Errorf("auth: token claims: %w", err)
	}
	claims := IDClaims{
		Issuer: c.Iss, Subject: c.Sub, Name: c.Name,
		PreferredUsername: c.PreferredUsername, Email: c.Email,
		Audience: c.Aud, AuthorizedParty: c.Azp,
		ExpiresAt: c.Exp, IssuedAt: c.Iat, Nonce: c.Nonce,
	}
	if want.Issuer != "" && claims.Issuer != want.Issuer {
		return IDClaims{}, fmt.Errorf("auth: token issuer %q, want %q", claims.Issuer, want.Issuer)
	}
	if len(want.Audiences) > 0 && !audienceOK(claims, want.Audiences) {
		return IDClaims{}, fmt.Errorf("auth: token audience %v is none of %v", claims.Audience, want.Audiences)
	}
	if claims.ExpiresAt == 0 {
		return IDClaims{}, errors.New("auth: token has no expiry")
	}
	if now.Add(-skew).After(time.Unix(claims.ExpiresAt, 0)) {
		return IDClaims{}, errors.New("auth: token expired")
	}
	if want.Nonce != "" && claims.Nonce != want.Nonce {
		return IDClaims{}, errors.New("auth: token nonce does not match this login")
	}
	return claims, nil
}

// audienceOK: any wanted audience named in `aud`, or standing as the token's
// authorized party — Keycloak drops a client out of `aud` and leaves it in
// `azp` when no other audience mapper applies.
func audienceOK(claims IDClaims, wanted []string) bool {
	for _, w := range wanted {
		if w == claims.AuthorizedParty {
			return true
		}
		for _, got := range claims.Audience {
			if got == w {
				return true
			}
		}
	}
	return false
}

// publicKey finds the signing key the header names. An unknown kid is an
// error the server answers by refetching the JWKS once — a provider that has
// rotated its keys looks exactly like a forged kid until it has.
func publicKey(keys JWKS, kid string) (*rsa.PublicKey, error) {
	for _, k := range keys.Keys {
		if k.Kid != kid {
			continue
		}
		if k.Kty != "RSA" {
			return nil, fmt.Errorf("auth: key %q is %q, not RSA", kid, k.Kty)
		}
		if k.Use != "" && k.Use != "sig" {
			return nil, fmt.Errorf("auth: key %q is for %q, not signing", kid, k.Use)
		}
		if k.Alg != "" && k.Alg != "RS256" {
			return nil, fmt.Errorf("auth: key %q signs %q, not RS256", kid, k.Alg)
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("auth: key %q modulus: %w", kid, err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("auth: key %q exponent: %w", kid, err)
		}
		if len(e) == 0 || len(e) > 8 {
			return nil, fmt.Errorf("auth: key %q has an unusable exponent", kid)
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}, nil
	}
	return nil, fmt.Errorf("auth: no key %q in the issuer's JWKS", kid)
}
