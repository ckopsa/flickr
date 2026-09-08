// flickr PWA service worker — app-shell cache only.
// HARD RULE: /api and /streams are NEVER cached (staleness would be poison);
// cross-origin (CDN hls.js / cast_sender) is never touched either; the
// reader's libraries are vendored under /vendor and precached with the shell.
// Bump on EVERY index.html change: the shell is served cache-first, so an
// unbumped cache keeps running old JS against new API data — which is not a
// cosmetic staleness. v5's grouping reads identity kind "extra"; v4's code
// treats those files as untitled movies and puts one tile on the grid per
// featurette. v7 adds passage.js to the shell: index.html calls its
// globals at parse time, so a v6 shell serving the new index without it
// would throw before the router runs. v8 added audio.js and audio.css the
// same way (index.html's grid builder calls isAudioItem at render time);
// v9 adds the reader (reader.js and the vendored epub.js + JSZip): same-
// origin now, so the shell carries them and a book opens with nothing
// fetched from the internet. v10 adds the PDF pane (pdfreader.js, and the
// vendored pdf.js module + its worker, which pdf.js loads by URL at open
// time) the same way, and reader.js became the facade the new index.html
// calls. v11 carries two index.html-only changes (reload when a new worker
// takes control; tap-to-play when autoplay is refused) — and the RULE that
// forgetting bit: the shell is cache-first and this file is the only thing
// the browser re-checks, so EVERY change to a SHELL file needs a bump here
// or no browser ever sees it. A CI check for that is worth a bead.
// v12: the precache and every refill are fetched with cache: 'reload',
// bypassing the browser's HTTP cache — which, with no Cache-Control
// from the server, had kept a weeks-old index.html "fresh" and fed it
// straight into v7 through v11 (2026-09-07). The server now says
// no-cache too; this is the belt to that suspenders.
// v13: the router asks the server what a hash means (GET /api/-/route) and
// passage.js lost the show-form resolution to internal/passage — a v12 shell
// serving the new index.html would call functions that are no longer there.
// v14: the grid stops regrouping too. index.html draws GET /api/library's
// tiles and audio.js lost audioWorks()/artists(), so a v13 shell has neither
// the grouping code nor the document that replaced it. Both halves of the
// old client are gone now; this bump is what makes the new pair meet.
// v15: the player reads the SESSION DOCUMENT (hyper 4) — play answers it,
// progress goes to its action and a 409 means a passage is on, and the marks
// are the server's; passage.js lost its marking half too, so a v14 shell
// serving the new index.html would call functions that are no longer there
// either.
// v16: the cast bridge is a file of its own (cast.js) and index.html calls
// castMediaSpec at load time, so a v15 shell serving the new index.html would
// throw the moment anyone cast something. web/receiver.html loads the same
// file, but from the network on the cast device — it is not shell.
// v17: the READER reads the session document too (hyper 5) — a book is
// opened by POST /api/items/{id}/read, reader.js and pdfreader.js take that
// document instead of an item and the URLs they used to guess, and
// passage.js lost locatorSection to the server. A v16 shell serving the new
// index.html would hand the panes an item and call a function that is gone.
// v18: index.html is markup and script tags now — the logic is renderers.js
// (one pure function per document kind), player.js (the device) and kernel.js
// (the router and the mounting), and audio.js is DELETED. A v17 shell has
// none of those three files and would serve an index.html whose one inline
// line, Kernel.boot(), names something that does not exist. This is the bump
// the CI check in .github/workflows/tests.yml now insists on.
// v19: the readers' dark page; v20: the book's own choice about its
// pictures (session.display), the 🖼 button in both panes.
const CACHE = 'flickr-shell-v32';
const SHELL = ['/', '/index.html', '/passage.js', '/focus.js', '/cast.js', '/audio.css',
               '/renderers.js', '/player.js', '/kernel.js',
               '/reader.js', '/pdfreader.js',
               '/vendor/jszip.min.js', '/vendor/epub.min.js',
               '/vendor/pdf.min.mjs', '/vendor/pdf.worker.min.mjs',
               '/manifest.json', '/icon-192.png', '/icon-512.png'];

// The one answer worth keeping: a 200 that IS the file asked for. A redirect
// followed to a 200 is a fine response and a poisonous cache entry — behind a
// sign-in service every shell request answers 302 to the login page, and
// stored under /index.html that page is the app for as long as the cache
// lives. addAll does not make this check (it only refuses a non-ok), so the
// shell is fetched a request at a time below and this is the gate each answer
// passes.
function good(resp) {
  return resp.ok && !resp.redirected && resp.type !== 'opaqueredirect';
}

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE)
      // One bad answer fails the whole install, which is what addAll did: half
      // a shell in the cache is worse than none.
      .then((c) => Promise.all(SHELL.map((u) => {
        const req = new Request(u, { cache: 'reload' });
        return fetch(req).then((resp) => {
          if (!good(resp)) throw new TypeError('shell: ' + u + ' answered ' + resp.status);
          return c.put(req, resp);
        });
      })))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  // network-only: anything not same-origin (CDNs), all /api and /streams
  if (url.origin !== self.location.origin) return;
  if (url.pathname.startsWith('/api') || url.pathname.startsWith('/streams')) return;
  // cache-first for the app shell only; everything else goes to the network
  const path = url.pathname === '/' ? '/' : url.pathname;
  if (!SHELL.includes(path)) return;
  e.respondWith(
    caches.match(req).then(
      (hit) => hit || fetch(new Request(req, { cache: 'reload' })).then((resp) => {
        if (good(resp)) {
          const copy = resp.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return resp;
      })
    )
  );
});
