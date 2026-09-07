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
const CACHE = 'flickr-shell-v13';
const SHELL = ['/', '/index.html', '/passage.js', '/audio.js', '/audio.css', '/reader.js', '/pdfreader.js',
               '/vendor/jszip.min.js', '/vendor/epub.min.js',
               '/vendor/pdf.min.mjs', '/vendor/pdf.worker.min.mjs',
               '/manifest.json', '/icon-192.png', '/icon-512.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE)
      .then((c) => c.addAll(SHELL.map((u) => new Request(u, { cache: 'reload' }))))
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
        if (resp.ok) {
          const copy = resp.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return resp;
      })
    )
  );
});
