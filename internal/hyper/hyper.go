// Package hyper is flickr's hypermedia envelope (docs/hypermedia.md).
//
// Every document the API answers is one JSON object of the same shape: the
// fields the kind carries sit at the top level, and the envelope adds four —
// `self` (the document's own address), `kind`, `links` (reads, keyed by
// relation) and `actions` (writes the caller may make NOW, keyed by name,
// each with an `input` sketch and a `label` the screen shows), plus
// `unavailable` for the actions the kind has that this document does not
// afford, each with a reason in words: a button that is not there, and why.
//
// Addresses are opaque to the client. It follows `links` and `actions`, and
// the only URL it knows by heart is the root. That only holds if a handler
// never leaves a relation out because "the client knows how to build it", so
// the builder here is deliberately the cheapest way to say a document:
//
//	hyper.WriteDoc(w, 200, hyper.Doc("/api/items/2090", "item", "Beach Games").
//		Field("id", 2090).
//		Link("work", "/api/works/show%3Athe-office", "The Office").
//		Action("play", hyper.Action{Method: "POST", Href: "/api/items/2090/play",
//			Input: map[string]string{"seek_seconds": "number?"}, Label: "▶ Play"}).
//		Unavailable("read", "a film is played, not read"))
//
// Errors are RFC 7807 problems with a `remedy` — what would make the action
// available. A refusal is an answer, not an exception.
package hyper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// Link is one read the client may follow, keyed by relation in a document's
// `links`. Title is what a screen would put on it, when there is anything to
// say beyond the relation itself.
type Link struct {
	Href  string `json:"href"`
	Title string `json:"title,omitempty"`
}

// Action is one write the caller may make NOW, keyed by name in a document's
// `actions`.
//
// Input is a SKETCH, not JSON Schema: field name → the type in a word, a "?"
// suffix for optional ("number", "number?", "passage?"). It is enough for a
// screen to build a control and for a reader to know what to send; the
// values themselves cross as ordinary JSON. A sketch value may also be a
// literal the caller should send back unchanged — an item id pre-filled into
// a body the server would otherwise have to be told twice.
type Action struct {
	Method string            `json:"method"`
	Href   string            `json:"href"`
	Input  map[string]string `json:"input,omitempty"`
	Label  string            `json:"label"`
}

// Unavailable is one action the kind has that this document does not afford,
// and why not in plain words. The reason is for a person to read.
type Unavailable struct {
	Reason string `json:"reason"`
}

// Envelope is one document. Build it with Doc and the chainable setters; it
// marshals to the shape above, with the fields in the order they were added
// so a golden file reads top-down the way the handler was written. links,
// actions and unavailable are maps, so they come out in key order and two
// runs of the same handler are byte-identical.
type Envelope struct {
	self, kind, title string
	fields            []field
	links             map[string]Link
	actions           map[string]Action
	unavailable       map[string]Unavailable
}

// field is one top-level entry. An embedded field has no name of its own:
// its value is an object whose members are spliced in at this position.
type field struct {
	name  string
	value any
	embed bool
}

// Doc starts a document: its own address, its kind, and the title a screen
// would show. An empty title is left out rather than written as "".
func Doc(self, kind, title string) *Envelope {
	return &Envelope{self: self, kind: kind, title: title}
}

// Field adds one of the kind's own fields at the top level.
func (e *Envelope) Field(name string, v any) *Envelope {
	e.fields = append(e.fields, field{name: name, value: v})
	return e
}

// Embed splices the members of v — anything that marshals to a JSON object —
// into the document at this position, in v's own field order. It is how a
// document carries a type the rest of the codebase already publishes
// (works.Work) without restating its fields, and so without drifting from
// them when one is added.
//
// A member whose name is already taken is DROPPED, the earlier value
// standing: the envelope's own four names are never shadowed, and a handler
// that wants a colliding field under another name writes it out first (see
// the work document's work_kind).
func (e *Envelope) Embed(v any) *Envelope {
	e.fields = append(e.fields, field{value: v, embed: true})
	return e
}

// Link adds one read to `links`. An empty title is omitted.
func (e *Envelope) Link(rel, href, title string) *Envelope {
	if e.links == nil {
		e.links = map[string]Link{}
	}
	e.links[rel] = Link{Href: href, Title: title}
	return e
}

// Action adds one write to `actions`.
func (e *Envelope) Action(name string, a Action) *Envelope {
	if e.actions == nil {
		e.actions = map[string]Action{}
	}
	e.actions[name] = a
	return e
}

// Unavailable records an action this document does not afford, and why.
func (e *Envelope) Unavailable(name, reason string) *Envelope {
	if e.unavailable == nil {
		e.unavailable = map[string]Unavailable{}
	}
	e.unavailable[name] = Unavailable{Reason: reason}
	return e
}

// Self is the document's own address.
func (e *Envelope) Self() string { return e.self }

// Kind is the document's kind.
func (e *Envelope) Kind() string { return e.kind }

func (e *Envelope) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	seen := map[string]bool{}
	write := func(name string, v any) error {
		if seen[name] {
			return nil // first writer of a name keeps it (see Embed)
		}
		val, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("hyper: field %q: %w", name, err)
		}
		key, err := json.Marshal(name)
		if err != nil {
			return err
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
		seen[name] = true
		return nil
	}

	if err := write("self", e.self); err != nil {
		return nil, err
	}
	if err := write("kind", e.kind); err != nil {
		return nil, err
	}
	if e.title != "" {
		if err := write("title", e.title); err != nil {
			return nil, err
		}
	}
	for _, f := range e.fields {
		if !f.embed {
			if err := write(f.name, f.value); err != nil {
				return nil, err
			}
			continue
		}
		if err := embed(f.value, write); err != nil {
			return nil, err
		}
	}
	if len(e.links) > 0 {
		if err := write("links", e.links); err != nil {
			return nil, err
		}
	}
	if len(e.actions) > 0 {
		if err := write("actions", e.actions); err != nil {
			return nil, err
		}
	}
	if len(e.unavailable) > 0 {
		if err := write("unavailable", e.unavailable); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// embed marshals v and hands each of its members to write, in v's own order
// — a json.Decoder reports an object's keys in document order, which a
// map[string]json.RawMessage would lose.
func embed(v any, write func(string, any) error) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("hyper: embed: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("hyper: embed: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("hyper: embed: %s is not a JSON object", raw)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return fmt.Errorf("hyper: embed: %w", err)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return fmt.Errorf("hyper: embed: %w", err)
		}
		if err := write(key.(string), val); err != nil {
			return err
		}
	}
	return nil
}

// Problem is an RFC 7807 refusal, served as application/problem+json.
//
// Type is a short stable token the client may switch on ("no-such-item",
// "passage-on"), Title the one-line human form, Detail this instance's
// particulars, and Remedy what would make the action available — the field
// that makes a refusal an answer rather than an exception.
type Problem struct {
	Type   string  `json:"type"`
	Title  string  `json:"title"`
	Status int     `json:"status"`
	Detail string  `json:"detail,omitempty"`
	Remedy *Remedy `json:"remedy,omitempty"`
}

// Remedy is what would make the refused action available: a sentence, and
// where to go for it when somewhere is where to go. It is an object rather
// than a bare string precisely so it can carry that link without the client
// having to parse prose for a URL.
type Remedy struct {
	Text string `json:"text"`
	Link *Link  `json:"link,omitempty"`
}

// Refuse builds a problem without a remedy; WithRemedy adds one.
func Refuse(status int, typ, title, detail string) Problem {
	return Problem{Type: typ, Title: title, Status: status, Detail: detail}
}

// WithRemedy names what would make the action available, and optionally
// where to go for it.
func (p Problem) WithRemedy(text string, link *Link) Problem {
	p.Remedy = &Remedy{Text: text, Link: link}
	return p
}

// WriteDoc answers with one document. The body is marshalled BEFORE any
// header is set, so a document that cannot be marshalled becomes a problem
// rather than a truncated 200.
func WriteDoc(w http.ResponseWriter, status int, e *Envelope) {
	body, err := json.Marshal(e)
	if err != nil {
		WriteProblem(w, Refuse(http.StatusInternalServerError, "document-broken",
			"The server could not render this document", err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// WriteProblem answers with a refusal, at the problem's own status.
func WriteProblem(w http.ResponseWriter, p Problem) {
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	body, err := json.Marshal(p)
	if err != nil { // a Problem is four strings and an int; this cannot fail
		body = []byte(`{"type":"document-broken","title":"unrenderable problem","status":500}`)
		p.Status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	w.Write(append(body, '\n'))
}
