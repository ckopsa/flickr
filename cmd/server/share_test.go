package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// tag reads one meta tag's content out of a share page: `property` for the
// og: family, `name` for twitter's. "" when the page carries none, which is
// the assertion a card with no picture wants.
func tag(t *testing.T, body, key string) string {
	t.Helper()
	re := regexp.MustCompile(`<meta (?:property|name)="` + regexp.QuoteMeta(key) + `" content="([^"]*)">`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// share fetches one share page and fails on an unexpected status.
func share(t *testing.T, h http.Handler, target string, status int) string {
	t.Helper()
	w := get(t, h, target)
	if w.Code != status {
		t.Fatalf("GET %s = %d, want %d: %s", target, w.Code, status, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET %s content-type = %q", target, ct)
	}
	return w.Body.String()
}

// The acceptance: a scene of the film, unfurled. The sentence is the one
// GET /api/-/passage says, the picture is absolute, and a browser lands on
// the hash the passage has always been.
func TestSharePageUnfurlsAPassage(t *testing.T) {
	_, h := fixtureServer(t)
	body := share(t, h, "/s/item/4?t=4740&end=5070", http.StatusOK)

	if got, want := tag(t, body, "og:title"), "1:19:00 – 1:24:30 of Frozen"; got != want {
		t.Errorf("og:title = %q, want %q", got, want)
	}
	if got, want := tag(t, body, "og:description"),
		"Young princess Anna sets off to find her sister."; got != want {
		t.Errorf("og:description = %q, want %q", got, want)
	}
	if got, want := tag(t, body, "og:image"), "http://example.com/api/items/4/poster"; got != want {
		t.Errorf("og:image = %q, want %q", got, want)
	}
	// The hop, three ways: the tag a crawler reads, the refresh a browser
	// with no script honours, and the script that makes it instant.
	const dest = "http://example.com/#/item/4?t=4740&amp;end=5070"
	if got, want := tag(t, body, "og:url"), dest; got != want {
		t.Errorf("og:url = %q, want %q", got, want)
	}
	if !strings.Contains(body, `<meta http-equiv="refresh" content="0; url=`+dest+`">`) {
		t.Errorf("no redirect to %s:\n%s", dest, body)
	}
	if !strings.Contains(body, `location.replace("http://example.com/#/item/4?t=4740\u0026end=5070")`) {
		t.Errorf("no script hop:\n%s", body)
	}
}

// The rest of the grammar unfurls too — the path after /s IS the hash, so
// every spelling the route document resolves has a card.
func TestSharePageSpellings(t *testing.T) {
	_, h := fixtureServer(t)
	for _, tc := range []struct {
		name, target       string
		status             int
		title, image, kind string
	}{
		{name: "an episode's own still", target: "/s/item/8?t=142&end=854", status: 200,
			title: "S03E22 2:22 – 14:14 of Beach Games",
			image: "http://example.com/api/items/8/still", kind: "video.other"},
		{name: "a run names both ends and the work", target: "/s/item/8?t=142&end=854&until=9",
			status: 200, title: "S03E22 2:22 – S03E23 14:14 of The Office",
			image: "http://example.com/api/items/8/still", kind: "video.other"},
		{name: "the show form says the same sentence",
			target: "/s/show/The%20Office?ep=S03E22&t=142&end=854", status: 200,
			title: "S03E22 2:22 – 14:14 of Beach Games",
			image: "http://example.com/api/items/8/still", kind: "video.other"},
		{name: "a book's passage, and its cover", target: "/s/item/3?from=ch%3A3&to=ch%3A4",
			status: 200, title: "ch. 3 – ch. 4 of The Haunting of Hill House",
			image: "http://example.com/api/items/3/cover", kind: "book"},
		{name: "an item with no passage is the thing itself", target: "/s/item/4", status: 200,
			title: "Frozen", image: "http://example.com/api/items/4/poster", kind: "video.other"},
		{name: "a whole show is its poster, not one episode's still",
			target: "/s/show/The%20Office", status: 200, title: "The Office",
			image: "http://example.com/api/items/8/poster", kind: "video.other"},
		// A link to something the library no longer holds says so, and is
		// still a page: the person who clicked reaches the client, which
		// shows the same refusal in words.
		{name: "an item that is gone", target: "/s/item/999?t=1", status: 404,
			title: "No such item"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := share(t, h, tc.target, tc.status)
			if got := tag(t, body, "og:title"); got != tc.title {
				t.Errorf("og:title = %q, want %q", got, tc.title)
			}
			if got := tag(t, body, "og:image"); got != tc.image {
				t.Errorf("og:image = %q, want %q", got, tc.image)
			}
			if tc.kind != "" {
				if got := tag(t, body, "og:type"); got != tc.kind {
					t.Errorf("og:type = %q, want %q", got, tc.kind)
				}
			}
			if !strings.Contains(body, `http-equiv="refresh"`) {
				t.Errorf("a share page always hands the browser on:\n%s", body)
			}
		})
	}
}

// The minted link carries the spelling that unfurls beside the one that is
// written down, and following it gives the same sentence back.
func TestMintedLinkCarriesShareHref(t *testing.T) {
	_, h, doc := playing(t, idBeach, nil)
	post(t, h, href(t, doc, "actions", "mark_in"), map[string]any{"seconds": 142})
	doc = decode(t, post(t, h, href(t, doc, "actions", "mark_out"), map[string]any{"seconds": 854}))

	for _, minted := range []map[string]any{
		decode(t, get(t, h, href(t, doc, "actions", "link"))),
		decode(t, get(t, h, "/api/-/passage?item=8&t=142&end=854")),
	} {
		shareHref, _ := minted["share_href"].(string)
		if want := "http://example.com/s/item/8?t=142&end=854"; shareHref != want {
			t.Fatalf("share_href = %q, want %q", shareHref, want)
		}
		body := share(t, h, strings.TrimPrefix(shareHref, "http://example.com"), http.StatusOK)
		if got := tag(t, body, "og:title"); got != minted["sentence"] {
			t.Errorf("the shared page says %q, the minted link %q", got, minted["sentence"])
		}
	}
}
