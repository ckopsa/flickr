package hyper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The envelope's four names come first and in a fixed order, the kind's own
// fields follow in the order the handler added them, and links/actions/
// unavailable come last in key order — so a golden file reads top-down the
// way the handler was written and two runs agree byte for byte.
func TestEnvelopeShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  *Envelope
		want string
	}{
		{
			name: "bare document",
			env:  Doc("/api/", "root", "flickr"),
			want: `{"self":"/api/","kind":"root","title":"flickr"}`,
		},
		{
			name: "an empty title is left out, not written empty",
			env:  Doc("/api/items/7", "item", ""),
			want: `{"self":"/api/items/7","kind":"item"}`,
		},
		{
			name: "fields keep the order they were added",
			env: Doc("/api/items/7", "item", "Beach Games").
				Field("id", 7).
				Field("medium", "video").
				Field("duration_seconds", 2640.5),
			want: `{"self":"/api/items/7","kind":"item","title":"Beach Games",` +
				`"id":7,"medium":"video","duration_seconds":2640.5}`,
		},
		{
			name: "links, actions and unavailable come out in key order",
			env: Doc("/api/items/7", "item", "Beach Games").
				Link("work", "/api/works/show%3Athe-office", "The Office").
				Link("next", "/api/items/8", "").
				Action("progress", Action{Method: "POST", Href: "/api/progress",
					Input: map[string]string{"item_id": "7", "position_seconds": "number"},
					Label: "Save place"}).
				Action("play", Action{Method: "POST", Href: "/api/items/7/play", Label: "▶ Play"}).
				Unavailable("read", "a film is played, not read"),
			want: `{"self":"/api/items/7","kind":"item","title":"Beach Games",` +
				`"links":{"next":{"href":"/api/items/8"},` +
				`"work":{"href":"/api/works/show%3Athe-office","title":"The Office"}},` +
				`"actions":{"play":{"method":"POST","href":"/api/items/7/play","label":"▶ Play"},` +
				`"progress":{"method":"POST","href":"/api/progress",` +
				`"input":{"item_id":"7","position_seconds":"number"},"label":"Save place"}},` +
				`"unavailable":{"read":{"reason":"a film is played, not read"}}}`,
		},
		{
			name: "empty maps are absent, not null and not {}",
			env:  Doc("/api/items/7", "item", "x").Field("id", 7),
			want: `{"self":"/api/items/7","kind":"item","title":"x","id":7}`,
		},
		{
			name: "a nested envelope is just a value",
			env: Doc("/api/works/show%3Ax", "work", "X").
				Field("members", []*Envelope{Doc("/api/items/7", "item", "Beach Games")}),
			want: `{"self":"/api/works/show%3Ax","kind":"work","title":"X",` +
				`"members":[{"self":"/api/items/7","kind":"item","title":"Beach Games"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// Embed splices a published type's own fields in at the top level, in that
// type's field order, and drops a member whose name is already taken — the
// envelope's four names can never be shadowed by an embedded payload.
func TestEmbed(t *testing.T) {
	type work struct {
		Key   string   `json:"work_key"`
		Kind  string   `json:"kind"`  // collides with the envelope's kind
		Title string   `json:"title"` // collides with the envelope's title
		Year  int      `json:"year,omitempty"`
		Tags  []string `json:"tags"`
	}
	env := Doc("/api/works/show%3Ax", "work", "The Office").
		Field("work_kind", "show").
		Embed(work{Key: "show:x", Kind: "show", Title: "The Office", Tags: []string{"a"}}).
		Field("members", []string{})
	got, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"self":"/api/works/show%3Ax","kind":"work","title":"The Office",` +
		`"work_kind":"show","work_key":"show:x","tags":["a"],"members":[]}`
	if string(got) != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}

	// Anything that is not a JSON object is a programming error, and says so
	// rather than writing half a document.
	for _, bad := range []any{nil, 7, "x", []int{1}} {
		if _, err := json.Marshal(Doc("/a", "k", "").Embed(bad)); err == nil {
			t.Errorf("Embed(%#v) should not marshal", bad)
		}
	}
}

func TestSelfAndKind(t *testing.T) {
	e := Doc("/api/items/7", "item", "x")
	if e.Self() != "/api/items/7" || e.Kind() != "item" {
		t.Errorf("self=%q kind=%q", e.Self(), e.Kind())
	}
}

func TestWriteDoc(t *testing.T) {
	w := httptest.NewRecorder()
	WriteDoc(w, http.StatusOK, Doc("/api/", "root", "flickr"))
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
	if got := w.Body.String(); got != "{\"self\":\"/api/\",\"kind\":\"root\",\"title\":\"flickr\"}\n" {
		t.Errorf("body = %q", got)
	}

	// A document that cannot be marshalled becomes a problem, not a
	// truncated 200 with a JSON content type.
	w = httptest.NewRecorder()
	WriteDoc(w, http.StatusOK, Doc("/api/", "root", "flickr").Field("bad", make(chan int)))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("unmarshallable document: status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("unmarshallable document: content-type = %q", ct)
	}
}

// A refusal is an answer: RFC 7807 media type, the problem's own status, and
// a remedy that may carry the link to go to.
func TestWriteProblem(t *testing.T) {
	w := httptest.NewRecorder()
	WriteProblem(w, Refuse(http.StatusNotFound, "no-such-work", "No such work",
		`the library has no work keyed "show:nope"`).
		WithRemedy("browse the library and follow a work from there",
			&Link{Href: "/api/library", Title: "Library"}))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content-type = %q", ct)
	}
	want := `{"type":"no-such-work","title":"No such work","status":404,` +
		`"detail":"the library has no work keyed \"show:nope\"",` +
		`"remedy":{"text":"browse the library and follow a work from there",` +
		`"link":{"href":"/api/library","title":"Library"}}}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}

	// A remedy without anywhere to go is still a remedy; a problem without
	// a status still gets one.
	w = httptest.NewRecorder()
	WriteProblem(w, Problem{Type: "passage-on", Title: "A passage is on"}.
		WithRemedy("keep_watching clears it", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want the 500 fallback", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); !strings.Contains(got, `"remedy":{"text":"keep_watching clears it"}`) {
		t.Errorf("body = %s", got)
	}
}
