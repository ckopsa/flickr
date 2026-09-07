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
   and the `audiobook`/`album`/`book` kinds. Text does not play: the play
   endpoints answer 415 for it and the grid hides books until the reader
   lands; trickplay skips audio and text alike (no frames to draw).
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

## Library layout

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
| `Books/` (`Ebooks/`) | `Books/<Author>/<Title (Year)>.epub`, `Books/<Author>/<Title>/<file>.epub` | kind `book`, author, title (from the file or the directory), year |

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
`.epub` (`.pdf` is a later bead). Subtitle sidecars (`.srt .ass .ssa .vtt`)
are matched to the file they sit beside, not admitted on their own.

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
`ch. 7 · 1:19:22 / 11:30:00`; a book reports a position but no fraction or
text until the reader bead adds a locator. Audio plays (design note 14);
text does not yet — the EPUB reader is bead flickr-9au.

## Run

```sh
cp .env.example .env   # fill in MinIO credentials
go run ./cmd/server
# open http://localhost:8080, hit "Scan library"
```

Requires `ffmpeg`/`ffprobe` on PATH. Media files are read straight from MinIO
via presigned URLs — both for probing and as FFmpeg input; nothing is copied
locally except HLS segments in `data/streams/`.

## API

| Endpoint | Purpose |
|---|---|
| `POST /api/scan`, `GET /api/scan` | trigger / observe incremental scan (enrichment runs after) |
| `GET /api/items` | library listing (incl. `media_info.medium`, `media_info.subtitles` and `enrichment`) |
| `GET /api/works` | library as works: movies, whole shows, audiobooks, albums, books (+ stray files), title-sorted, each with `work_key`, `kind`, `medium`, `author`, counts, and a `representative_item_id` for artwork |
| `GET /api/works/{key}/items` | one work's member items (episodes in season/episode order, parts and tracks in part order, each track with its `identity.track_title`); 404 for unknown keys |
| `GET /api/artists` | the album works shelved by artist: `name`, `album_count`, `albums` in year order (`work_key`, `title`, `year`, `track_count`, `representative_item_id`), and the artist's own `representative_item_id` — the item whose `/cover` is the picture; name-sorted, `[]` without music |
| `GET /api/continue?client_id=NAME` | a profile's resume list: most-recent first, max 20, finished (≥90%) and <5 s positions excluded, one entry per work |
| `GET /api/feed/media?since=CURSOR` | change feed: works changed since the cursor with per-audience progress; response carries the next cursor (`l<n>.s<n>`); no `since` = everything |
| `POST /api/items/{id}/decision` | dry-run: decision + trace, no side effects; audio items take the audio branch (415 for text — books are read, not streamed) |
| `POST /api/items/{id}/play` | decide and act: presigned URL (direct) or HLS session (transcode, audio-only for audio items); 415 for text |
| `POST /api/items/{id}/identity` | user identity override (persists across scans) |
| `POST /api/items/{id}/reprobe` | re-probe one item on demand (also re-discovers subtitle sidecars in its directory) |
| `POST /api/items/{id}/enrich` | TMDB-enrich one item on demand (503 if no API key) |
| `GET /api/items/{id}/subtitles/{ordinal}.vtt` | subtitle track as WebVTT — embedded or external sidecar (415 for bitmap tracks, 404 for bad ordinal) |
| `GET /api/items/{id}/poster` | cached TMDB poster (image/jpeg, 404 if absent) |
| `GET /api/items/{id}/cover` | an audio item's embedded cover art, extracted with ffmpeg on first request and cached in `data/covers/`; falls back to the poster route, 404 when neither exists |
| `GET /api/items/{id}/still` | cached TMDB episode still (image/jpeg, 404 if absent) |
| `GET /api/items/{id}/trickplay.json` | scrub-preview sprite index (404 if not generated) |
| `GET /api/items/{id}/trickplay/{n}.jpg` | sprite sheet n |
| `POST /api/items/{id}/trickplay` | force-generate sprites for one item (synchronous) |
| `DELETE /api/sessions/{id}` | stop a transcode session (idle sessions are auto-reaped) |
| `POST/GET /api/progress` | playback position per (item, client) |
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
           ?from=<locator>&to=<locator>  text passages (future epub reader)
#/show/<title>?ep=S02E05&t=…&end=…[&until=S02E07]
                                      the same, addressed by episode code
```

- `t` / `end` are seconds into the item, integers or decimals (`4740`,
  `79.5`). An `end` at or before `t` is ignored.
- `until=<item_id>` turns the link into an episode run: playback auto-advances
  through the episodes and stops after the item with that id. `t` applies to
  the first item only; `end`, if given, applies within the last item —
  omit it to let that episode play out.
- `from` / `to` are locators for text passages. They are parsed and carried
  through the route today so the grammar is one thing; the player gives them
  no behaviour until the reader lands.
- The show form is for a minter that knows a show's link and a person's
  spelling of an episode but no item ids: `ep=S02E05` (any case) names the
  episode, `until=S02E07` names the last episode of a run. It is resolved
  against the show's season/episode numbers when opened and the route is
  replaced by the item form, so everything below applies unchanged. An `ep`
  (or `until`) that names no episode shows one sentence on the show page
  and plays nothing.

Examples:

```
#/item/51?t=4740&end=5070              the scene from 1:19:00 to 1:24:30 of item 51
#/item/58?t=120&until=59               episodes 58 and 59, skipping 58's first two minutes
#/show/Ninjago?ep=S02E05&until=S02E06  the same run, spelled by show and episode
```

Three rules hold for every passage session:

1. **Start there.** `t` wins over saved progress; a passage never resumes
   from `/api/progress`.
2. **Stop there.** At `end` (or once the `until` episode has finished) the
   player pauses and shows *End of the passage*. It is not a natural end: no
   up-next countdown, no "finished" mark. Both bounds are flagged on the
   scrub bar and the time readout counts within the passage
   (`2:14 / 5:30 in passage`).
3. **No progress is written.** A scene replayed for a talk is not where the
   person is in the film; the saved place belongs to normal viewing. Nothing
   in a passage session — heartbeat, end, run — touches `/api/progress` or
   the continue-watching row.

The escape hatch is **Keep watching**: it leaves passage mode from the
current position, drops the passage from the route, and normal behaviour
resumes — progress writes included. Pressing play while paused at the end
is the same choice. **Back** closes the player and leaves the bare item
route behind. Casting honours all of this from the sender side: the page
watches the receiver's time and pauses it at `end`.

### Making a passage

The player mints these links too. While watching, **Mark in** and **Mark
out** in the control row set the bounds at the current position (the cast
receiver's position while casting); a mark within two seconds of a chapter
start snaps to it, since the scrubber's chapter ticks are the natural cut
points. Marking out before the in point swaps the two. A set chip shows its
time (`In 1:19:00`), tapping it again re-marks at the current position, and
its small × clears it; the amber flags appear on the scrub bar exactly as a
played passage's do. Once an in point exists, **Copy passage link** writes
the absolute `#/item/<id>?t=…&end=…` URL to the clipboard and always shows
it as selectable text under the controls — a phone on plain `http://` has no
clipboard API but can still long-press and copy. Marks survive an episode
advance within the same show, so marking in on one episode and out after
up-next has moved to a later one makes a run: the link carries
`&until=<that episode's id>` with `end` inside it. Marks are dropped when the
player closes or another work starts. Nothing is stored: a passage is its
URL, and the household's day planner is where it is kept.

`web/passage.js` holds the grammar (`parsePassage`, `passageQuery`), the end
predicate and the marking logic (`snapToChapter`, `markBounds`,
`passageLink`) as pure functions; `node --test web/passage_test.mjs` runs
their tests without a build step.

## Casting

The web UI is a Google Cast sender: the cast icon in the control row streams
to a Chromecast running the default media receiver, and the page becomes the
remote (play/pause, ±seek, volume, stop, scrub bar, chapter jumps). While a
cast session is active the client sends the *Chromecast Ultra's* capability
manifest — the decision engine negotiates for the device that actually plays,
not the browser.

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
