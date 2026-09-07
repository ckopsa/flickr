// flickr — the kernel (docs/hypermedia.md §The client).
//
// The whole of the client's own knowledge, and it is small:
//
//   * ONE address by heart, `/api/`. Everything else is a relation followed
//     out of a document — including the route resolver itself, which the root
//     names as `actions.route`.
//   * the hash is split off the URL and handed to the server: GET
//     /api/-/route?hash=… answers which view to draw, the document to draw it
//     from, the passage resolved, and whether arriving starts it. No grammar
//     is parsed here (internal/passage holds it) beyond splitting at the '?'.
//   * a document CACHE keyed by `self`, so a work followed twice costs one
//     fetch.
//   * `api(href, opts)`: application/problem+json comes back as a Problem
//     with its `detail` and its `remedy`, because a refusal is an answer.
//   * the profile, which is the client_id every document is read as. It rides
//     as a COOKIE of that name, so no href this file follows is ever touched.
//
// The <video> element and the reader container are created ONCE and live for
// the life of the page (see Player.element and #reader-pane below): the
// session chrome is re-rendered whenever the session document changes, and
// the device element is moved into the fresh chrome synchronously, which by
// the HTML spec's media element insertion steps neither reloads nor pauses
// it. A document swap must never drop the buffer, and this is how.
(function (root) {
  'use strict';

  const R = root.Render;
  const esc = R.esc;
  const $ = id => document.getElementById(id);

  // THE one address a client may hold. Every other address in this file — and
  // there is none — would be a rule the browser had learned by heart.
  const ROOT = '/api/';

  // --- refusals ----------------------------------------------------------------

  // A Problem is what a refusal reads as: the sentence, and what would make
  // the action available.
  class Problem extends Error {
    constructor(status, body, fallback) {
      const b = body || {};
      super(b.detail || b.title || b.error || fallback || 'That could not be done.');
      this.status = status;
      this.problem = b;
      this.detail = b.detail || b.title || b.error || this.message;
      this.remedy = b.remedy || null;
    }
    // The remedy in one sentence, for a screen that has room for it.
    get remedyText() {
      const r = this.remedy;
      if (!r) return '';
      return typeof r === 'string' ? r : (r.action || r.detail || '');
    }
  }

  // api follows one href. It never builds one.
  async function api(href, opts) {
    const r = await fetch(href, opts);
    if (r.status === 204) return null;
    const body = await r.json().catch(() => null);
    if (!r.ok) throw new Problem(r.status, body, r.statusText);
    return body;
  }

  // --- the document cache ------------------------------------------------------
  // Keyed by the document's own address, which is the only key there is.
  const docs = new Map();
  function remember(doc) {
    if (doc && doc.self) docs.set(doc.self, doc);
    return doc;
  }
  function forget() { docs.clear(); }
  async function doc(href) {
    if (!href) return null;
    if (docs.has(href)) return docs.get(href);
    return remember(await api(href));
  }

  // --- identity ----------------------------------------------------------------
  // clientId IS the profile name: the playback API is keyed by it. It is sent
  // as a cookie so that following a link needs no query string bolted on —
  // the server reads client_id from either (cmd/server/hyper.go, profileOf).
  let profile = localStorage.profileName || null;
  function setProfileCookie(name) {
    document.cookie = 'client_id=' + encodeURIComponent(name || '') +
      '; path=/; max-age=31536000; samesite=lax';
  }
  function who() { return profile || 'anonymous'; }

  // --- state -------------------------------------------------------------------
  let rootDoc = null;            // GET /api/ — the one address known by heart
  let system = null;             // GET (root.links.system) — hardware, base_url
  let libState = { q: '', genre: null, note: '' };
  let libraryDoc = null, continueDoc = null;
  let currentRoute = null;       // the route document the current hash resolved to
  let currentWork = null;        // the work document the current view belongs to
  let currentTrickplay = null;   // its sprite sheets, where the item has them
  let currentItem = null;        // the item document on screen
  let pendingAutoplay = null;    // item id to start once its route renders
  let pendingPlay = null;        // { id, mode: 'nav' | 'zero' } from a prev/next
  let collapsed = false;         // an audio sitting playing on in the mini bar
  let pendingExpand = false;     // the bar was tapped: the full chrome is wanted
  let routeSeq = 0;
  let swReloadPending = false;   // a new worker took control mid-stream

  // --- views -------------------------------------------------------------------

  // A route change swaps the whole of the view, and a cut between two screens
  // reads as a flicker. document.startViewTransition cross-fades the swap
  // where the browser has one; where it does not, or where the person asked
  // for less motion, the swap is the plain swap it always was.
  //
  // The tapped poster is the exception to the cross-fade: it wears a
  // view-transition-name for the length of the swap and the detail poster it
  // becomes takes the SAME name, which is what tells the browser to morph one
  // into the other. The name is derived from the tile's own hash, which names
  // the same thing its `self` does, and only one element wears it at a time.
  let morphing = null;   // the poster tapped, named and waiting to be morphed
  let morphName = '';

  function animates() {
    return typeof document.startViewTransition === 'function' &&
      !matchMedia('(prefers-reduced-motion: reduce)').matches;
  }

  // swap answers a promise that settles once the DOM has changed: the
  // browser runs the mutation a frame later, after it has captured the old
  // view, so anything that paints INTO the new markup — the continue shelf,
  // the hero — has to wait for it rather than land in the view on its way out.
  function swap(mutate) {
    if (!animates()) { mutate(); return Promise.resolve(); }
    const t = document.startViewTransition(mutate);
    // A swap that overtakes another skips it, and a skip rejects `ready`:
    // that is the expected way of two navigations in a row, not an error.
    if (t && t.ready) t.ready.catch(() => {});
    return t && t.updateCallbackDone ? t.updateCallbackDone.catch(() => {}) : Promise.resolve();
  }

  function morph(el) {
    if (morphing) morphing.style.viewTransitionName = ''; // one wearer at a time
    morphing = null;
    morphName = '';
    if (!animates() || !el) return;
    const poster = el.querySelector('.poster-wrap');
    const hash = el.dataset.nav || '';
    if (!poster || !hash) return;
    morphName = 'poster' + hash.replace(/[^A-Za-z0-9]+/g, '-');
    morphing = poster;
    poster.style.viewTransitionName = morphName;
  }

  // A picture is there or it is on its way; .loaded says which, and the frame
  // shimmers until it does (index.html carries the placeholder). The frame is
  // the tile's wrapper where the picture has one and the PICTURE ITSELF where
  // it has none — an episode still, the Next up art, the detail poster — so
  // every image on the page settles by the same mark.
  function markLoaded(img) {
    (img.closest('.poster-wrap') || img).classList.add('loaded');
  }

  function mount(html) {
    return swap(() => {
      $('view').innerHTML = html;
      // The tapped tile went with the markup above; its name moves to the
      // poster it became, and a picture already in the cache is already there.
      const poster = $('detail-poster');
      if (poster && morphName) poster.style.viewTransitionName = morphName;
      morphing = null;
      morphName = '';
      for (const img of $('view').querySelectorAll('img')) {
        if (img.complete && img.naturalWidth) markLoaded(img);
      }
    // A television has no pointer to leave anywhere: the ring has to land on
    // the fresh view itself, or the first press of an arrow is spent finding
    // out where it was. It lands once the swap has settled, which is what
    // this promise says.
    }).then(() => focusFirst());
  }

  function showStage(on) {
    return swap(() => {
      $('stage').hidden = !on;
      $('view').hidden = !!on;
    });
  }

  // The session chrome, re-rendered from the session document every time that
  // document changes — and the ONE device element moved into it. Nothing in
  // the chrome is a <video>; the buffer belongs to the device, not the render.
  function renderSession(session) {
    // A COLLAPSED sitting is chrome too, of the other shape: the next track in
    // the run redraws the bar rather than throwing the player over whatever
    // the person is reading.
    if (collapsed) { paintMini(session); return; }
    const chrome = $('player-chrome');
    const html = R.session(session, { work: currentWork });
    chrome.innerHTML = html;
    chrome.hidden = !html;
    if (!html) return; // a reading session has no player chrome; the pane is its own
    root.Player.attach(chrome); // the device comes back out of the bar, if it was in it
    hideMini();
    showStage(true);
  }

  // --- the mini player ---------------------------------------------------------
  //
  // Music does not stop because you went looking for the next record. Leaving
  // an audio item COLLAPSES its sitting into the bottom bar instead of ending
  // it: the bar is drawn from the same session document and the one device
  // element moves into the hidden slot inside it, so the buffer — and the
  // sound — carry on across the document swap. Video keeps the old rule: a
  // picture nobody is looking at is not worth a stream.
  //
  // Three things end a collapsed sitting: the bar's own tap (which asks for
  // the full chrome back), starting any other item, and Stop.
  function playsOn() {
    return root.Player.medium() === 'audio' && !!root.Player.session();
  }
  function collapse() {
    const s = root.Player.session();
    if (!s) { closeStage(); return; }
    collapsed = true;
    paintMini(s);
    showStage(false);
  }
  function paintMini(session) {
    const bar = $('mini-player');
    const html = R.miniPlayer(session, { work: currentWork });
    if (!bar || !html) return;
    bar.innerHTML = html;
    bar.hidden = false;
    root.Player.attach(bar); // synchronous, so the element never leaves the document
  }
  // The full chrome again, which is one re-render of the session document.
  function expand() {
    const s = root.Player.session();
    collapsed = false;
    if (!s) { hideMini(); return; }
    renderSession(s);
  }
  function hideMini() {
    collapsed = false;
    const bar = $('mini-player');
    if (bar) bar.hidden = true;
  }
  // Starting a play is the end of a collapsed sitting: the bar belongs to what
  // was playing, and this is another one.
  async function startPlay(itemDoc, opts) {
    collapsed = false;
    await root.Player.play(itemDoc, opts);
  }

  // One sentence, said out loud: a refusal's detail and the remedy that would
  // make the action available. It is the only place this client tells a person
  // something went wrong, and it says what the SERVER said.
  let noteTimer = null;
  function note(msg) {
    if (!msg) return;
    const el = $('toast');
    if (!el) return;
    el.textContent = msg;
    el.hidden = false;
    if (noteTimer) clearTimeout(noteTimer);
    noteTimer = setTimeout(() => { el.hidden = true; }, 8000);
  }

  // --- the library view --------------------------------------------------------

  async function paintLibrary() {
    await mount(R.library(libraryDoc, libState, continueDoc));
    libState.note = '';
    paintContinue();
  }

  // The search box is the header's and outlives every render, so typing in it
  // repaints the banded sections in place rather than the whole view.
  function filterLibrary() {
    // The filter is kept in step with the box even off the library, so coming
    // back to the grid draws what the box says rather than what it said last.
    libState.q = $('search').value.trim().toLowerCase();
    const grid = $('grid');
    if (!grid) return; // not on the library: there is nothing to repaint
    grid.innerHTML = R.libraryGrid(libraryDoc, libState).html;
  }

  // --- search ------------------------------------------------------------------
  //
  // Typing filters the tiles that are ON SCREEN as it goes, which is instant
  // and finds a title. It cannot find an episode by its name or a track by
  // its own: neither is on a tile. So once the typing settles — or on Enter,
  // which means now — the whole library is searched, and that is a ROUTE
  // (#/search/<q>), resolved by the server like every other: a page of
  // results is somewhere a person can be sent.
  const searchSettleMs = 300;
  const searchMinChars = 2;
  let searchTimer = null;

  function onSearchInput() {
    filterLibrary();
    if (searchTimer) clearTimeout(searchTimer);
    searchTimer = setTimeout(runSearch, searchSettleMs);
  }

  // Emptying the box by pressing the chip the shelf wears, which is the same
  // as emptying it by hand: the box IS the filter, so nothing else is told.
  function clearSearch() {
    if (searchTimer) clearTimeout(searchTimer);
    searchTimer = null;
    $('search').value = '';
    filterLibrary();
  }

  function onSearchKey(e) {
    if (e.key !== 'Enter') return;
    if (searchTimer) clearTimeout(searchTimer);
    runSearch();
  }

  // One letter is not a search worth leaving the library for, and refining
  // one is not a second page: the first search is navigated to and every
  // keystroke after it REPLACES that entry, so Back goes where the person
  // came from rather than back through every prefix they typed. Emptying the
  // box while the results are up is leaving them.
  function runSearch() {
    searchTimer = null;
    const q = $('search').value.trim();
    const onResults = (location.hash || '').startsWith('#/search/');
    if (q.length < searchMinChars) {
      if (onResults) replaceHash('#/');
      return;
    }
    const hash = R.searchHash(q);
    if (onResults) replaceHash(hash);
    else navigate(hash);
  }

  function paintContinue() {
    const box = $('cw');
    if (!box) return;
    const html = continueDoc ? R.continueShelf(continueDoc) : '';
    box.innerHTML = html;
    box.hidden = !html;
    // The hero leads with whatever is in progress, so it is drawn again when
    // the shelf lands on its own errand — or empties.
    const slot = $('hero-slot');
    if (slot) slot.innerHTML = R.hero(libraryDoc, continueDoc);
    focusFirst(true); // the hero the ring was on went with that repaint
  }

  // The resume shelf is its own document, followed from the root (or the
  // library, which names it too). It is re-read whenever playback ends, which
  // is when a resume point moves.
  async function loadContinue() {
    const href = R.linkHref(rootDoc, 'continue') || R.linkHref(libraryDoc, 'continue');
    if (!href || !profile) { continueDoc = null; paintContinue(); return; }
    try { continueDoc = await api(href); } catch (e) { continueDoc = null; }
    paintContinue();
  }

  // --- routing -----------------------------------------------------------------

  // The hash, resolved by the SERVER. The only thing done to it here is the
  // split the name promises — everything past the '?' is the server's grammar.
  async function fetchRoute(hash) {
    const act = R.actionOf(rootDoc, 'route');
    if (!act) return { view: 'library', document: null, passage: null, autoplay: false };
    const sep = act.href.includes('?') ? '&' : '?';
    try {
      return await api(act.href + sep + 'hash=' + encodeURIComponent(hash));
    } catch (e) {
      return {
        view: 'library', document: null, passage: null, autoplay: false,
        problem: e instanceof Problem ? e.problem : { detail: String(e && e.message || e) },
      };
    }
  }

  function navigate(hash) {
    stopPreview();   // the tile being left is not still being hovered
    if (location.hash === hash) applyRoute();
    else location.hash = hash;
  }
  function replaceHash(hash) {
    if (location.hash === hash) applyRoute();
    else location.replace(hash);
  }

  // itemHash keeps whatever passage the CURRENT route carries for that same
  // item: re-routing the item that is playing must not strip its passage.
  function itemHash(id, p) {
    if (p === undefined) {
      p = currentRoute && currentRoute.view === 'item' &&
          currentRoute.document && currentRoute.document.id === id ? currentRoute.passage : null;
    }
    return R.itemHash(id, root.passageQuery(p));
  }

  function hashNamesItem(id) {
    const h = location.hash || '';
    return h === '#/item/' + id || h.startsWith('#/item/' + id + '?');
  }

  // The work an item belongs to: one relation followed, and cached. Nothing
  // else on this side knows how a member finds its work.
  async function workOf(itemDoc) {
    const href = R.linkHref(itemDoc, 'work');
    if (!href) return null;
    try { return await doc(href); } catch (e) { return null; }
  }

  // The item's trickplay index, which is what puts a frame beside each chapter
  // on its page. It is a sidecar rather than a document — no `self`, so the
  // cache above has no key for it — and sheetIndex below is the one that
  // holds it, by address, shared with the tiles' hover previews. A file with
  // no sheets answers that it has none, which is an answer: the chapters are
  // the list of names they always were.
  async function trickplayOf(itemDoc) {
    const href = R.linkHref(itemDoc, 'trickplay');
    return href ? await sheetIndex(href) : null;
  }

  // What a member needs said about it: the work it belongs to, and the sheets
  // its chapters are pictured from.
  function itemCtx() { return { work: currentWork, trickplay: currentTrickplay }; }

  async function applyRoute() {
    const seq = ++routeSeq;
    const hash = location.hash || '#/';
    const r = await fetchRoute(hash);
    if (seq !== routeSeq) return;
    currentRoute = r;
    // The search box filters the library's tiles and searches the whole
    // shelf, so it is up on those two views and nowhere else. Leaving both of
    // them EMPTIES it: the box outlives the view it belongs to, and a home
    // drawn through a query nobody can see any more is a library with titles
    // missing from it.
    const searching = r.view === 'library' || r.view === 'search';
    $('search').hidden = !searching;
    if (!searching) { $('search').value = ''; libState.q = ''; }

    if (r.problem) {
      // A refusal is an answer: its sentence goes on the library view, which
      // is where its remedy points.
      libState.note = r.problem.detail || r.problem.title || '';
      pendingAutoplay = pendingPlay = null;
      if (hash !== '#/') { location.replace('#/'); return; }
    }

    const routedItem = r.view === 'item' && r.document ? r.document.id : null;
    // Leaving the item that is playing (browser back/forward included). Audio
    // does not stop for it: the sitting collapses into the bar and plays on.
    if (currentItem && routedItem !== currentItem.id) {
      if (playsOn()) collapse(); else closeStage();
    }
    if (r.view !== 'item') pendingExpand = false; // the bar's tap goes to an item or nowhere

    if (r.view === 'work') {
      currentWork = remember(r.document);
      currentItem = null;
      showStage(false);
      mount(R.work(currentWork, libState));
      pendingAutoplay = pendingPlay = null;
      return;
    }
    if (r.view === 'artist') {
      // The route answers a STUB for a shelf — its address and a link to it —
      // so the shelf itself is one relation away.
      let shelf = null;
      try { shelf = await doc(R.linkHref(r.document, 'artist') || r.document.self); }
      catch (e) { pendingAutoplay = pendingPlay = null; location.replace('#/'); return; }
      if (seq !== routeSeq) return;
      currentWork = null;
      currentItem = null;
      showStage(false);
      mount(R.artist(shelf));
      pendingAutoplay = pendingPlay = null;
      return;
    }
    if (r.view === 'search') {
      // Arrived by link rather than by typing: the box says what is being
      // searched for, so refining it carries on from there.
      const q = (r.document && r.document.query) || '';
      if ($('search').value.trim() !== q) $('search').value = q;
      currentWork = null;
      currentItem = null;
      showStage(false);
      mount(R.search(r.document));
      pendingAutoplay = pendingPlay = null;
      return;
    }
    if (r.view === 'item') {
      await openItem(remember(r.document), r, seq);
      return;
    }
    // The library, likewise a stub: the shelf is `links.library` — on the
    // route answer when there is one, and on the root when the answer was a
    // refusal and carries no document at all.
    const shelf = R.linkHref(r.document, 'library') || R.linkHref(rootDoc, 'library') ||
      (r.document && r.document.self);
    try { libraryDoc = await doc(shelf); }
    catch (e) { /* the note above is all there is to say */ }
    if (seq !== routeSeq) return;
    currentWork = null;
    currentItem = null;
    showStage(false);
    await paintLibrary();
    loadContinue();
    pendingAutoplay = pendingPlay = null;
  }

  async function openItem(itemDoc, r, seq) {
    if (!hashNamesItem(itemDoc.id)) {
      // The show form resolved to this item: hand over to the item route so
      // one code path plays every passage.
      pendingAutoplay = pendingPlay = null;
      location.replace(itemHash(itemDoc.id, r.passage || null));
      return;
    }
    currentItem = itemDoc;
    // Two relations, followed side by side: the page waits for one round trip
    // rather than two.
    const [wk, tp] = await Promise.all([workOf(itemDoc), trickplayOf(itemDoc)]);
    currentWork = wk;
    currentTrickplay = tp;
    if (seq !== routeSeq) return;

    if (itemDoc.medium === 'text') {
      const tp = root.isTextPassage(r.passage) ? r.passage : null;
      if (root.Reader.isOpen(itemDoc.id) &&
          root.passageQuery(root.Reader.passage()) === root.passageQuery(tp)) {
        showStage(true);
      } else {
        showStage(false);
        mount(R.item(itemDoc, itemCtx()));
        if (tp || pendingAutoplay === itemDoc.id) await openReader(itemDoc, tp);
      }
      pendingAutoplay = pendingPlay = null;
      return;
    }

    const rp = root.isTimedPassage(r.passage) ? r.passage : null;
    const samePlaying = root.Player.playingItem() === itemDoc.id;
    // The bar's own item, arrived at again. Its tap asks for the full chrome
    // back; arriving any other way leaves the sound where it is and draws the
    // page under the bar.
    if (collapsed && samePlaying) {
      if (!pendingExpand) {
        showStage(false);
        mount(R.item(itemDoc, itemCtx()));
        pendingAutoplay = pendingPlay = null;
        return;
      }
      expand();
    }
    pendingExpand = false;
    if (samePlaying && root.passageQuery(rp) === root.passageQuery(root.Player.passage())) {
      showStage(true); // already playing this item: keep the player as it is
    } else if (rp) {
      // A passage route plays on arrival, from its own start — `t` wins over
      // the saved place, so never null here.
      showStage(false);
      mount(R.item(itemDoc, itemCtx()));
      await startPlay(itemDoc, { seek: rp.t == null ? 0 : rp.t, passage: rp });
    } else if (samePlaying) {
      showStage(true); // "Keep watching" dropped the passage: nothing to restart
    } else {
      showStage(false);
      mount(R.item(itemDoc, itemCtx()));
      if (pendingAutoplay === itemDoc.id) await startPlay(itemDoc, {});
      else if (pendingPlay && pendingPlay.id === itemDoc.id) {
        await startPlay(itemDoc, { seek: pendingPlay.mode === 'zero' ? 0 : undefined });
      }
    }
    pendingAutoplay = pendingPlay = null;
  }

  // --- the reader --------------------------------------------------------------
  // A book is READ THROUGH A SESSION the way a film is played through one: the
  // item's own `read` action answers the session document, and that document
  // is the whole of what the pane is handed. The reader container is created
  // once (index.html's #reader-pane) and never re-rendered — moving an iframe
  // would reload the book.
  async function openReader(itemDoc, textPassage, act) {
    act = act || R.actionOf(itemDoc, 'read');
    if (!act) return;
    await root.Player.close();
    hideMini(); // a book is read in silence: the sitting the bar held is over
    let session;
    try {
      session = await api(act.href, {
        method: act.method || 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          client_id: who(),
          ...(textPassage ? { passage: { from: textPassage.from, to: textPassage.to } } : {}),
        }),
      });
    } catch (e) {
      libState.note = e.detail || String(e.message || e);
      return;
    }
    $('player-chrome').hidden = true;
    showStage(true);
    root.Player.holdSession(session); // the stop on the way out is the same one
    root.Reader.open(session, {
      container: $('reader-pane'),
      onKeep: s => {
        root.Player.holdSession(s || session);
        if (currentRoute && currentRoute.passage) location.replace(itemHash(session.item_id, null));
      },
      onBack: () => {
        closeStage();
        if (currentRoute && currentRoute.passage) location.replace(itemHash(session.item_id, null));
        else applyRoute();
      },
    });
  }

  // Leaving the stage: the stream and the book share one exit.
  function closeStage() {
    root.Reader.close();
    root.Player.close();
    hideMini();
    currentItem = null;
    showStage(false);
    loadContinue(); // a sitting ended: the resume points moved
    if (swReloadPending) location.reload();
  }

  // --- minting a passage link --------------------------------------------------
  //
  // Two actions answer the same document — { href, sentence } — and are shown
  // the same way: the WORK's `passage` (two places picked from what the work
  // published) and the SESSION's `link` (the two marks the server kept). The
  // picker's selects are named by the action's own input sketch, so the query
  // is the action's fields and nothing invented here.
  let copiedTimer = null;
  async function mint(req) {
    let href = req.href;
    const from = $('passage-from'), to = $('passage-to');
    if (from || to) {
      const q = [];
      if (from && from.value) q.push('from=' + encodeURIComponent(from.value));
      if (to && to.value) q.push('to=' + encodeURIComponent(to.value));
      if (q.length) href += (href.includes('?') ? '&' : '?') + q.join('&');
    }
    let minted;
    try { minted = await api(href, { method: req.method || 'GET' }); }
    catch (e) { note([e.detail, e.remedyText].filter(Boolean).join(' — ')); return; }
    await copyMinted(minted);
  }
  async function copyMinted(minted) {
    let copied = false;
    try { await navigator.clipboard.writeText(minted.href); copied = true; }
    catch (e) { /* no clipboard API on plain http, or refused: the text is shown anyway */ }
    showMinted(minted, copied);
  }
  // Handing a minted passage ON, the way the device it is held in hands things
  // on: a phone's own share sheet — Messages, Mail, AirDrop — when the browser
  // has one, and the clipboard when it does not. What is shared is
  // `share_href`, the spelling that unfurls into a card in a chat; the plain
  // link is what gets copied, because that is what is written down.
  async function share(minted) {
    if (!minted) return;
    if (navigator.share) {
      try {
        await navigator.share({ title: minted.sentence || '', url: minted.share_href || minted.href });
        showMinted(minted, false);
        return;
      } catch (e) {
        // A dismissed sheet is an answer, not a failure to work around.
        if (e && e.name === 'AbortError') return;
      }
    }
    await copyMinted(minted);
  }
  function showMinted(minted, copied) {
    const box = $('passage-link'), noteEl = $('passage-link-note');
    if (!box || !noteEl) return;
    if (copiedTimer) { clearTimeout(copiedTimer); copiedTimer = null; }
    noteEl.textContent = copied ? 'Copied' : (minted.sentence || 'Passage link');
    noteEl.classList.toggle('copied', copied);
    $('passage-link-url').textContent = minted.href;
    box.hidden = false;
    if (copied) copiedTimer = setTimeout(() => showMinted(minted, false), 2000);
  }

  // --- the profile gate --------------------------------------------------------

  // A profile is a row now — a name, the face the server gave it and whether
  // it is a kid's — so the gate draws faces and the chip wears one. Profiles
  // kept in this browser (a server too old to have any) are names on disk,
  // and read back as rows with no face.
  function localRows() {
    return JSON.parse(localStorage.localProfiles || '[]')
      .map(n => (typeof n === 'string' ? { name: n } : n)).filter(u => u && u.name);
  }

  async function profileRows() {
    const href = R.linkHref(rootDoc, 'profiles');
    if (!href) return { rows: localRows(), live: false };
    try {
      const users = await api(href);
      return { rows: (users || []).filter(u => u && u.name), live: true };
    } catch (e) {
      return { rows: localRows(), live: false };
    }
  }

  // The face a profile is drawn with, and the one the chip falls back to
  // when a profile was made before there were any.
  function faceOf(u) { return (u && u.avatar) || '👤'; }

  // The faces a new profile may wear are the SERVER's — `avatars` on the root
  // — so this file holds no emoji of its own beyond the one it falls back to
  // for a profile made before there were any. Picking none is allowed: the
  // create leaves `avatar` out and the server assigns the face it always did.
  let gateFace = '';
  function paintFaces() {
    const row = $('gate-faces');
    if (!row) return;
    const faces = (rootDoc && rootDoc.avatars) || [];
    if (faces.indexOf(gateFace) < 0) gateFace = '';
    row.hidden = !faces.length;
    row.innerHTML = faces.map(f => '<button data-face-pick="' + esc(f) + '"' +
      (f === gateFace ? ' class="picked"' : '') + '>' + esc(f) + '</button>').join('');
  }

  async function openGate() {
    $('gate').hidden = false;
    $('gate-kid-check').checked = false; // a fresh ask, not the last one's answer
    gateFace = '';
    paintFaces();
    $('gate-profiles').innerHTML = '<span style="color:#9a9daa;font-size:13px">loading…</span>';
    const { rows, live } = await profileRows();
    $('gate-note').textContent = live ? ''
      : 'Server has no profile support yet — profiles are stored in this browser only.';
    $('gate-profiles').innerHTML = rows.length
      ? rows.map(u => '<button data-profile="' + esc(u.name) + '" data-face="' + esc(faceOf(u)) + '">' +
          '<span class="gate-face">' + esc(faceOf(u)) + '</span>' + esc(u.name) +
          (u.kid ? '<span class="gate-kid">kids</span>' : '') + '</button>').join('')
      : '<span style="color:#9a9daa;font-size:13px">No profiles yet — create one below.</span>';
  }

  async function createProfile(name, kid) {
    name = String(name || '').trim();
    if (!name) return;
    const act = R.actionOf(rootDoc, 'create_profile');
    let made = null;
    if (act) {
      try {
        made = await api(act.href, {
          method: act.method || 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, kid: !!kid, ...(gateFace ? { avatar: gateFace } : {}) }),
        });
      } catch (e) {
        if (e.status === 409 || /exist/i.test(e.message)) made = { name }; // duplicate is fine
      }
    }
    if (!made) {
      const l = localRows();
      if (!l.some(u => u.name === name)) l.push({ name, kid: !!kid, avatar: gateFace });
      localStorage.localProfiles = JSON.stringify(l);
    }
    // The face the server gave it, or — where there was no server to give one
    // — the face picked at the gate, or the one nobody picked.
    selectProfile(name, (made && made.avatar) || gateFace || faceOf(null));
  }

  // The chip is who is watching: their face and their name.
  function paintChip() {
    $('profile-chip').textContent =
      (localStorage.profileFace || '👤') + ' ' + (profile || '');
  }

  async function selectProfile(name, face) {
    profile = name;
    localStorage.profileName = name;
    localStorage.profileFace = face || '👤';
    setProfileCookie(name);
    paintChip();
    $('gate').hidden = true;
    // Who is asking changes what the documents say: the root's resume link,
    // the work's progress, the item's place.
    forget();
    rootDoc = remember(await api(ROOT));
    await applyRoute();
  }

  // --- the settings panel ------------------------------------------------------
  //
  // The engineering controls — which client to play as, the scan, what the
  // encoder chose — behind a gear, because they are not what a viewer came
  // for. The autoplay-next toggle is the device's: the panel only asks the
  // Player for the setting and hands it back, so the transport row's own
  // button keeps working.
  function paintAutoplay() {
    $('settings-autoplay').textContent =
      'Autoplay next: ' + (root.Player.autoplay() ? 'on' : 'off');
  }
  function openSettings() {
    paintAutoplay();
    paintTenFoot();
    paintCues();
    $('settings').hidden = false;
    watchActivity(true);
  }

  function closeSettings() {
    $('settings').hidden = true;
    watchActivity(false);
  }

  // --- subtitle appearance -----------------------------------------------------
  //
  // How the subtitles READ is a matter of eyesight and of the room, never of
  // the file: no document has an opinion about how big a word should be. So
  // the three settings are kept in this browser and applied the two ways
  // WebVTT allows — the type and the ground through a ::cue rule written into
  // #cue-style, which is what a browser honours for a cue's font and its
  // background; the position through each cue's own `line`, which only the
  // device can set, so the player is handed the number and puts it on the
  // cues as each track loads.
  const CUE_SIZES = { small: '70%', medium: '100%', large: '140%', huge: '200%' };
  const CUE_GROUNDS = {
    box: 'background: rgba(0,0,0,0.75);',
    shadow: 'background: transparent; text-shadow: 0 1px 2px #000, 0 0 6px #000;',
    none: 'background: transparent;',
  };
  // Raised is three lines up from the bottom, which clears a transport bar and
  // most burned-in credits; bottom is wherever the cue itself asked to sit.
  const CUE_LINES = { bottom: 'auto', raised: -4 };
  const CUE_SELECTS = [['cue-size', 'size'], ['cue-ground', 'ground'], ['cue-position', 'position']];

  // A remembered value counts only if it is still one of the offered ones:
  // what is in storage was written by an older version of this file, or by
  // nobody at all.
  function offered(map, v, dflt) {
    return Object.prototype.hasOwnProperty.call(map, v) ? v : dflt;
  }

  function cuePrefs() {
    let s = {};
    try { s = JSON.parse(localStorage.cueStyle || '{}') || {}; } catch (e) { /* no storage */ }
    return {
      size: offered(CUE_SIZES, s.size, 'medium'),
      ground: offered(CUE_GROUNDS, s.ground, 'box'),
      position: offered(CUE_LINES, s.position, 'bottom'),
    };
  }

  function applyCues() {
    const p = cuePrefs();
    const style = $('cue-style');
    if (style) {
      style.textContent = '::cue { font-size: ' + CUE_SIZES[p.size] + '; ' + CUE_GROUNDS[p.ground] + ' }';
    }
    root.Player.setCueLine(CUE_LINES[p.position]);
    paintCues(p);
  }

  function paintCues(p) {
    p = p || cuePrefs();
    for (const [id, key] of CUE_SELECTS) {
      if ($(id)) $(id).value = p[key];
    }
  }

  function setCue(key, value) {
    const p = cuePrefs();
    p[key] = value;
    try { localStorage.cueStyle = JSON.stringify(p); } catch (e) { /* it lasts the sitting */ }
    applyCues();
  }

  // --- household activity ------------------------------------------------------
  //
  // Who is playing what, right now — the server's own list, followed from the
  // root (`links.activity`). It is READ while the panel is up and nowhere
  // else: a dashboard nobody is looking at is a poll nobody asked for, so the
  // timer starts with the panel and stops with it.
  //
  // Rows are the document's words — the profile, the work, the member's own
  // label, how it is being sent and where they have got to — and there is
  // nothing to press: the document offers no action, and neither does this.
  const ACTIVITY_MS = 5000;
  let activityTimer = null;

  function watchActivity(on) {
    if (activityTimer) clearInterval(activityTimer);
    activityTimer = null;
    if (!on) return;
    paintActivity();
    activityTimer = setInterval(paintActivity, ACTIVITY_MS);
  }

  async function paintActivity() {
    const box = $('settings-activity');
    const l = rootDoc && rootDoc.links && rootDoc.links.activity;
    if (!box) return;
    if (!l) { box.textContent = 'This server does not report activity.'; return; }
    let doc;
    try {
      doc = await api(l.href);
    } catch (e) {
      box.textContent = e.detail || 'Activity could not be read.';
      return;
    }
    if ($('settings').hidden) return; // the panel closed while we were asking
    const rows = (doc && doc.items) || [];
    box.innerHTML = rows.length
      ? rows.map(a =>
          '<div class="act-row">' +
            '<span class="act-who">' + esc(a.profile || '') + '</span>' +
            '<span class="act-what">' + esc(a.work_title || a.title || '') +
              (a.label && a.label !== a.work_title
                ? ' <span class="act-part">' + esc(a.label) + '</span>' : '') +
            '</span>' +
            '<span class="act-how">' +
              esc([a.position, a.method].filter(Boolean).join(' · ')) + '</span>' +
          '</div>').join('')
      : '<div class="act-none">Nothing is playing.</div>';
  }

  // --- ten-foot mode -----------------------------------------------------------
  //
  // A television is a screen nobody touches, read from a sofa and driven by
  // four arrows and an OK button. Ten-foot mode is that room: the attribute on
  // <body> is the whole of the layout (index.html sets the type scale, the
  // six-up grid and a focus ring that LIFTS the card), and the arrows below
  // are the rest — because a web page moves focus for none of them.
  //
  // Guessed on the first visit and remembered after: a wide screen, no finger,
  // and a user agent that says television. Any one of those alone is a desktop
  // at a desk, which is not this room. The gear overrules the guess either way.
  const TV_UA = /\b(smart-?tv|googletv|appletv|hbbtv|netcast|web0s|webos|tizen|viera|bravia|aftb|aftm|aftt|crkey|playstation|xbox)\b/i;

  function looksLikeTV() {
    try {
      return window.innerWidth >= 1280 &&
        !matchMedia('(pointer: coarse)').matches &&
        TV_UA.test(navigator.userAgent || '');
    } catch (e) { return false; }
  }

  let tenFoot = false;
  function setTenFoot(on) {
    tenFoot = !!on;
    document.body.toggleAttribute('data-ten-foot', tenFoot);
    try { localStorage.tenFoot = tenFoot ? 'on' : 'off'; } catch (e) { /* no storage: the guess stands */ }
    paintTenFoot();
  }
  function paintTenFoot() {
    const b = $('settings-tenfoot');
    if (b) b.textContent = 'Ten-foot mode: ' + (tenFoot ? 'on' : 'off');
  }

  // What the arrows may land on, in document order, each with the box it
  // occupies. Anything the layout folded away has no box — the view under a
  // raised stage, a closed season — and a thing with no box is not there to be
  // reached.
  const FOCUSABLE = 'a[href], button:not([disabled]), select:not([disabled]), ' +
    'input:not([disabled]), summary, [tabindex]:not([tabindex="-1"])';

  // A panel over the page takes the arrows with it: nothing behind the gate or
  // the gear is reachable for as long as one of them is up.
  function focusRoot() {
    if (!$('gate').hidden) return $('gate');
    if (!$('settings').hidden) return $('settings');
    return document;
  }

  function focusables() {
    const out = [];
    for (const el of focusRoot().querySelectorAll(FOCUSABLE)) {
      const box = el.getBoundingClientRect();
      if (box.width <= 0 || box.height <= 0) continue;
      out.push({ el: el, box: box });
    }
    return out;
  }

  // Where the ring goes when a whole view is swapped under it: the first
  // thing in the fresh view that has a box. Only on a television — at a desk
  // there is a pointer, and a page that grabs the focus on every navigation
  // is a page that fights the person using it.
  //
  // `lost` is the second call: the resume shelf lands on its own errand and
  // redraws the hero, which is the very element the ring was put on, so the
  // ring is put back — but only if it fell on the floor.
  function focusFirst(lost) {
    if (!tenFoot) return;
    if (!$('gate').hidden || !$('settings').hidden) return; // a panel owns the arrows
    if (!$('stage').hidden) return;                         // and so does a device
    const a = document.activeElement;
    if (lost && a && a !== document.body) return;
    for (const el of $('view').querySelectorAll(FOCUSABLE)) {
      const box = el.getBoundingClientRect();
      if (box.width <= 0 || box.height <= 0) continue;
      el.focus();
      return;
    }
  }

  const ARROWS = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down' };

  // The DEVICES own the arrows for as long as they are up: the player seeks
  // with them and the reader turns pages with them, and both bind the same
  // document keydown this file does. Focus inside the stage — or nowhere at
  // all — is theirs; a control the viewer has already stepped out to is not.
  function stageOwnsArrows(t) {
    const stage = $('stage');
    if (!stage || stage.hidden) return false;
    return !t || t === document.body || !t.closest || !!t.closest('#stage');
  }

  function typingIn(t) {
    return !!(t && (t.isContentEditable || /^(INPUT|SELECT|TEXTAREA)$/.test(t.tagName)));
  }

  // Move the ring one step in the arrow's direction, and say whether it moved:
  // an arrow that reaches nothing is left to whoever else wants it.
  function moveFocus(dir) {
    const items = focusables();
    if (!items.length) return false;
    const active = document.activeElement;
    const here = items.find(c => c.el === active);
    const i = root.Focus.pick(here ? here.box : null, items.map(c => c.box), dir);
    if (i < 0) return false;
    const el = items[i].el;
    el.focus();
    // A card the row has scrolled past is brought into the room rather than
    // focused off the edge of it.
    if (el.scrollIntoView) el.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    return true;
  }

  function onArrow(e) {
    const dir = ARROWS[e.key];
    if (!dir || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return false;
    if (typingIn(e.target) || stageOwnsArrows(e.target)) return false;
    if (!moveFocus(dir)) return false;
    e.preventDefault(); // the arrows would scroll the page out from under the ring
    return true;
  }

  // Back, as a remote sends it. The panels close first, then the sitting, then
  // the page's own history. Escape while the stage is up is left alone: the
  // device's Escape already leaves fullscreen and the help card before it
  // leaves the sitting, and that order is better than this one.
  function onBackKey(e) {
    if (e.key !== 'Backspace' && e.key !== 'Escape') return false;
    if (typingIn(e.target)) return false;
    if (!$('gate').hidden) return false; // who is watching has no way past
    if (!$('settings').hidden) { closeSettings(); e.preventDefault(); return true; }
    if (!$('stage').hidden) {
      if (e.key === 'Escape') return false;
      if (playsOn()) collapse(); else closeStage();
      applyRoute();
      e.preventDefault();
      return true;
    }
    e.preventDefault();
    history.back();
    return true;
  }

  // --- the scan ----------------------------------------------------------------

  async function scan() {
    const act = R.actionOf(rootDoc, 'scan');
    const status = R.linkHref(rootDoc, 'scan');
    if (!act) return;
    await api(act.href, { method: act.method || 'POST' });
    const el = $('scan-status');
    const poll = setInterval(async () => {
      let s;
      try { s = await api(status); } catch (e) { clearInterval(poll); return; }
      el.textContent = s.running
        ? (s.paused ? `scan paused (playback active) — ${s.probed}/${s.total} probed`
                    : `scanning… ${s.probed}/${s.total} probed, ${s.skipped} unchanged`)
        : `done: ${s.probed} probed, ${s.skipped} unchanged, ${s.removed} removed` +
          (s.errors ? ', ' + s.errors + ' errors' : '');
      if (!s.running) {
        clearInterval(poll);
        forget(); // the library changed under every cached document
        libraryDoc = null;
        applyRoute();
      }
    }, 1000);
  }

  // --- delegated events --------------------------------------------------------
  //
  // One listener for the whole page. A control carries the href and the method
  // the DOCUMENT gave it (data-act/data-href/data-method) and a navigation
  // carries a hash (data-nav): nothing between here and the server is composed.
  function onClick(e) {
    const t = e.target;
    const gate = t.closest('[data-profile]');
    if (gate) { selectProfile(gate.dataset.profile, gate.dataset.face); return; }
    // A face picked for the profile being made: the pick is remembered until
    // Create sends it, and picking the one already picked drops it again.
    const face = t.closest('[data-face-pick]');
    if (face) {
      gateFace = gateFace === face.dataset.facePick ? '' : face.dataset.facePick;
      paintFaces();
      return;
    }
    const chip = t.closest('[data-genre]');
    if (chip) { libState.genre = chip.dataset.genre || null; paintLibrary(); return; }
    // The chip over the shelf says which words are filtering it; pressing it
    // is the box going empty.
    if (t.closest('[data-clear-q]')) { clearSearch(); return; }
    // The three buttons that end a sitting rather than following a relation:
    // Back out of the player, Back out of a passage, and Cancel on up next.
    if (t.closest('#upnext-cancel')) { e.stopPropagation(); root.Player.cancelUpNext(); return; }
    if (t.closest('#passage-back')) {
      const id = currentItem && currentItem.id;
      closeStage();
      if (id != null) replaceHash(itemHash(id, null));
      return;
    }
    if (t.closest('#detail-back')) {
      // Back out of the player: an audio sitting collapses into the bar
      // rather than ending, and the item's own page comes back under it.
      if (playsOn()) collapse(); else closeStage();
      applyRoute();
      return;
    }
    const goto_ = t.closest('[data-goto]');
    if (goto_) { root.Player.goTo(Number(goto_.dataset.goto), goto_.dataset.mode || 'nav'); return; }
    const seek = t.closest('[data-seek]');
    if (seek) { root.Player.playFrom(currentItem, Number(seek.dataset.seek)); return; }
    const actEl = t.closest('[data-act]');
    if (actEl) {
      // A form carries an action the same way a button does, but it is
      // SUBMITTED rather than clicked: its fields are the body. Leave the
      // click alone so the browser validates the fields and raises submit.
      if (actEl.tagName === 'FORM') return;
      e.preventDefault();
      e.stopPropagation();
      // A control that also names a route is TAKEN there: the resume shelf's
      // rows are the case — the action is `play` on the item, and the item's
      // own page is where the place to pick up from is written.
      if (actEl.dataset.nav) {
        pendingAutoplay = Number(actEl.dataset.nav.replace('#/item/', ''));
        morph(actEl);
        navigate(actEl.dataset.nav);
        return;
      }
      invoke(actEl);
      return;
    }
    // The up-next panel is one big control: anywhere on it plays the next.
    if (t.closest('#upnext')) { root.Player.advance(); return; }
    const navEl = t.closest('[data-nav]');
    if (navEl) {
      e.preventDefault();
      if (navEl.dataset.autoplay) pendingAutoplay = Number(navEl.dataset.nav.replace('#/item/', ''));
      morph(navEl);
      // The mini bar: a tap on it is a request for the full chrome back, which
      // is what tells the route apart from merely arriving at the same item.
      if (navEl.dataset.expand) pendingExpand = true;
      navigate(navEl.dataset.nav);
    }
  }

  // --- the moving tile ---------------------------------------------------------
  //
  // A pointer that RESTS on a tile — six hundred milliseconds, not a pass over
  // it on the way somewhere else — sets the film going on the poster: the very
  // sheets the scrub bar previews from, four frames a second, gone the moment
  // the pointer leaves. It is a nicety and it behaves like one — never where
  // the pointer is a finger, because nothing rests on a touchscreen and a
  // sheet is a big picture to fetch for a tap, and never where less motion was
  // asked for.
  //
  // Every address here came out of a document: the index is the tile's own
  // `links.trickplay` (the renderer carried it into data-trickplay) and the
  // sheets are addresses INSIDE the index it answers.
  const PREVIEW_DELAY = 600;   // how long a pointer rests before the tile moves
  const PREVIEW_MS = 250;      // four frames a second
  const PREVIEW_FRAMES = 20;   // how many frames one loop plays
  const sheetIndexes = new Map();  // trickplay href -> the index, or null for none
  let preview = null;          // { card, timer, el, tick } while one is running

  function previewWanted() {
    try {
      return !matchMedia('(pointer: coarse)').matches &&
        !matchMedia('(prefers-reduced-motion: reduce)').matches;
    } catch (e) { return false; }
  }

  function onPointerOver(e) {
    const t = e.target;
    const card = t && t.closest ? t.closest('[data-trickplay]') : null;
    if (preview && preview.card === card) return;   // moving about inside the same tile
    stopPreview();
    if (!card || !previewWanted()) return;
    preview = { card: card, el: null, tick: null, timer: setTimeout(() => startPreview(card), PREVIEW_DELAY) };
  }

  function onPointerOut(e) {
    if (!preview) return;
    if (e.relatedTarget && preview.card.contains(e.relatedTarget)) return;
    stopPreview();
  }

  function stopPreview() {
    if (!preview) return;
    clearTimeout(preview.timer);
    clearInterval(preview.tick);
    if (preview.el) preview.el.remove();
    preview = null;
  }

  async function sheetIndex(href) {
    if (sheetIndexes.has(href)) return sheetIndexes.get(href);
    let idx = null;
    try { idx = await api(href); } catch (e) { /* none were ever made: no preview */ }
    sheetIndexes.set(href, idx);
    return idx;
  }

  async function startPreview(card) {
    const wrap = card.querySelector('.poster-wrap');
    if (!wrap) return;
    const idx = await sheetIndex(card.dataset.trickplay);
    if (!idx || !preview || preview.card !== card) return;
    const list = idx.sheet_hrefs || [];
    if (!list.length) return;
    // ONE sheet, from the middle of the film: a hover lasts seconds, and a
    // whole film at four frames a second would be a loop minutes long — and
    // every sheet a fetch. The frames are taken spread across that sheet, so
    // what plays is a passage of the film rather than half a minute of it.
    const sheet = list[Math.floor(list.length / 2)];
    const per = (idx.cols || 1) * (idx.rows || 1);
    const step = Math.max(1, Math.floor(per / PREVIEW_FRAMES));
    // The sheet is fetched before anything is shown: a preview that arrives
    // blank and fills in reads as a broken picture.
    const pic = new Image();
    pic.onload = () => {
      if (!preview || preview.card !== card || !wrap.isConnected) return;
      const el = document.createElement('div');
      el.className = 'tile-preview';
      wrap.appendChild(el);
      preview.el = el;
      let n = 0;
      const draw = () => {
        // The view under it can be swapped away mid-loop; a tile that is no
        // longer on the page is not being hovered.
        if (!el.isConnected) { stopPreview(); return; }
        frame(el, wrap, idx, sheet, (n++ * step) % per);
      };
      draw();
      preview.tick = setInterval(draw, PREVIEW_MS);
    };
    pic.src = sheet;
  }

  // One frame of the sheet, drawn to COVER the poster the way the artwork
  // does: the sheet is a grid of wide frames and a tile is a tall box, so the
  // frame is scaled until it fills and what will not fit is cropped evenly.
  function frame(el, wrap, idx, sheet, n) {
    const cols = idx.cols || 1, rows = idx.rows || 1;
    const box = wrap.getBoundingClientRect();
    const tw = idx.tile_width || 1, th = idx.tile_height || 1;
    const scale = Math.max(box.width / tw, box.height / th);
    const w = tw * scale, h = th * scale;
    const col = n % cols, row = Math.floor(n / cols);
    el.style.backgroundImage = 'url("' + sheet + '")';
    el.style.backgroundSize = (cols * w) + 'px ' + (rows * h) + 'px';
    el.style.backgroundPosition =
      (-(col * w) + (box.width - w) / 2) + 'px ' +
      (-(row * h) + (box.height - h) / 2) + 'px';
  }

  // A poster is fetched after its frame is drawn, so the frame shimmers until
  // the picture lands. `load` does not bubble, so the one listener for the
  // whole page catches it on the way down instead.
  function onLoad(e) {
    const img = e.target;
    if (img && img.tagName === 'IMG') markLoaded(img);
  }
  // A picture that fails is settled too: the frame stops shimmering rather
  // than promising a picture that is not coming.
  function onImageError(e) {
    const img = e.target;
    if (img && img.tagName === 'IMG') markLoaded(img);
  }

  // The keyboard does what the mouse does. The renderers' tiles are divs with
  // tabindex, not links, so nothing activates them on their own: Enter and
  // Space on one are handed to the same delegation, which already knows how to
  // read a control. Native controls keep their own behaviour.
  function onKeydown(e) {
    // On a television the arrows are the only pointer there is, so they move
    // the ring; Backspace is the remote's Back. Neither means anything at a
    // desk, which is why both are ten-foot mode's alone.
    if (tenFoot && !e.defaultPrevented && (onArrow(e) || onBackKey(e))) return;
    if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return;
    const t = e.target;
    if (!t || !t.closest) return;
    if (t.closest('a, button, input, textarea, select')) return;
    if (!t.closest('[data-nav], [data-act]')) return;
    e.preventDefault(); // Space would scroll the page
    onClick(e);
  }

  // Invoking a control is the same three lines whatever it is: the action's
  // method, the action's href, and the answer handed to whoever owns it.
  async function invoke(el) {
    const name = el.dataset.act;
    const req = { href: el.dataset.href, method: el.dataset.method || 'POST', name };
    if (el.dataset.clear) req.body = { clear: true };
    // The tick sends back the value the DOCUMENT chose: the action's `input`
    // carried it as a literal and the button carries it here, so which way
    // the mark goes was never the browser's decision.
    if (el.dataset.watched) req.body = { watched: el.dataset.watched === 'true' };
    // Marking a thing watched, and dropping a row off the resume shelf, both
    // move a PLACE — and the place is what the row's ✓, the shelf and a
    // show's Next up are all drawn from. So the answer is followed by
    // forgetting every cached document and re-reading the view, rather than
    // by patching the screen where it stands.
    // Putting a title on My List, and taking it off again, move a shelf the
    // same way: the row on the home and the bookmark on every tile are drawn
    // from one answer, so the write is followed by the same forget-and-reread.
    if (name === 'watched' || name === 'forget' ||
        name === 'save' || name === 'unsave') { await write(req); return; }
    // `read` is the kernel's: a book's sitting is the reader pane, not the
    // device. Everything else is the device's, and it takes the action as it
    // was written on the button.
    if (name === 'read') { await openReader(currentItem, null, req); return; }
    // Starting something is the end of a collapsed sitting: the bar belongs to
    // what was playing, and this is another one.
    if (name === 'play' || name === 'resume') collapsed = false;
    // The two minting actions are the same answer shown the same way.
    if (name === 'passage' || name === 'link') { await mint(req); return; }
    await root.Player.invoke(req, { item: currentItem, work: currentWork });
  }

  // One write, and the view read again after it. A refusal is said out loud
  // and nothing is re-read: the screen still shows what the server still
  // holds.
  async function write(req) {
    try {
      await api(req.href, {
        method: req.method,
        headers: req.body ? { 'Content-Type': 'application/json' } : undefined,
        body: req.body ? JSON.stringify(req.body) : undefined,
      });
    } catch (err) {
      note(err instanceof Problem ? err.detail : String(err.message || err));
      return;
    }
    forget();
    applyRoute();
  }

  // A form is an action with a BODY: the fields a renderer drew from the
  // action's `input` sketch, sent as JSON to the action's own href. Fixing an
  // identity is the one so far, and it is why the button alone could never
  // work — a POST with no body says nothing.
  async function onSubmit(e) {
    const form = e.target.closest('[data-act]');
    if (!form || form.tagName !== 'FORM') return;
    e.preventDefault();
    try {
      await api(form.dataset.href, {
        method: form.dataset.method || 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(formBody(form)),
      });
    } catch (err) {
      note(err instanceof Problem ? err.detail : String(err.message || err));
      return;
    }
    // The answer changed what every document says about this thing.
    forget();
    applyRoute();
  }

  // The body one form sends: a field per input, typed as the sketch drew it —
  // a number control is a number — and an empty optional field left out
  // rather than sent as "".
  function formBody(form) {
    const body = {};
    for (const el of form.querySelectorAll('input[name]')) {
      const v = el.value.trim();
      if (v === '' && !el.required) continue;
      body[el.name] = el.type === 'number' ? Number(v) : v;
    }
    return body;
  }

  // --- boot --------------------------------------------------------------------

  async function boot() {
    document.addEventListener('click', onClick);
    document.addEventListener('keydown', onKeydown);
    document.addEventListener('load', onLoad, true); // capture: load does not bubble
    document.addEventListener('error', onImageError, true);
    document.addEventListener('submit', onSubmit);
    document.addEventListener('pointerover', onPointerOver);
    document.addEventListener('pointerout', onPointerOut);
    window.addEventListener('hashchange', applyRoute);
    window.addEventListener('beforeunload', () => root.Player.close());
    $('profile-chip').onclick = openGate;
    const newProfile = () => createProfile($('gate-name').value, $('gate-kid-check').checked);
    $('gate-create').onclick = newProfile;
    $('gate-name').onkeydown = e => { if (e.key === 'Enter') newProfile(); };
    $('scan').onclick = scan;
    $('search').oninput = onSearchInput;
    $('search').onkeydown = onSearchKey;
    $('gear').onclick = openSettings;
    $('settings-close').onclick = closeSettings;
    $('settings').onclick = e => { if (e.target.id === 'settings') closeSettings(); };
    $('settings-autoplay').onclick = () => { root.Player.setAutoplay(!root.Player.autoplay()); paintAutoplay(); };
    $('settings-tenfoot').onclick = () => setTenFoot(!tenFoot);
    for (const [id, key] of CUE_SELECTS) {
      $(id).onchange = e => setCue(key, e.target.value);
    }
    // Remembered if it was ever chosen, guessed if it was not.
    let remembered = null;
    try { remembered = localStorage.tenFoot || null; } catch (e) { /* no storage */ }
    setTenFoot(remembered ? remembered === 'on' : looksLikeTV());

    // The shell is cache-first (sw.js), so the first visit after a deploy runs
    // the OLD index.html while the new worker installs behind it. When a new
    // worker takes control of a page an old one was serving, reload once —
    // unless the player is mid-stream: a film should not restart because a
    // deploy landed. A first-ever install (no previous controller) does not
    // reload. (The rule PR #11 added; sw.js carries the other half.)
    if ('serviceWorker' in navigator) {
      const hadController = !!navigator.serviceWorker.controller;
      navigator.serviceWorker.register('/sw.js').catch(e => console.warn('SW registration failed', e));
      navigator.serviceWorker.addEventListener('controllerchange', () => {
        if (!hadController) return;
        if (root.Player.playingItem() != null) { swReloadPending = true; return; }
        location.reload();
      });
    }

    if (profile) { paintChip(); setProfileCookie(profile); }
    if (!location.hash) history.replaceState(null, '', '#/');

    root.Player.init(kernel);
    applyCues();   // the device is made now, so the cue line has somewhere to land

    try { rootDoc = remember(await api(ROOT)); }
    catch (e) { $('view').innerHTML = '<div id="empty">' + esc(e.detail || e.message) + '</div>'; return; }
    if (!profile) openGate();

    await applyRoute();

    // The system panel: the LAN-reachable base a cast device can resolve, and
    // what the encoder chose.
    try {
      system = await api(R.linkHref(rootDoc, 'system'));
      root.Player.setBaseUrl(system.base_url || location.origin);
      const hw = system.hardware || {};
      $('hw').textContent = hw.chosen
        ? `HW: ${hw.chosen.kind} (${Object.values(hw.chosen.encoders).join(', ')})`
        : 'HW: software only';
      $('hw').title = (hw.trace || []).map(s => `${s.ok ? '✓' : '✗'} ${s.candidate}: ${s.detail}`).join('\n');
    } catch (e) { /* the panel is a nicety */ }
  }

  const kernel = {
    api, Problem, doc, remember, forget,
    boot, applyRoute, navigate, replaceHash, itemHash,
    profile: who, showStage, renderSession, closeStage, loadContinue, mint, share,
    currentRoute: () => currentRoute,
    collapsed: () => collapsed,
    currentWork: () => currentWork,
    currentItem: () => currentItem,
    setPendingPlay: p => { pendingPlay = p; },
    note,
  };
  root.Kernel = kernel;
})(typeof window !== 'undefined' ? window : this);
