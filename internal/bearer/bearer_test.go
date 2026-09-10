package bearer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The three shapes of the gate: nothing, a pasted bearer, and a Keycloak
// client that holds its token until it is nearly out.
func TestSources(t *testing.T) {
	ctx := context.Background()
	if tok, err := (None{}).Token(ctx); tok != "" || err != nil {
		t.Errorf("None = %q, %v; want no header at all", tok, err)
	}
	if tok, err := Static("pasted").Token(ctx); tok != "pasted" || err != nil {
		t.Errorf("Static = %q, %v", tok, err)
	}

	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		fmt.Fprint(w, `{"access_token": "t", "expires_in": 300}`)
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	cc := &ClientCredentials{
		HTTP: srv.Client(), TokenURL: srv.URL, ID: "transcriber", Secret: "sssh",
		Now: func() time.Time { return now },
	}
	for i := 0; i < 3; i++ {
		if tok, err := cc.Token(ctx); tok != "t" || err != nil {
			t.Fatalf("token = %q, %v", tok, err)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times for a token good for five minutes, want once", asked)
	}
	// A minute before it expires the worker asks again, rather than finding
	// out at the end of an hour of transcription.
	now = now.Add(4*time.Minute + 30*time.Second)
	if _, err := cc.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("asked %d times, want a refresh a minute before expiry", asked)
	}
	if TokenURL("https://kc/realms/house/") != "https://kc/realms/house/protocol/openid-connect/token" {
		t.Errorf("TokenURL = %q", TokenURL("https://kc/realms/house/"))
	}
}
