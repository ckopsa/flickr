# flickr — media server PoC

A proof-of-concept media server over an S3/MinIO bucket, built to demonstrate
a handful of architectural decisions (see design notes below):

1. **Pure-function playback decisions** — `internal/decision.Decide(media, caps, policy)`
   is side-effect-free, table-tested, and every decision returns a trace
   answering "why did this transcode?" (shown in the UI).
2. **Versioned capability negotiation** — clients declare what they can play
   (`ClientCapabilities`, `schema_version` field); the server never hardcodes
   device models.
3. **Isolated FFmpeg boundary** — `internal/pipeline` is the only package that
   knows FFmpeg exists. `BuildArgs` (job → argv) is pure and unit-tested;
   sessions own the subprocess and HLS output.
4. **Split storage** — `data/library.db` (mostly-static metadata) and
   `data/state.db` (high-churn playback positions) are separate SQLite files,
   both WAL; a scan can never lock out position writes.
5. **Incremental, job-based scanning** — list bucket → diff etags → probe only
   changed objects (bounded worker pool) → batch-write in transactions →
   reconcile deletions. Restarting never re-probes the whole library.
6. **Identification ≠ enrichment** — object key → identity is deterministic
   and separately stored; user overrides (`POST /api/items/{id}/identity`) are
   never clobbered by a rescan. Identification is directory-aware
   (`Shows/<name>/Season <n>/`, `Movies/<Name (Year)>/`) and versioned:
   bumping `IdentityVersion` makes the next scan recompute identities from
   keys alone — no re-probe, no bandwidth. Nothing under a category directory
   stays unidentified (v3): bonus material (`Extras/`, `Featurettes/`,
   `Deleted Scenes/`) becomes kind `extra` attached to its show or film —
   deleted scenes named `S01E01 ...` must not masquerade as the real S01E01 —
   an unnumbered file under `Shows/<name>/` is an episode of that show
   (episode 0 = nobody numbered it), and a year-less file under `Movies/` is
   still that movie. The result is a library where every file hangs off a
   movie or a show, and only a movie or a show is ever a tile.
   TMDB enrichment (title, overview,
   poster, genres) is a separate post-scan stage keyed off identity;
   episodes additionally get per-episode title/overview/still from one
   `/tv/{id}/season/{n}` call per show-season per run. Enrichment is
   invalidated automatically when an item's identity changes, is versioned
   (a `"v"` marker inside the stored JSON re-enriches rows that predate
   newer fields), and is disabled entirely without a `TMDB_API_KEY`.
7. **Verified hardware-accel detection** — at startup the pipeline probes
   nvenc/qsv/vaapi/v4l2m2m with real test encodes (compiled-in ≠ working)
   and keeps the full accept/reject trace (`GET /api/system`). The decision
   engine stays hardware-agnostic; encoder choice happens at the FFmpeg
   boundary, including per-family quirks (preset namespaces, vaapi hwupload).
8. **Self-maintaining sessions and scans** — transcode sessions track when
   their HLS output was last fetched; a reaper stops sessions idle >5 minutes
   (vanished clients never say goodbye). Scans run on a schedule
   (`SCAN_INTERVAL`, default 12h; an empty library triggers one at startup),
   and probing extracts subtitle streams and the full audio-track list
   (`media_info.audio_tracks`, ordinal-mapped to `-map 0:a:N`; the scalar
   first-stream fields remain the decision engine's input). Text subtitle
   tracks are served as WebVTT (`.vtt` endpoint, extracted on demand with
   ffmpeg, cached in `data/subs/`), bitmap tracks are listed but marked
   unsupported. External subtitle sidecars (`Movie.en.srt` next to
   `Movie.mkv`; `.srt`/`.ass`/`.ssa`/`.vtt` — `.sub` is skipped as
   ambiguous MicroDVD/VobSub) are discovered during the scan's listing pass
   and appended after the embedded tracks with `"external": true`; a
   sidecar fingerprint in the skip decision means adding or removing an
   `.srt` re-attaches tracks on the next scan without re-probing the video.
9. **ABR only when decode is already being paid for** — a video re-encode
   carries an adaptive-bitrate ladder (primary rung + 720p/1.2–3 Mbps rungs
   below it, capped by the client's `max_height`): one decode, per-variant
   scale+encode, one HLS master playlist (`master.m3u8`). Copy/remux and
   audio-only transcodes stay single-rendition — the ladder never causes a
   decode that the decision engine didn't already order.
10. **Trickplay previews as a post-scan stage** — after scan + enrichment,
   sprite sheets (1 frame/10 s, 320px tiles, 10x10 grids) are generated one
   item at a time under `data/trickplay/<id>/`, written atomically, yielding
   to live playback exactly like scans do; `TRICKPLAY=0` disables the stage.
   The web UI shows hover previews on the scrub bar when the sidecar exists.
11. **Profiles and telemetry stay lightweight** — user profiles are names in
   `state.db` (`/api/users`); clients pass the profile name as `client_id` to
   the unchanged progress API. `POST /api/telemetry` appends any JSON object
   to `data/telemetry.jsonl` for client-side error forensics.
12. **Self-description feed** — the library can describe itself as WORKS
   rather than files: `internal/works` purely derives one work per movie and
   one per show (episodes grouped by identity title, case-insensitively;
   bonus material joins the work it belongs to as a non-episode member, so it
   is reachable but never a work of its own; unidentifiable files stay visible
   as `file:<id>` works), keyed by TMDB id
   (`tmdb:<id>`) with a deterministic slug fallback (`show:ninjago`,
   `movie:frozen-2013`). Per-audience progress is recomputed on demand, never
   stored, and always ships the machine-readable fraction beside the
   authority's own human text (`0.31` + `"S02E05 · 12:30"`) — an episode
   counts watched at ≥90% of its duration, the furthest partial episode adds
   its fraction, a work finishes at ≥0.9; extras count toward none of it, and
   finishing an episode never advances into a featurette. Change detection uses monotonic
   sequence counters, not timestamps: every library/playback row write stamps
   a per-database `seq`, and the feed cursor is the counter pair
   `l<libSeq>.s<stateSeq>` — a work is "changed" when any member item or any
   playback row on it outruns the cursor. Deletions don't leave tombstones;
   they bump a deletion mark that forces the next fetch into a full resync
   (always correct, just not minimal). Rows from before the migration carry
   seq 0, so a first cursorless fetch returns everything — the correct
   initial sync.
13. **Medium is a property of the file, not of the server** — every probed
   item carries `media_info.medium` (`video`, `audio`, `text`; rows written
   before the field existed are read as `video`), the scan admits files by
   an extension table per medium, and probing routes on it: audio goes
   through ffprobe like a film (cover art is never mistaken for a video
   stream, so an audio item has `video_codec ""` and no dimensions), text
   through an EPUB reader (`ProbeEpub`: container.xml → OPF → metadata,
   spine, TOC) that yields one `chapter` per spine item and a `sections`
   count. Identification gained the `Audiobooks/`, `Music/` and `Books/`
   grammars (see *Library layout*), and works gained `medium`, `author`,
   and the `audiobook`/`album`/`book` kinds. Audio plays (14) and text is
   read, not played (15): the play endpoints answer 415 for text, and
   trickplay skips audio and text alike (no frames to draw).
14. **Audio plays through the same pipeline, minus the picture** — an audio
   item takes its own branch of the pure `Decide`: the container, codec,
   channel and bitrate questions are asked, the video ones (codec,
   resolution, HDR, ladder) never are. Direct play when the client takes the
   file as it is; otherwise an audio-only HLS session (`target.audio_only`:
   `-vn`, the chosen stream copied when the client's HLS player accepts the
   codec in-stream, re-encoded to AAC when it does not, never a ladder —
   there is no decode to spend). Cover art is the file's attached picture,
   extracted once on first request into `data/covers/<id>.jpg` and served by
   `/api/items/{id}/cover`, which falls back to the poster route (and
   remembers a file with no picture, so it is asked once). The web UI plays
   audio through the very same player as a film — media element, hls.js,
   scrub bar, chapter ticks, casting, the `saveProgress()` gate and the
   passage predicates — with `web/audio.js` putting the cover where the
   picture would be and the work's parts or tracks beside it: an
   audiobook's parts and an album's tracks play in order, up-next walks
   them the way it walks episodes, and a chapter tap seeks. Progress text
   for a chaptered single-file book reads `ch. 7 · 1:19:22 / 11:30:00`
   (the chapter from `media_info.chapters` by position), a set keeps
   `part 3 of 12 · 41:10`, and continue-listening rides `/api/continue`
   unchanged. **Music is the same machinery, used the way the house uses
   it — a record on in the kitchen.** An album is the work, its tracks play
   in order through the parts list and up-next, and a track keeps its own
   name: identity v5 reads `Music/<Artist>/<Album (Year)>/<07 Karma
   Police.flac>` into `identity.track_title` (the filename after the
   ordinal, spelled as written — dots and hyphens kept, underscores to
   spaces; the whole name when there is no ordinal), so the pane lists
   `Karma Police` with `Track 7` beneath it and a label reads `Track 7 ·
   Karma Police`. Above the album sits the **artist**: `/api/artists` (and
   `works.Artists`, a pure grouping over the album works) shelves albums by
   artist, case-insensitively, in year order with the year-less last, and
   names the item whose cover is the artist's picture; the grid shows one
   tile per artist, `#/artist/<name>` lists the shelf with covers and years,
   an album opens its pane, and Back from a track returns to the shelf. An
   album nobody filed under an artist keeps a tile of its own. No playlists,
   no shuffle, no lyrics: the unit is the record, and the order is the
   record's.
15. **Text reads in a reader, and a place in a text is a locator** — a
   book is opened by `POST /api/items/{id}/read`, which answers a reading
   session naming `links.book` (the epub bytes off the bucket,
   range-capable), and read by a reader pane on epub.js, vendored under
   `web/vendor/` with JSZip and precached by the service worker, so the
   LAN-only engine loads nothing from the internet at read time. The pane
   shows the session's contents (`sections`, one per spine item), pages,
   and reports where the reader is as an EPUB **CFI** plus the book's own
   **percentage**: `playback_state` gained a nullable `locator TEXT` column
   beside `position_seconds`, holding `{"cfi","section","fraction"}` as
   JSON — the same row, a different unit. `section` is the 1-based spine
   position; the client sends it, or the server reads it off the CFI's
   spine step (`epubcfi(/6/14!…)` → 14/2 = 7). The fraction law gets its
   text unit: a book's `progress` is the stored fraction and its
   `progress_text` is `ch. 7 · 34%`; finished at ≥ 0.9 like everything
   else; the continue row keeps a book by its locator and hands the client
   the `fraction`. Text passages (`?from=…&to=…`, see *Deep links*) obey
   the three passage rules unchanged: open at `from`, stop at `to` with
   *End of the passage*, write nothing. A book's cover — the image its OPF
   names — is extracted once on first request into `data/covers/<id>.jpg`
   and served by the same `GET /api/items/{id}/cover` an audio item's
   attached picture is (14); tiles ask `/cover` for every non-video item.
16. **A PDF is a book with pages instead of sections** — `.pdf` under
   `Books/` is admitted as the same `book` kind and `text` medium as an
   EPUB, and everything from 15 applies with the page as the unit. Probing
   reads the file's own structure with no external tool (`ProbePDF`):
   trailer → cross-reference → Catalog → page tree → `/Count`, through
   classic xref tables, PDF 1.5 xref streams (FlateDecode, PNG predictors)
   and object streams alike, and when the table lies or is missing it scans
   the file for the page tree instead; the count lands in
   `media_info.page_count` (0 = the file would not say; the probe never
   fails on that) and the Info dictionary's `/Title` and `/Author` in
   `media_info.document`. The reader pane is pdf.js, vendored like epub.js
   (`web/pdfreader.js` behind the same `Reader` facade `web/reader.js`
   exports): one page at a time on a canvas fit to the pane's width, Prev /
   Next, a page field, the arrow keys; `GET /api/items/{id}/book` serves it
   as `application/pdf` and pdf.js reads it by Range. The place is the page:
   the same `locator` column holds `{"page": 213, "fraction": 0.5325}`,
   `POST /api/progress` takes `page` beside the EPUB fields, and the
   fraction law gets its page unit — `progress` is page over the probed
   count (the server's own arithmetic; the reader's fraction stands in when
   the count is unknown) and `progress_text` reads `p. 213 / 400` (or `p.
   213`); finished at ≥ 0.9, continue-reading by page, page 1 the cover
   nobody resumes to. The locator grammar gains `pg:<n>`: `#/item/<id>?from=
   pg:213&to=pg:240` opens at page 213 and ends the passage when page 240 is
   the page shown — inclusive, the page stays on view, *End of the passage*
   appears and Next turns no further — and writes nothing, the three passage
   rules unchanged. A PDF has no named cover: `/cover` answers 404 without a
   download and the tile falls back as for any coverless item.

## Library layout

**The grid's order is the server's.** `GET /api/library` lists the tiles in
one order and the browser draws them in it: **shows, films, artist shelves,
audio works (audiobooks, and any album nobody filed under an artist), books**
— each band in the works' own title order, artists by name. An album filed
under an artist gets no tile of its own; its artist's shelf
(`#/artist/<name>`, `GET /api/artists/{name}`) stands for it. Bonus material
never earns a tile at all: it hangs off the show or the film it belongs to. A
file the path could not place lands in the band its medium belongs to, so
nothing in the bucket is invisible. The search box and the genre chips FILTER
that list — they never regroup it (`libraryOrder`, `cmd/server/library.go`,
table-tested).

Identification reads the object key only (`scanner.Identify`), so the
bucket's directory layout IS the catalogue. Category directories are
matched case-insensitively, anywhere on the path (`csi-fs/Adult/Shows/…`
works), and nested category directories are walked through
(`tv/Shows/…`, `Audio/Audiobooks/…`). Nothing under a category directory
is ever `unknown`: whatever does not fit a shape below still resolves to
that category's kind with what the path gives.

| Category | Shape | Identity |
|---|---|---|
| `Movies/` (`Films/`, `Documentaries/`) | `Movies/<Title (Year)>/<file>`, `Movies/<Title (Year)>.mkv`, `Movies/<Title>/<file>` | kind `movie`, title, year when present; `Extras/` below a title → kind `extra` |
| `Shows/` (`TV/`, `Series/`) | `Shows/<Show (Year)>/Season <n>/<S01E02 or E02 or 1x02 or 01 - Name>.mkv`, `Shows/<Show>/<file>` | kind `episode`, title = show, season/episode from the filename then the season directory (0 = unnumbered); `Featurettes/`, `Deleted Scenes/` → `extra` |
| `Audiobooks/` | `Audiobooks/<Author>/<Title (Year)>/<03 - Chapter Three.m4b>`, `Audiobooks/<Author>/<Title>.m4b` | kind `audiobook_part`, author, title, year, `part` from the filename's leading ordinal (`03 - …`, `Part 3`, `03`; 0 when absent or for a single-file book) |
| `Music/` (`Albums/`) | `Music/<Artist>/<Album (Year)>/<01 Title.flac>` | kind `track`, author = artist, title = ALBUM (the album is the work), `part` = track number, `track_title` = the name after the ordinal (`Title`); a loose `Music/<Artist>/<Title>.mp3` is a one-track album named by the file |
| `Books/` (`Ebooks/`) | `Books/<Author>/<Title (Year)>.epub`, `Books/<Author>/<Title>/<file>.epub` (`.pdf` the same) | kind `book`, author, title (from the file or the directory), year |

Examples, key → identity:

```
Movies/Frozen (2013)/movie.mp4                       movie      Frozen, 2013
Shows/Doctor Who (2005)/Season 1/Ep 3.mkv            episode    Doctor Who, 2005, S01E03
Shows/The Office/Featurettes/S01E01 Deleted.mkv      extra      The Office, S01E01
Audiobooks/Frank Herbert/Dune (1965)/03 - Part 3.m4b audiobook_part  Frank Herbert · Dune, 1965, part 3
Audiobooks/Ursula K. Le Guin/The Dispossessed.m4b    audiobook_part  Ursula K. Le Guin · The Dispossessed, part 0
Music/Radiohead/OK Computer (1997)/01 Airbag.flac    track      Radiohead · OK Computer, 1997, part 1 "Airbag"
Books/George Orwell/1984.epub                        book       George Orwell · 1984
```

What the scan admits, by medium: **video** `.mkv .mp4 .m4v .avi .mov .webm
.ts .wmv`; **audio** `.m4b .m4a .mp3 .flac .opus .ogg .aac .wav`; **text**
`.epub .pdf`. Subtitle sidecars (`.srt .ass .ssa .vtt`) are matched to the
file they sit beside, not admitted on their own.

Vocabulary, from file to work:

| Identity kind | Work kind | Medium | Members ordered by |
|---|---|---|---|
| `movie`, `extra` | `movie` | `video` | the film first, extras after |
| `episode`, `extra` | `show` | `video` | (season, episode), unnumbered last, extras last |
| `audiobook_part` | `audiobook` (`part_count`) | `audio` | `part`, unnumbered last |
| `track` | `album` (`track_count`); albums shelve under an artist (`/api/artists`) | `audio` | `part` |
| `book` | `book` | `text` | — |
| `unknown` / no identity | `file:<id>` | the item's own | — |

Progress follows the shape: an audiobook's fraction is time heard over the
sum of its parts' durations (earlier parts count whole), its text reads
`part 3 of 12 · 41:10`; a single chaptered file reads
`ch. 7 · 1:19:22 / 11:30:00`; a book's fraction is the percentage the reader
reports with its locator, its text `ch. 7 · 34%` (a row written before the
reader existed still reads as a position without a place); a PDF's fraction
is its page over its page count, its text `p. 213 / 400` (`p. 213` when the
probe found no count). Audio plays (design note 14) and text reads (15, 16).

## Run

```sh
cp .env.example .env   # fill in MinIO credentials
go run ./cmd/server
# open http://localhost:8080, hit "Scan library"
```

Requires `ffmpeg`/`ffprobe` on PATH. Media files are read straight from MinIO
via presigned URLs — both for probing and as FFmpeg input; nothing is copied
locally except HLS segments in `data/streams/`.

## Deploy

A merge to `master` that touches `cmd/`, `internal/`, `web/` or the Go
module deploys itself (`.github/workflows/image.yml`): on the household's
`flickr` runners the server is cross-compiled for arm64, appended as one
layer to the jellyfin base (its arm64 ffmpeg carries Rockchip MPP) with
crane — no docker daemon — and pushed to `docker.kopsa.info/flickr:<short
sha>` and `:latest`. Then one Nomad variable is written,
`nomad/jobs/flickr/deploy image_tag=<short sha>`; the job's template
restarts the task on the change and `force_pull` fetches the tag. The
job, the variable's ACLs and the by-hand and rollback recipes live in
`ckopsa/home-infrastructure` (`terraform/nomad-jobs/flickr.hcl`,
`terraform/docs/flickr-stream/RUNBOOK.md`). Without the `NOMAD_ADDR` /
`NOMAD_TOKEN` repository secrets the workflow pushes the image and
leaves the variable write to a person.

## API

| Endpoint | Purpose |
|---|---|
| `POST /api/scan`, `GET /api/scan` | trigger / observe incremental scan (enrichment runs after) |
| `GET /api/items` | library listing (incl. `media_info.medium`, `media_info.subtitles` and `enrichment`) |
| `GET /api/works` | library as works: movies, whole shows, audiobooks, albums, books (+ stray files), title-sorted, each with `work_key`, `kind`, `medium`, `author`, counts, and a `representative_item_id` for artwork |
| `GET /api/works/{key}/items` | one work's member items (episodes in season/episode order, parts and tracks in part order, each track with its `identity.track_title`); 404 for unknown keys |
| `GET /api/artists` | the album works shelved by artist: `name`, `album_count`, `albums` in year order (`work_key`, `title`, `year`, `track_count`, `representative_item_id`), and the artist's own `representative_item_id` — the item whose `/cover` is the picture; name-sorted, `[]` without music |
| `GET /api/continue?client_id=NAME` | a profile's resume list as a **document** (below): most-recent first, max 20, finished (≥90%) and <5 s positions excluded, one entry per work |
| `GET /api/feed/media?since=CURSOR` | change feed: works changed since the cursor with per-audience progress; response carries the next cursor (`l<n>.s<n>`); no `since` = everything |
| `POST /api/items/{id}/decision` | dry-run: decision + trace, no side effects; audio items take the audio branch (415 for text — books are read, not streamed) |
| `POST /api/items/{id}/play` | decide and act: presigned URL (direct) or HLS session (transcode, audio-only for audio items); 415 for text. Answers the **session document** (below), and takes an optional `passage` |
| `POST /api/items/{id}/read` | open a book: answers the **reading session** (below) — `links.book`, `sections` (or `page_count`), the profile's saved `locator`, the `passage` resolved to sections or pages — and takes an optional `passage`. 415 for anything that is not text |
| `POST /api/items/{id}/identity` | user identity override (persists across scans) |
| `POST /api/items/{id}/reprobe` | re-probe one item on demand (also re-discovers subtitle sidecars in its directory) |
| `POST /api/items/{id}/enrich` | TMDB-enrich one item on demand (503 if no API key) |
| `GET /api/items/{id}/subtitles/{ordinal}.vtt` | subtitle track as WebVTT — embedded or external sidecar (415 for bitmap tracks, 404 for bad ordinal) |
| `GET /api/items/{id}/poster` | cached TMDB poster (image/jpeg, 404 if absent) |
| `GET /api/items/{id}/cover` | an audio item's embedded cover art, extracted with ffmpeg on first request and cached in `data/covers/`; falls back to the poster route, 404 when neither exists |
| `GET /api/items/{id}/still` | cached TMDB episode still (image/jpeg, 404 if absent) |
| `GET /api/items/{id}/book` | a text item's bytes for the reader (`application/epub+zip`, or `application/pdf` for a `.pdf`; Range requests honoured); 415 for anything that is not text |
| `GET /api/items/{id}/cover` | the item's own cover art: a book's OPF cover image, extracted once into `data/covers/`; 404 when the book names none (a PDF never does), 415 for video (whose artwork is the poster) |
| `GET /api/items/{id}/trickplay.json` | scrub-preview sprite index (404 if not generated) |
| `GET /api/items/{id}/trickplay/{n}.jpg` | sprite sheet n |
| `POST /api/items/{id}/trickplay` | force-generate sprites for one item (synchronous) |
| `GET /api/sessions/{id}` | the session document: url, method, decision trace, the passage resolved, the marks, and the actions below |
| `POST /api/sessions/{id}/progress` | the place, written to the same store as `/api/progress` — **409 `passage-on`** while a passage is on |
| `POST /api/sessions/{id}/keep_watching` | leave the passage; progress writes are accepted from then on. Answers the session |
| `POST /api/sessions/{id}/keep_reading` | the same, in the words of the medium: a reading session's way out of a text passage |
| `POST /api/sessions/{id}/next` | start the work's next member on the same device, carrying a run's `until` forward. Answers the NEW session |
| `POST /api/sessions/{id}/mark/{in\|out}` | set one end of a passage at `{"seconds": n}` (or `{"clear": true}`); snapped, swapped and turned into a run by the server. Answers the session |
| `GET /api/sessions/{id}/link` | the marked passage: the absolute link and the sentence that says it; 409 until an in point exists |
| `GET /api/-/passage?item=&t=&end=[&until=]` | the same, composed rather than marked |
| `GET /api/-/passage?work=&from=&to=` | …or composed from two of the `places` the work document publishes (`S03E22 2:22`, `1:19:00`, `ch. 3`): the server resolves them against the work's members, swaps a backwards pair, and turns a later member into the run's `until` |
| `DELETE /api/sessions/{id}` | stop a session: the transcode, if any, and the row (idle sessions are auto-reaped) |
| `POST/GET /api/progress` | playback position per (item, client): `position_seconds` for video/audio; for text also `locator` (an EPUB CFI), `fraction` (0..1, the book's own percentage; 400 outside that range) and optional `section` (1-based spine index, derived from the CFI when omitted); for a PDF `page` (1-based; 400 when negative) with the same `fraction` (page over count). GET returns the text fields only when the row holds a locator |
| `GET/POST /api/users` | list / idempotently create profile names (clients use the name as `client_id`) |
| `POST /api/telemetry` | append any JSON object to `data/telemetry.jsonl`; always 200 |

`decision`/`play` take `{"capabilities": {...}, "client_id": "...", "seek_seconds": 0}`,
plus optional playback selections: `"audio_track": <ordinal>` (the decision
engine evaluates the chosen track's codec/channels and the pipeline maps
`-map 0:a:N`; direct play is untouched — the client negotiates natively) and
`"subtitle_burn": <ordinal>` (burn an embedded bitmap subtitle track into the
picture; forces a video re-encode, overlay applied before all other filters).
The web UI includes capability presets (Chromecast v1, 4K HDR TV, cellular cap)
to demonstrate how the same file direct-plays or transcodes per client.

### The hypermedia documents

flickr is becoming backend-driven: the server says what exists and what may
be done to it, and the browser renders what it is told (`docs/hypermedia.md`
has the envelope, the document table and the order of work). Nine of those
documents are served now, beside — not instead of — everything above, and the
browser reads every one of them, the root included and nothing else: the
kernel starts at `/api/`, follows `actions.route` to learn what a hash means,
and renders the answer. There is no other address in the client (*The
client*, below).

| Endpoint | Document |
|---|---|
| `GET /api/` | the root: the profile (`client_id`, query or cookie), links to library, continue, artists, works, items, scan, system and profiles, and the actions `scan`, `create_profile` and `route` — the address that turns a hash into a document. It is the only address a client is meant to know by heart, which is why the resolver is named here rather than learned |
| `GET /api/items/{id}` | one item: its fields, `media_info` (chapters, sections, page count), `tech` (the grey line: what the file IS) and `overview` (the episode's own synopsis, or the work's), `year` where a title takes one, `extra: true` for bonus material, `resume` — where the asking profile left off, so nothing has to ask a progress route for it — its subtitle tracks with their `.vtt` addresses, artwork links, `links.work`/`prev`/`next` (the work's own order, bonus material aside), and the actions `play`, `read`, `progress`, `identity`, `reprobe`, `enrich` |
| `GET /api/works/{key}` | one work: the fields `/api/works` publishes, `members` in order as item envelopes, `places` (the passage grammar's tokens with the labels a chip shows — `S03E22 0:00` · "S03E22 · Beach Games", `1:19:00` · "Let It Go", `ch. 7` · "The Cellar"), the profile's progress, and the actions `play`, `read`, `passage` |
| `GET /api/library` | the grid: `items`, one tile per thing in the order the grid draws them (see *Library layout*), a `genre` facet for the chip row, and `count`. A tile is a small envelope — `self` and `links.self` (the work or artist document), `kind` (`work`/`artist`), `title`, `subtitle` ("3 seasons · 42 episodes", "Radiohead · 1997 · 12 tracks", "3 albums"), `tech`, `medium`, `work_kind`, `year`, `genres`, `item_id` (the member a tap opens) and `links.artwork`, the poster or the file's own cover, chosen here rather than in the browser |
| `GET /api/artists/{name}` | one artist's shelf: `name` as the shelf spells it (the match is case-insensitive), `album_count`, `track_count`, `albums` in year order (the year-less last) as the same tiles with their covers, and `links.artwork`. An unknown name is a `no-such-artist` problem with the library as the remedy |
| `GET /api/sessions/{id}` | one play — direct play included, which used to have no id: `url`, `method`, `decision`, `started_at`, the `passage` resolved (with `ends_at`, the bound that applies to THIS item) and the `marks` being made, `links.item`/`back`/`work`/`next`, and the actions `progress`, `keep_watching`, `stop`, `next`, `mark_in`, `mark_out`, `link` |
| `GET /api/sessions/{id}` (a book) | one reading, opened by `read`: `method: "read"`, `format` (`epub`/`pdf`), `etag`, `sections` (the contents, `index` + `title`) or `page_count`, the profile's saved `locator` (or null), the `passage` resolved (`from`/`to` as locators, the `from_section`/`to_section` or `from_page`/`to_page` they land on, and `ends_at`), `links.book`/`item`/`back`/`work`, and the actions `progress`, `keep_reading`, `stop` |
| `GET /api/continue` | the resume shelf: `items`, one per work, each an item envelope with the `work_title` over its own `label`, `position_seconds` (or a book's `fraction`), `percent` — the bar's width in one unit for every medium — `links.artwork` (the episode's still, the film's poster, the record's or the book's cover, and the work's picture when this one file has none) and one action, `resume`, labelled in the verb the medium uses |
| `GET /api/-/route?hash=` | what a hash means: `view` (`library`, `work`, `artist`, `item`), the `document` to render, the `passage` resolved to item ids and to a book's spine sections or a PDF's pages, and `autoplay` — true for a text passage as for a timed one. See "Deep links and passages" |
| `GET /api/-/passage?…` | a minted passage: its absolute `href` and the `sentence` that says it (`S03E22 2:22 – 14:14 of Beach Games`) — from an item and two times, or from a work and two of the places it publishes |

Every document is one JSON object with `self`, `kind`, `title`, its own
fields, then `links`, `actions` (each with an `input` sketch and a `label`)
and `unavailable` — the actions the kind has that this document does not
afford, each with a reason in words. Errors are RFC 7807
`application/problem+json` with a `remedy`. Every relation these documents
publish now names a document that is served — the last two, `/api/library`
and `/api/-/passage`, landed with the steps above.
`internal/hyper` is the envelope; the goldens are
`cmd/server/testdata/hyper/*.json` (`go test ./cmd/server -run TestHyperGolden -update`
rewrites them).

## Deep links and passages

The web UI is hash-routed and `location.hash` is the only source of truth,
so every view has a stable address:

| Route | View |
|---|---|
| `#/` | library grid |
| `#/show/<title>` | a show's episode list (`<title>` is `encodeURIComponent`'d) |
| `#/artist/<name>` | an artist's shelf: albums with covers, in year order (`<name>` is `encodeURIComponent`'d, matched case-insensitively; an unknown name falls back to `#/`) |
| `#/item/<id>` | one item's detail pane (its Play button resumes from the saved place); for an audiobook part or a track, the audio pane with the work's parts and chapters |

A **passage** is a start and an end within a work — a scene, or a run of
episodes — and other systems (the household's day planner) mint links to
them, so the grammar is fixed. The query rides on the hash path, after the
item route:

```
#/item/<id>?t=<start_seconds>&end=<end_seconds>
           &until=<item_id>           optional: an episode RUN
#/item/<id>?from=<locator>&to=<locator>
                                      a TEXT passage: a run of a book
#/show/<title>?ep=S02E05&t=…&end=…[&until=S02E07]
                                      the same, addressed by episode code
```

- `t` / `end` are seconds into the item, integers or decimals (`4740`,
  `79.5`). An `end` at or before `t` is ignored.
- `until=<item_id>` turns the link into an episode run: playback auto-advances
  through the episodes and stops after the item with that id. `t` applies to
  the first item only; `end`, if given, applies within the last item —
  omit it to let that episode play out.
- `from` / `to` are **locators** for text passages, four spellings of a
  place in a book: `cfi:<epub cfi>` (a point, the form the EPUB reader's own
  progress speaks — a bare `epubcfi(…)` is accepted as the same), `ch:<n>`
  (the n-th spine section, 1-based, as `media_info.chapters` lists them),
  `pct:<0..1>` (the book's own percentage) or `pg:<n>` (the n-th page of a
  PDF, 1-based). The reader opens at `from` and stops at `to`. A `ch:<n>`
  bound is inclusive — the passage runs through section n and ends when the
  reader would turn into n+1, the way `until` plays its episode out; a
  `pg:<n>` bound is inclusive too, and since a page is its own last page the
  passage ends when page n is the page shown (it stays on view, the reader
  turns no further); `cfi:` and `pct:` bounds are points, reached when the
  page shown starts at or after them. The book ending ends any passage.
  Either bound alone is fine (`from` alone: start there, read on; `to`
  alone: from the beginning to there). A `to` before a `from` of the same
  spelling is dropped like an `end` before `t`. A spelling the open book
  cannot read (`pg:` on an EPUB, `ch:` or `cfi:` on a PDF) opens at the
  beginning and bounds nothing; `pct:` works on both.
- The show form is for a minter that knows a show's link and a person's
  spelling of an episode but no item ids: `ep=S02E05` (any case) names the
  episode, `until=S02E07` names the last episode of a run. It is resolved
  against the show's season/episode numbers when opened and the route is
  replaced by the item form, so everything below applies unchanged. An `ep`
  (or `until`) that names no episode is refused in one sentence, and nothing
  plays.

**The server resolves the hash.** The browser splits the hash off the URL and
asks `GET /api/-/route?hash=<hash>` (the address the root names as
`actions.route`), which answers which `view` to render (`library`, `work`,
`artist`, `item`), the `document` to render it from, the `passage` with every
episode code resolved to an item id and every text locator to a spine
section, and whether arriving `autoplay`s. **The kernel renders that answer
and nothing else** — one renderer per document kind, each a pure function of
the envelope (*The client*, below). A title, an
episode or an item that is not there comes back as `application/problem+json`
with a remedy — the library, or the show's own page. The grammar above lives
in `internal/passage` (Go), one implementation, table-tested against the same
cases as `web/passage_test.mjs`; the client parses nothing past the hash.

Examples:

```
#/item/51?t=4740&end=5070              the scene from 1:19:00 to 1:24:30 of item 51
#/item/58?t=120&until=59               episodes 58 and 59, skipping 58's first two minutes
#/show/Ninjago?ep=S02E05&until=S02E06  the same run, spelled by show and episode
#/item/77?from=ch%3A3&to=ch%3A4        chapters 3 and 4 of book 77
#/item/77?from=pct%3A0.4&to=cfi%3Aepubcfi(%2F6%2F14!%2F4%2F2)
                                       from 40% of the book to a marked place in section 7
#/item/80?from=pg%3A213&to=pg%3A240    pages 213 through 240 of PDF 80
```

### Passages are session state

A passage is not a mode the browser puts itself in: it is **state on the
session**, and the three rules are the server's. `POST /api/items/{id}/play`
takes the passage (`{"passage": {"t": 142, "end": 854, "until": 9}}`, item
ids — the show form is resolved before play) and answers the session
document, which carries it back resolved, with `ends_at`: the bound that
applies to the item now playing, which for the earlier items of a run is
nothing at all.

1. **Start there.** The server seeds the seek from `t`; a passage never
   resumes from the saved place, and the client does not ask for it.
2. **Stop there.** The clock is the browser's — it is the one playing — so
   it still pauses at `passage.ends_at` and shows *End of the passage*. What
   the panel offers is the document's: `actions.keep_watching` (labelled as
   the server labels it) and `links.back`. It is not a natural end: no
   up-next countdown, no "finished" mark. Both bounds are flagged on the
   scrub bar and the time readout counts within the passage
   (`2:14 / 5:30 in passage`). A run's next episode is `links.next`.
3. **No progress is written.** A progress write during a passage is
   **refused**: `409 application/problem+json`, `type: "passage-on"`, with
   `keep_watching` as the remedy — on the session's own progress action and
   on the legacy `POST /api/progress` alike, which refuses the same way when
   that item has a live passage session for that profile. The client not
   sending is no longer the rule; the client is simply told no.

The escape hatch is **Keep watching**: `actions.keep_watching` clears the
passage on the session, and progress is accepted from then on. The client
drops the passage from the route too, so a reload is normal viewing.
Pressing play while paused at the end is the same choice. **Back** closes
the player and leaves the bare item route behind. Casting honours all of
this: the sender watches the receiver's time and pauses it at
`passage.ends_at`, the panel it then shows is the same one the local player
shows, and the custom receiver reads that bound off the session document and
stops there for itself as well (see **Casting**).

### Making a passage

The player mints these links too, and **the server keeps the marks and mints
the link**. While watching, **Mark in** and **Mark out** in the control row
post the current position (the cast receiver's position while casting) to
`actions.mark_in` / `actions.mark_out`, and the session document comes back
with the marks as the server kept them: a mark within two seconds of a
chapter start snapped to it, since the scrubber's chapter ticks are the
natural cut points, and an out point before the in point swapped with it —
the person said where the passage is, not which end they meant. A set chip
shows its time (`In 1:19:00`), tapping it again re-marks at the current
position, and its small × clears that end (`{"clear": true}`); the amber
flags appear on the scrub bar exactly as a played passage's do. Once an in
point exists `actions.link` appears — until then it is an `unavailable` with
the reason in words — and **Copy passage link** GETs it for the absolute
`#/item/<id>?t=…&end=…` URL and the sentence that says it (`S03E22 2:22 –
14:14 of Beach Games`), writes the URL to the clipboard and shows both under
the controls as selectable text — a phone on plain `http://` has no
clipboard API but can still long-press and copy. Marks survive an episode
advance within the same work, so marking in on one episode and out after
up-next has moved to a later one makes a run: the server sees the out point
land on a later member and the link carries `&until=<that episode's id>`
with `end` inside it. Marks are dropped when the player closes or another
work starts. Nothing is stored in the library: a passage is its URL, and the
household's day planner is where it is kept. The same link can be composed
rather than marked: `GET /api/-/passage?item=&t=&end=[&until=]` in the
grammar's own spelling, or `GET /api/-/passage?work=&from=&to=` in the
`places` the work document publishes — `from=S03E22 2:22&to=S03E23 14:14` is
that run, `from=1:19:00&to=1:24:30` that scene of the film, `from=ch. 3&to=ch.
4` those two sections of the book. Both answer the same document: the
absolute `href`, the `sentence`, and the item the passage starts in.

A book's passage is the same session. `POST /api/items/{id}/read` opens a
**reading session** — the same envelope, in the units a book has: no stream
and no clock, but `links.book` for the bytes, `sections` (or `page_count`)
for the contents, the profile's saved `locator` to resume from, and the
passage resolved to the sections or the pages its two locators land on. The
three rules hold word for word. It opens at `from` and never at the saved
locator; it shows *End of the passage* on reaching `to` and turns no
further, offering `actions.keep_reading` and `links.back` as the document
labels them; and a locator write while the passage is on is the same **409
`passage-on`**, with **keep_reading** as the remedy — `POST
/api/sessions/{id}/keep_reading` clears it, and the place is saved from then
on. The reader posts its place to `actions.progress` and treats that 409 as
the answer it is: the client not sending is no longer the rule for a book
either.

`web/passage.js` holds the grammar (`parsePassage`, `passageQuery`, and for
text `parseLocator`, `sectionFromCFI`) and both end predicates
(`passageEnded`, `textPassageEnded`) as pure functions — the clock is the
browser's because it is the browser that is playing, and everything else has
gone to the server: which section or page a locator lands in is the reading
session's answer now. `node --test web/passage_test.mjs` runs their tests
without a build step, as `node --test web/cast_test.mjs` does for
`web/cast.js`. The
marking logic left it for the server (`cmd/server/passage.go`, tested in
`cmd/server/session_test.go`): one implementation, and the rules hold for
every client rather than for the one that remembers them.
`web/reader.js` resolves locators against the open EPUB, `web/pdfreader.js`
against the open PDF; both are handed the reading session document and
compose no address of their own.

## The client

`web/` is three files and a device or two, and the whole of what it knows is
that `/api/` exists:

| File | What it is |
|---|---|
| `kernel.js` | the router and the mounting: boot (the profile gate, the service worker, cast), `splitHash` → `actions.route` → `render(view, document, passage, autoplay)`, `hashchange` under a sequence guard, a document cache keyed by `self`, an `api(href)` that reads `application/problem+json` into a `Problem` (its `detail`, its `remedy`), and one delegated click listener. A control carries the href and the method the DOCUMENT gave it; the kernel follows them. The profile rides as a `client_id` cookie, so no href is ever touched |
| `renderers.js` | one renderer per document kind — `library`, `work` (show pane / record pane by `work_kind`), `artist`, `item`, `session` (the player chrome), `continue`. Each is a pure function of the envelope returning HTML: it draws the fields, and a control per entry in `actions` carrying that action's own `label`. A missing action is a control that is not there; an `unavailable` entry is the reason, in words. **A renderer never composes a URL** — every address it emits is `links[rel].href` or `actions[name].href`, and `web/render_test.mjs` greps the source to keep it so. What it does compose is a hash: the hash grammar above is the client's |
| `player.js` | the device: the media element, hls.js, the position tick, the scrub bar and its chapter ticks and passage flags, the volume, the subtitle and audio-track selects, the trickplay previews, the tap-to-play the browser asks for, the end-of-passage pause, and the heartbeat that posts to `session.actions.progress.href` (a 409 there means a passage is on, which is an answer, not an error) |
| `cast.js`, `reader.js`, `pdfreader.js` | the other devices, unchanged in what they do: the cast bridge reads the session document, and a book is read through one |
| `passage.js` | what is left of the grammar on this side: the clock predicates and the hash spellings. The rules are the server's |

`index.html` is markup and script tags, and one line of logic — `Kernel.boot()`.

Two invariants hold the seam:

- **The media element is made once.** No renderer emits a `<video>`; the
  session chrome carries a `#device-slot`, and the device element is moved
  into it synchronously on every re-render. A document swap — a mark set, a
  passage left — must never drop the buffer, and this is how.
- **One address by heart.** `/api/` is the only literal in `kernel.js`, and
  there is none at all in `renderers.js` or `player.js`. `web/kernel_test.mjs`
  asserts both.

## Casting

The web UI is a Google Cast sender: the cast icon in the control row streams
to a Chromecast, and the page becomes the remote (play/pause, ±seek, volume,
stop, scrub bar, chapter jumps). While a cast session is active the client
sends the *Chromecast Ultra's* capability manifest — the decision engine
negotiates for the device that actually plays, not the browser.

### What is cast is the session document

The sender composes nothing. A play answers a session document
(`GET /api/sessions/{id}`, see **Passages are session state**) and the bridge
reads the load straight off it — `url` and `content_type` say what to play,
`method` says whether that is an HLS playlist or a file, the decision's
target says which segment format this device negotiated, `seek_seconds` says
where the bytes begin, `passage.ends_at` says where to stop, and `title`,
`links.work` and `links.artwork` are what the TV shows while it plays. The
one URL the sender builds is the absolute form of an address the document
already gave it, against the LAN base `GET /api/system` advertises. The
subtitle tracks are the item document's, by ordinal, and are the one thing
added here. `web/cast.js` holds all of it as pure functions
(`castMediaSpec`, `sessionEndBound`, `castReceiverBound`, `castStartTime`),
tested by `node --test web/cast_test.mjs`.

Up next while casting is not a cast feature: the natural end of a stream is
a cast `IDLE`/`FINISHED` where locally it is `ended`, and from there both
paths run the same code — `links.next` names the next member, the same
countdown runs, and the advance re-plays through the same route.

### The receiver

`web/receiver.html` is a custom CAF receiver, and it reads the session
document too. The sender puts the document's own address in the load's
`customData`; the receiver fetches it and takes two things from it: the end
bound in its own clock (`castReceiverBound` — a transcode's stream starts at
the seek the server cut it at, so the item's absolute `ends_at` has that
offset taken off), and `links.next`, what follows in the work. It pauses at
the bound itself, once per load. That matters when the sender is a phone
that has locked or left the room: the passage still ends where it says it
ends, instead of running on to the end of the episode. Advancing stays the
sender's — it owns the route, the countdown and the panel — and so does the
clock in the ordinary case: the sender pauses at the same bound from its own
tick, and whichever gets there first the other finds it already paused.
Resuming plays on, which is exactly "keep watching".

Using it needs a Cast developer console registration (a Custom Receiver
whose URL is this server's `/receiver.html`) and the resulting app id in
`localStorage.castAppId`; unset, the sender falls back to the **default
media receiver**, which knows nothing about passages. Everything above still
works there — the default receiver takes the session's `url` and the sender
keeps the clock, pausing it at `passage.ends_at` from its 250 ms tick, a
second or so late at worst. What the custom receiver adds is that the device
holds the bound too, and can say so in its telemetry.

Plumbing that makes it work:
- HLS segment format is **negotiated** via `hls_segment_formats` in the
  capability manifest: TS is the floor (some Android-TV cast receivers
  reject fMP4 outright), fMP4 is used when the stream carries HEVC (which
  cannot ride in TS); a TS-only client asking for HEVC gets an h264
  re-encode, with the reason in the trace.
- The server advertises a LAN-reachable `base_url` (`GET /api/system`;
  override with `ADVERTISE_URL`) since the device can't resolve localhost.
- Permissive CORS on stream routes (the receiver fetches cross-origin).

Caveat: the Cast sender SDK needs Chrome and a secure context — open the UI
at `http://localhost:8099` (localhost counts as secure) or put it behind
HTTPS; a plain `http://<lan-ip>` page won't show the cast button.

`scripts/cast_harness.py` (needs `.venv` with pychromecast) drives a real
cast device end-to-end without a browser: discovers via mDNS, gets a play
decision, loads the URL, and asserts on the receiver's own status channel.
`--url` casts an arbitrary URL as a control test. Exits 0/1 for CI use.
The server must be reachable from the LAN — check the host firewall
(this was once a silent `ufw` DROP; stream fetches are logged for exactly
this kind of diagnosis).

## Tests

```sh
go test ./...
```

Decision-engine tests cover the canonical matrix cases (e.g. 4K HEVC HDR10 +
h264-only client → tone-mapped, downscaled h264 transcode); pipeline tests
pin the FFmpeg argv contract (filter order, seek-before-input, copy paths).
The works tests table the fraction law for every shape, books included
(`ch. 7 · 34%`, `p. 213 / 400`); the scanner tests build EPUBs and PDFs in
memory (classic xref tables, xref and object streams, lying offsets, no
trailer at all); the store tests migrate a pre-locator `state.db`;
`node --test web/passage_test.mjs` covers the passage and locator grammar;
and `node --test web/cast_test.mjs` covers the cast bridge's reading of the
session document.

**The two sides share one truth.** `cmd/server/goldens_shared_test.go` copies
`cmd/server/testdata/hyper/*.json` to `web/testdata/hyper/` and FAILS when the
two differ (`go test ./cmd/server -update` refreshes both halves in one run),
and the client's suites render those very documents:

```sh
node --test web/*_test.mjs
```

- `web/render_test.mjs` runs every renderer over every golden and asserts the
  contract: each action becomes a control carrying the document's own label
  and href, each address in the output came out of the document, no renderer
  emits a media element, and an unknown field changes nothing.
- `web/kernel_test.mjs` asserts what the client is allowed to know — one
  `/api/` literal in the kernel, none in the device or the renderers, one line
  of logic in `index.html` — and that every file the page loads is in `sw.js`'s
  SHELL list.

CI runs both suites, parses every `web/*.js`, and on a pull request fails when
a SHELL file changed without a `CACHE` bump in `web/sw.js`
(`scripts/shell-bump-check.sh`): the shell is served cache-first and `sw.js` is
the only file a browser re-checks, so an unbumped change never reaches a
browser that has the old shell.
