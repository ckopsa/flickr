package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// The RFC 7636 appendix B example, which is the one pair of values every
// PKCE implementation can be checked against.
func TestChallengeRFC7636(t *testing.T) {
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if got := Challenge(verifier); got != challenge {
		t.Errorf("Challenge(%q) = %q, want %q", verifier, got, challenge)
	}
}

func TestNewVerifier(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		for name, v := range map[string]string{"verifier": NewVerifier(), "state": NewState()} {
			if len(v) < 43 || len(v) > 128 {
				t.Fatalf("%s %q is %d chars, want 43..128", name, v, len(v))
			}
			if strings.ContainsAny(v, "+/=") {
				t.Fatalf("%s %q is not base64url without padding", name, v)
			}
			if seen[v] {
				t.Fatalf("%s %q came up twice", name, v)
			}
			seen[v] = true
		}
	}
}

func TestSignVerify(t *testing.T) {
	secret := []byte("a secret at least thirty-two bytes long")
	payload := []byte(`{"sub":"chris"}`)
	good := Sign(secret, payload)

	for _, tc := range []struct {
		name   string
		secret []byte
		token  string
		ok     bool
	}{
		{"good", secret, good, true},
		{"flipped byte in the payload", secret, flip(t, good, 0), false},
		{"flipped byte in the MAC", secret, flip(t, good, 1), false},
		{"wrong secret", []byte("another secret entirely, thirty-two+"), good, false},
		{"no dot", secret, strings.ReplaceAll(good, ".", ""), false},
		{"empty", secret, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Verify(tc.secret, tc.token)
			if tc.ok {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if string(got) != string(payload) {
					t.Fatalf("payload = %q, want %q", got, payload)
				}
				return
			}
			if err == nil {
				t.Fatalf("Verify accepted %q, payload %q", tc.token, got)
			}
		})
	}
}

// flip changes one byte of the token's part'th dot-separated half, keeping
// it decodable base64url: a tampered token, not a malformed one.
func flip(t *testing.T, token string, part int) string {
	t.Helper()
	halves := strings.SplitN(token, ".", 2)
	raw, err := base64.RawURLEncoding.DecodeString(halves[part])
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0x01
	halves[part] = base64.RawURLEncoding.EncodeToString(raw)
	return halves[0] + "." + halves[1]
}

func TestSession(t *testing.T) {
	secret := []byte("household session secret, 32+ bytes")
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	claims := SessionClaims{
		Subject: "f:realm:chris", Name: "Chris",
		IssuedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	token := EncodeSession(secret, claims)

	t.Run("round trip", func(t *testing.T) {
		got, err := DecodeSession(secret, token, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("DecodeSession: %v", err)
		}
		if got.Subject != claims.Subject || got.Name != claims.Name {
			t.Fatalf("got %+v, want subject %q name %q", got, claims.Subject, claims.Name)
		}
		if !got.ExpiresAt.Equal(claims.ExpiresAt) {
			t.Fatalf("expires at %s, want %s", got.ExpiresAt, claims.ExpiresAt)
		}
	})
	t.Run("expired", func(t *testing.T) {
		if _, err := DecodeSession(secret, token, claims.ExpiresAt.Add(time.Second)); err == nil {
			t.Fatal("DecodeSession accepted an expired session")
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		if _, err := DecodeSession([]byte("not the household secret at all!"), token, now); err == nil {
			t.Fatal("DecodeSession accepted a session signed by somebody else")
		}
	})
	t.Run("no subject", func(t *testing.T) {
		empty := EncodeSession(secret, SessionClaims{ExpiresAt: now.Add(time.Hour)})
		if _, err := DecodeSession(secret, empty, now); err == nil {
			t.Fatal("DecodeSession accepted a session with no subject")
		}
	})
}
