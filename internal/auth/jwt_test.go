package auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The tokens below are assembled by hand — header, claims, signature — so
// the table can say things no library would let it say: alg none, alg HS256
// with the RSA public key as the HMAC secret, a kid nobody published.

const testKid = "test"

func testKey(t *testing.T) (*rsa.PrivateKey, JWKS) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, JWKS{Keys: []JWK{{
		Kty: "RSA", Kid: testKid, Use: "sig", Alg: "RS256",
		N: base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}), // 65537
	}}}
}

func b64(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// signRS256 is a whole token: the header and claims the test names, signed.
func signRS256(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	signing := b64(t, header) + "." + b64(t, claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyJWT(t *testing.T) {
	key, keys := testKey(t)
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	const issuer = "https://keycloak.example/realms/domestic-realm"

	header := map[string]any{"alg": "RS256", "kid": testKid, "typ": "JWT"}
	base := func() map[string]any {
		return map[string]any{
			"iss": issuer, "sub": "u-1", "aud": "flickr",
			"name": "Chris", "preferred_username": "chris", "email": "chris@example",
			"nonce": "n-1",
			"iat":   now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
		}
	}
	want := Expect{Issuer: issuer, Audiences: []string{"flickr"}, Nonce: "n-1"}

	// The confusion attack: the same claims, signed HS256 with the public
	// key's own bytes, which a verifier that believes the header accepts.
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hs256 := func() string {
		signing := b64(t, map[string]any{"alg": "HS256", "kid": testKid}) + "." + b64(t, base())
		mac := hmac.New(sha256.New, pubDER)
		mac.Write([]byte(signing))
		return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}

	for _, tc := range []struct {
		name  string
		token string
		want  Expect
		ok    bool
	}{
		{name: "good", token: signRS256(t, key, header, base()), want: want, ok: true},
		{
			name: "bad signature",
			token: func() string {
				tok := signRS256(t, key, header, base())
				parts := strings.Split(tok, ".")
				sig, err := base64.RawURLEncoding.DecodeString(parts[2])
				if err != nil {
					t.Fatal(err)
				}
				sig[0] ^= 0x01
				return parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sig)
			}(),
			want: want,
		},
		{
			name: "wrong issuer",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"iss": "https://keycloak.example/realms/somebody-else"})),
			want: want,
		},
		{
			name: "audience missing",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"aud": []string{"account", "another-client"}})),
			want: want,
		},
		{
			name: "audience satisfied by azp",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"aud": []string{"account"}, "azp": "flickr"})),
			want: want, ok: true,
		},
		{
			name: "audience as an array",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"aud": []string{"account", "flickr"}})),
			want: want, ok: true,
		},
		{
			name: "expired",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"exp": now.Add(-2 * time.Minute).Unix()})),
			want: want,
		},
		{
			name: "inside the skew",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"exp": now.Add(-30 * time.Second).Unix()})),
			want: want, ok: true,
		},
		{
			name:  "alg none",
			token: b64(t, map[string]any{"alg": "none", "kid": testKid}) + "." + b64(t, base()) + ".",
			want:  want,
		},
		{name: "alg HS256 with the public key as the secret", token: hs256(), want: want},
		{
			name:  "unknown kid",
			token: signRS256(t, key, map[string]any{"alg": "RS256", "kid": "rotated"}, base()),
			want:  want,
		},
		{
			name:  "nonce mismatch",
			token: signRS256(t, key, header, base()),
			want:  Expect{Issuer: issuer, Audiences: []string{"flickr"}, Nonce: "n-2"},
		},
		{
			// A bearer token from an agent carries no nonce, and the caller
			// asks for none.
			name: "no nonce wanted, none carried",
			token: signRS256(t, key, header, merge(base(), map[string]any{
				"nonce": nil, "aud": "agent-x"})),
			want: Expect{Issuer: issuer, Audiences: []string{"flickr", "agent-x"}}, ok: true,
		},
		{name: "not a JWT", token: "nonsense", want: want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := VerifyJWT(keys, tc.token, tc.want, now)
			if !tc.ok {
				if err == nil {
					t.Fatalf("VerifyJWT accepted it: %+v", claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyJWT: %v", err)
			}
			if claims.Subject != "u-1" || claims.Issuer != issuer {
				t.Fatalf("claims = %+v", claims)
			}
			if claims.Name != "Chris" || claims.PreferredUsername != "chris" || claims.Email != "chris@example" {
				t.Fatalf("claims = %+v", claims)
			}
		})
	}
}

func merge(base, over map[string]any) map[string]any {
	for k, v := range over {
		base[k] = v
	}
	return base
}

func TestParseJWKS(t *testing.T) {
	_, keys := testKey(t)
	body, err := json.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseJWKS(body)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].Kid != testKid {
		t.Fatalf("keys = %+v", got.Keys)
	}
	for _, bad := range []string{`{"keys":[]}`, `not json`} {
		if _, err := ParseJWKS([]byte(bad)); err == nil {
			t.Fatalf("ParseJWKS accepted %q", bad)
		}
	}
}
