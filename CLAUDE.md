# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## Build & Test

Plain Go and a bare `node`, no external services: the tests never shell
out (`os/exec` only lives in runtime paths — pipeline sessions,
hardware-accel detection, the scanner's ffprobe call), so no ffmpeg, no
MinIO and no `.env` are needed to run them. The client's suites need no
npm and no browser either: the renderers are pure functions over the Go
goldens.

```bash
gofmt -l .             # must print nothing
go vet ./...
go build ./...
go test -count=1 ./...
node --test web/*_test.mjs   # the client's suites: no npm, no browser
node scripts/browser-smoke.mjs   # the page in a real Chromium: needs Playwright
SMOKE_OIDC=1 node scripts/browser-smoke.mjs   # ...the same walk, behind a sign-in
```

The browser smoke is the one check that opens the page: it builds
`cmd/server/smoke_test.go` (`-tags smoke`, skipped otherwise) to serve the
fixture library on a port with generated media, an EPUB and a PDF, then
walks the gate, the worker installing its shell, home, settings, a show, a film, a record under the
mini-player, both readers, search, a phone viewport, a tablet held both
ways, a landscape phone's pocket theatre, a thumb (the double-tap skip
and a reader's tap zone), the install row with nothing to offer, and a
kids profile, failing on any JavaScript error. The tablet, the pocket,
the thumb and the install row run in a second browser context with
`hasTouch`, because `(pointer: coarse)` cannot be turned on for a context
that already exists. Run it before a PR that touches `web/`.
`SMOKE_OIDC=1` stands the fake Keycloak of `cmd/server/fakeissuer_test.go`
in front of the same library and adds one step first: anonymous is
refused in words, the door leads to the provider and back to the hash it
left, the settings panel says who is signed in, and Sign out lands at the
door again. Run it both ways after touching the gate (README "Signing in").

CI (`.github/workflows/tests.yml`) runs those four steps on every push
to `master`, every pull request and on demand, then the client's own:
`node --check` over every `web/*.js`, `node --test web/*_test.mjs`, and
on a pull request the shell/cache check
(`scripts/shell-bump-check.sh`). Its `test` check is the merge gate: a
red `test` blocks the merge; fix and push rather than waiting. Running
the commands above locally before pushing is cheap (seconds) and
expected. `.github/workflows/
beads-sync.yml` carries a committed `.beads/issues.jsonl` into the Dolt
remote on push to `master` — see the header of that file and the bd
section above before relying on it.

Deploys: a merge to `master` touching the server or web UI runs
`.github/workflows/image.yml` (build on the `flickr` runners, push to
the cluster registry, write the Nomad deploy variable). See README
"Deploy".

## Architecture Overview

- `cmd/server/` — the HTTP server: routes under `/api/...`, serves
  `web/`, owns the scan schedule, the session reaper and the post-scan
  enrichment and trickplay stages. Its reads answer **documents** — the
  hypermedia envelope of `internal/hyper` — and `hyper.go`, `library.go`,
  `session.go`, `read.go`, `route.go`, `passage.go` and `continue.go` are
  one document kind each.
- `internal/decision/` — `Decide(media, caps, policy)`: the pure,
  side-effect-free playback decision (direct play / remux / transcode)
  with a trace answering "why did this transcode?" for every verdict.
- `internal/pipeline/` — the only package that knows FFmpeg exists:
  `BuildArgs` (job → argv) is pure; sessions own the subprocess and HLS
  output; hardware-accel detection, subtitle and trickplay extraction
  live here too.
- `internal/scanner/` — incremental, job-based library scanning (list →
  diff etags → probe changed objects → batch-write → reconcile) plus
  `Identify`, the deterministic, versioned object-key → identity rule.
- `internal/store/`, `internal/tmdb/`, `internal/works/`,
  `internal/model/` — split SQLite storage (`library.db` metadata,
  `state.db` playback positions), TMDB enrichment as a separate
  post-scan stage, the pure works/feed derivation, and the shared types.
- `internal/hyper/` — the envelope every handler answers in: `Envelope`,
  `Link`, `Action`, `Problem` and the `Doc` builder. `internal/passage/`
  is the passage grammar, the one implementation of it.
- `web/` — the vanilla-JS UI, with no build step, in three parts: the
  **kernel** (`kernel.js` — boot, the hash handed to `GET /api/-/route`,
  the document cache, the fetch that reads a problem, one delegated click
  listener), the **renderers** (`renderers.js` — one pure function per
  document kind, each returning an HTML string) and the **device**
  (`player.js`, `reader.js`, `pdfreader.js`, `cast.js` — the media
  element, hls.js, the cast session, the reader's pages, the clock).
  `index.html` is markup, script tags and one line of logic. Beside them
  the Cast receiver page and the PWA manifest and service worker;
  capability presets show the same file direct-playing or transcoding per
  client.

Start with `README.md`: its numbered list is the design rationale, item
by item.

## Conventions & Patterns

- The interesting logic is pure functions, table-tested:
  `decision.Decide`, `pipeline.BuildArgs`, `scanner.Identify` (and
  `works` derivation). Add a case to the table before touching the
  function; the tests pin contracts (filter order, seek-before-input,
  copy paths), not implementation.
- Identification never does I/O. `scanner.Identify` maps an object key
  to an identity from the key alone; bumping `IdentityVersion` makes the
  next scan recompute identities without a re-probe. Enrichment is a
  separate stage keyed off identity, never folded into it.
- FFmpeg stays behind `internal/pipeline`; the decision engine is
  hardware-agnostic and encoder choice happens at that boundary.
- **The server says what exists and what may be done; the browser renders
  what it is told and owns the device.** A read answers a document —
  fields, `links`, `actions` (each with `method`, `href`, an `input`
  sketch and a `label`) and `unavailable` with a reason in words; a
  refusal is `application/problem+json` with a `remedy`. Grow the
  document rather than teaching the client a rule.
- **A renderer never composes a URL.** Every address it emits is
  `links[rel].href` or `actions[name].href`, copied out of the document;
  the client knows `/api/` by heart and nothing else. `web/kernel_test.mjs`
  and `web/render_test.mjs` grep the source and fail on a second literal,
  so a new address in `renderers.js` or `player.js` is a test failure, not
  a review comment. What a renderer may compose is a hash: the hash
  grammar is the client's, and the server resolves it.
- `location.hash` is the single source of truth for navigation — change
  the hash, never the view directly — and the hash is handed to
  `GET /api/-/route` rather than parsed.
- The media element is made ONCE and moved between renders; no renderer
  emits a `<video>`. A document swap must never drop the buffer.
- `web/sw.js` serves the shell cache-first, so a change to any file in its
  SHELL list needs a `CACHE` bump in the same commit
  (`scripts/shell-bump-check.sh` fails the gate otherwise).
- The client's suites run over the SERVER's goldens
  (`cmd/server/testdata/hyper/*.json`, copied to `web/testdata/hyper/` and
  held in step by `cmd/server/goldens_shared_test.go`). Refresh both halves
  with `go test ./cmd/server -update`.
