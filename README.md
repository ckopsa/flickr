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
| `GET /api/items` | library listing (incl. `media_info.subtitles` and `enrichment`) |
| `GET /api/works` | library as works: movies + whole shows (+ stray files), title-sorted, with `work_key`, counts, and a `representative_item_id` for artwork |
| `GET /api/works/{key}/items` | one work's member items (episodes in season/episode order); 404 for unknown keys |
| `GET /api/continue?client_id=NAME` | a profile's resume list: most-recent first, max 20, finished (≥90%) and <5 s positions excluded, one entry per work |
| `GET /api/feed/media?since=CURSOR` | change feed: works changed since the cursor with per-audience progress; response carries the next cursor (`l<n>.s<n>`); no `since` = everything |
| `POST /api/items/{id}/decision` | dry-run: decision + trace, no side effects |
| `POST /api/items/{id}/play` | decide and act: presigned URL (direct) or HLS session (transcode) |
| `POST /api/items/{id}/identity` | user identity override (persists across scans) |
| `POST /api/items/{id}/reprobe` | re-probe one item on demand (also re-discovers subtitle sidecars in its directory) |
| `POST /api/items/{id}/enrich` | TMDB-enrich one item on demand (503 if no API key) |
| `GET /api/items/{id}/subtitles/{ordinal}.vtt` | subtitle track as WebVTT — embedded or external sidecar (415 for bitmap tracks, 404 for bad ordinal) |
| `GET /api/items/{id}/poster` | cached TMDB poster (image/jpeg, 404 if absent) |
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
| `#/item/<id>` | one item's detail pane (its Play button resumes from the saved place) |

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

`web/passage.js` holds the grammar (`parsePassage`, `passageQuery`) and the
end predicate as pure functions; `node --test web/passage_test.mjs` runs
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
