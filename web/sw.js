// flickr PWA service worker — app-shell cache only.
// HARD RULE: /api and /streams are NEVER cached (staleness would be poison);
// cross-origin (CDN hls.js / cast_sender) is never touched either.
// Bump on EVERY index.html change: the shell is served cache-first, so an
// unbumped cache keeps running old JS against new API data — which is not a
// cosmetic staleness. v5's grouping reads identity kind "extra"; v4's code
// treats those files as untitled movies and puts one tile on the grid per
// featurette. v7 adds passage.js to the shell: index.html calls its
// globals at parse time, so a v6 shell serving the new index without it
// would throw before the router runs. v8 adds audio.js and audio.css the
// same way: index.html's grid builder calls isAudioItem at render time.
const CACHE = 'flickr-shell-v8';
const SHELL = ['/', '/index.html', '/passage.js', '/audio.js', '/audio.css', '/manifest.json', '/icon-192.png', '/icon-512.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting())
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
      (hit) => hit || fetch(req).then((resp) => {
        if (resp.ok) {
          const copy = resp.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return resp;
      })
    )
  );
});
