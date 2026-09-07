# flickr's hypermedia envelope

**Decision (2026-09-07):** flickr becomes backend-driven with its own
hypermedia envelope, and does not become a waymark application. The
server says what exists and what may be done to it; the browser renders
what it is told and owns only the device — the media element, hls.js,
the cast session, the reader's page rendering, the play gesture. waymark
stays the hub that mirrors flickr's feed; nothing in this design changes
`/api/feed/media`.

## Why

The browser held real knowledge of its own: it regrouped items into
shows, albums and artists (mirroring `works.Build` and `works.Artists`),
it alone knew the passage grammar and its three rules, it resolved
`#/show/<title>?ep=S03E22` into an item, and it minted passage links.
Every failure on 2026-09-07 was a fat-client failure: a shell three
deploys stale, an autoplay refusal swallowed, a route the old client did
not know. A rule that lives only in a client is manners, not a rule.

## The envelope

Every document is one JSON object of this shape. Fields the kind carries
sit at the top level; the envelope adds four:

```json
{
  "self":   "/api/items/2090",
  "kind":   "item",
  "title":  "Beach Games",
  "...":    "the kind's own fields",
  "links":  { "work": {"href": "/api/works/show%3Athe-office", "title": "The Office"},
              "poster": {"href": "/api/items/2090/poster"},
              "next":  {"href": "/api/items/2091", "title": "The Job"} },
  "actions": {
    "play": { "method": "POST", "href": "/api/items/2090/play",
              "input": { "seek_seconds": "number?", "passage": "passage?" },
              "label": "▶ Play" },
    "progress": { "method": "POST", "href": "/api/items/2090/progress",
                  "input": { "position_seconds": "number" } }
  },
  "unavailable": {
    "read": { "reason": "a film is played, not read" }
  }
}
```

- `self` is the document's own address. Addresses are opaque to the
  client: it follows `links` and `actions`, and the only URL it knows by
  heart is the root, `/api/`.
- `links` are reads, keyed by relation. `actions` are writes the caller
  may make NOW, keyed by name, each with `method`, `href`, an `input`
  sketch (field → type, `?` optional) and a `label` the screen shows.
  `unavailable` names actions the kind has that this document does not
  afford, with a reason in words — a button that is not there, and why.
- Collections are envelopes whose `items` are envelopes:
  `{ "self", "kind": "…_collection", "items": [...], "links": {...} }`.
- Errors are RFC 7807 `application/problem+json` with `type`, `title`,
  `status`, `detail`, and `remedy` — what would make the action
  available. A refusal is an answer, not an exception.
- `input` is a sketch, not JSON Schema: enough for a screen to build a
  control and for a reader to know what to send. Values cross as JSON.

The package is `internal/hyper`: `Envelope`, `Link`, `Action`,
`Problem`, and a `Doc` builder that every handler uses. Handlers stop
writing bare maps.

## The documents

| Document | Address | Carries | Actions |
|---|---|---|---|
| root | `GET /api/` | profile, links to library, search, continue, artists, scan, system | `scan` |
| library | `GET /api/library` | tiles in server order — shows, films, artist shelves, audio works, books; genre facets; each tile a link + artwork | — |
| work | `GET /api/works/{key}` | the work's fields, `members` in order (each an item envelope, with its chapters or sections), `places` (chapter/episode/section tokens **with labels**), the profile's progress | `play` (representative or next unfinished), `read` (a book), `passage` (mint a link from `from`/`to`) |
| item | `GET /api/items/{id}` | `media_info`, chapters, subtitles, artwork links, `links.work`/`prev`/`next` | `play`, `read`, `progress`, `identity`, `reprobe`, `enrich` |
| session | `GET /api/sessions/{id}` | `url`, `method`, the decision trace, `passage` (`t`, `end`, `until`, resolved item ids), `ends_at` | `progress` (refused while a passage is on), `keep_watching`, `stop`, `next` (the up-next document), `mark_in`, `mark_out`, `link` |
| artist | `GET /api/artists/{name}` | albums in year order, cover | — |
| search | `GET /api/search?q=…` | `groups` (works, artists, episodes, tracks, parts, books), each a headed list of hits — a work's own tile, an artist's shelf tile, or a member with its label, its work and its artwork | — |
| continue | `GET /api/continue` | entries as item envelopes with the resume position and the work link | per entry: `resume` |
| route | `GET /api/-/route?hash=…` | `view` (`library` \| `work` \| `artist` \| `item` \| `search`), the `document` to render, `passage` (resolved, or null), `autoplay` | — |
| passage | `GET /api/-/passage?…` | a minted link and its sentence (`S03E22 2:22 – 14:14 of Beach Games`) | — |

`GET /api/items` and `GET /api/works` remain as flat lists for the
feed and for tools; the browser stops reading them.

## Routes are resolved by the server

The hash stays human — `#/show/The%20Office?ep=S03E22&t=142&end=854`,
`#/item/2090?t=142&end=854`, `#/item/2332?from=ch:3&to=ch:3`,
`#/artist/Radiohead`, `#/search/beach%20games`, `#/` — and every link minted so far keeps
working. But the client no longer parses it beyond splitting the hash:
it asks `GET /api/-/route?hash=<hash>` and renders the answer. The
server resolves an episode code against the show's members, a text
locator against the book's sections, an unknown title into a problem
with the library as the remedy. The passage grammar leaves `passage.js`
for `internal/passage` (Go), one implementation, table-tested.

## Passages are session state

`play` takes an optional `passage` (`t`, `end`, `until`, `from`, `to`).
The session document carries it resolved, and the three rules become
the server's:

1. **Start there** — the server seeds `seek_seconds` from `t`; the
   client never reads `/api/progress` for a passage session.
2. **Stop there** — the client still pauses at `end` (it holds the
   clock), but what it shows is the session document's
   `actions.keep_watching` and `links.back`; a run's next episode is
   `actions.next`, already stripped of `t`/`end` by the server.
3. **No progress is written** — `POST …/progress` during a passage
   answers `409 application/problem+json`, `type: passage-on`, `remedy:
   keep_watching`. The client not sending is no longer the rule.

`keep_watching` clears the passage on the session; progress writes are
accepted from then on. `mark_in` / `mark_out` post a position (the
client's clock) and the server keeps the marks on the session;
`actions.link` answers the minted passage link and its sentence.

## The client

`web/index.html` becomes a kernel and renderers:

- **kernel**: `splitHash` → `GET /api/-/route` → `render(view, document)`;
  `hashchange` does the same. A `<video>` element and the reader
  container persist across renders, so a document swap never drops the
  buffer.
- **renderers**: one per document kind (`library`, `work`, `artist`,
  `item`, `session`, `continue`), each a pure function of the envelope
  that draws the fields and a control per entry in `actions`. A renderer
  never composes a URL: it uses `links[rel].href` and
  `actions[name].href`.
- **device**: `player.js` (media element, hls.js, cast, the position
  tick, the scrub bar, chapter ticks, end-of-passage pause), `reader.js`
  and `pdfreader.js` unchanged in what they do, now handed a document.
- `web/passage.js` shrinks to the clock predicates (`passageEnded`,
  `textPassageEnded`); the grammar and the marks logic are gone.
- The shell stays cache-first with `Cache-Control: no-cache` from the
  server and `cache: 'reload'` precaches (the 2026-09-07 rule); CI fails
  a shell change without a `sw.js` bump.

## Tests

- Go: golden JSON per document kind (`testdata/*.json`), the route
  resolver's table (every hash spelling in the README), the passage
  rules as handler tests (a progress write during a passage is 409; after
  `keep_watching` it is 200), `internal/passage` grammar tables.
- JS: the renderers are pure functions over envelopes, table-tested
  under `node --test` with fixtures taken from the Go goldens, so the two
  sides share one truth.

## Order of work

1. `internal/hyper` + root, item and work documents (`places` with
   labels). No client change.
2. `internal/passage` (grammar from `passage.js`) + `GET /api/-/route`
   with every current hash; the client router calls it.
3. `GET /api/library`, `GET /api/artists/{name}`; the client's
   `buildLibrary`/`audio.js` grouping deleted.
4. Session document, passage as session state, the 409, marks and
   minting server-side; `passage.js` shrinks.
5. Reader documents: `read` action, text passages resolved server-side,
   locator progress gated the same way.
6. Client kernel + renderers: `index.html` split into modules under the
   contract; goldens shared with Go.
7. Cast receiver reads the session document.
8. README's API and deep-link sections rewritten around the envelope.

Each step ships on its own, green, behind the existing routes until the
client is switched; the old JSON shapes are removed only in step 6.
