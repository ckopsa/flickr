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
  let currentItem = null;        // the item document on screen
  let pendingAutoplay = null;    // item id to start once its route renders
  let pendingPlay = null;        // { id, mode: 'nav' | 'zero' } from a prev/next
  let routeSeq = 0;
  let swReloadPending = false;   // a new worker took control mid-stream

  // --- views -------------------------------------------------------------------

  function mount(html) { $('view').innerHTML = html; }

  function showStage(on) {
    $('stage').hidden = !on;
    $('view').hidden = !!on;
  }

  // The session chrome, re-rendered from the session document every time that
  // document changes — and the ONE device element moved into it. Nothing in
  // the chrome is a <video>; the buffer belongs to the device, not the render.
  function renderSession(session) {
    const chrome = $('player-chrome');
    const html = R.session(session, { work: currentWork });
    chrome.innerHTML = html;
    chrome.hidden = !html;
    if (!html) return; // a reading session has no player chrome; the pane is its own
    root.Player.attach(chrome);
    showStage(true);
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

  function paintLibrary() {
    mount(R.library(libraryDoc, libState));
    libState.note = '';
    paintContinue();
    const box = $('search');
    if (box) {
      box.oninput = () => {
        libState.q = box.value.trim().toLowerCase();
        const grid = R.libraryGrid(libraryDoc, libState);
        $('grid').innerHTML = grid.html;
        $('lib-count').textContent = grid.count ? grid.count + ' title' + (grid.count === 1 ? '' : 's') : '';
      };
    }
  }

  function paintContinue() {
    const box = $('cw');
    if (!box) return;
    const html = continueDoc ? R.continueShelf(continueDoc) : '';
    box.innerHTML = html;
    box.hidden = !html;
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

  async function applyRoute() {
    const seq = ++routeSeq;
    const hash = location.hash || '#/';
    const r = await fetchRoute(hash);
    if (seq !== routeSeq) return;
    currentRoute = r;

    if (r.problem) {
      // A refusal is an answer: its sentence goes on the library view, which
      // is where its remedy points.
      libState.note = r.problem.detail || r.problem.title || '';
      pendingAutoplay = pendingPlay = null;
      if (hash !== '#/') { location.replace('#/'); return; }
    }

    const routedItem = r.view === 'item' && r.document ? r.document.id : null;
    // Leaving the item that is playing (browser back/forward included).
    if (currentItem && routedItem !== currentItem.id) closeStage();

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
    paintLibrary();
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
    currentWork = await workOf(itemDoc);
    if (seq !== routeSeq) return;

    if (itemDoc.medium === 'text') {
      const tp = root.isTextPassage(r.passage) ? r.passage : null;
      if (root.Reader.isOpen(itemDoc.id) &&
          root.passageQuery(root.Reader.passage()) === root.passageQuery(tp)) {
        showStage(true);
      } else {
        showStage(false);
        mount(R.item(itemDoc, { work: currentWork }));
        if (tp || pendingAutoplay === itemDoc.id) await openReader(itemDoc, tp);
      }
      pendingAutoplay = pendingPlay = null;
      return;
    }

    const rp = root.isTimedPassage(r.passage) ? r.passage : null;
    const samePlaying = root.Player.playingItem() === itemDoc.id;
    if (samePlaying && root.passageQuery(rp) === root.passageQuery(root.Player.passage())) {
      showStage(true); // already playing this item: keep the player as it is
    } else if (rp) {
      // A passage route plays on arrival, from its own start — `t` wins over
      // the saved place, so never null here.
      showStage(false);
      mount(R.item(itemDoc, { work: currentWork }));
      await root.Player.play(itemDoc, { seek: rp.t == null ? 0 : rp.t, passage: rp });
    } else if (samePlaying) {
      showStage(true); // "Keep watching" dropped the passage: nothing to restart
    } else {
      showStage(false);
      mount(R.item(itemDoc, { work: currentWork }));
      if (pendingAutoplay === itemDoc.id) await root.Player.play(itemDoc, {});
      else if (pendingPlay && pendingPlay.id === itemDoc.id) {
        await root.Player.play(itemDoc, { seek: pendingPlay.mode === 'zero' ? 0 : undefined });
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
    let copied = false;
    try { await navigator.clipboard.writeText(minted.href); copied = true; }
    catch (e) { /* no clipboard API on plain http, or refused: the text is shown anyway */ }
    showMinted(minted, copied);
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

  async function profileNames() {
    const href = R.linkHref(rootDoc, 'profiles');
    if (!href) return { names: JSON.parse(localStorage.localProfiles || '[]'), live: false };
    try {
      const users = await api(href);
      return { names: (users || []).map(u => u.name).filter(Boolean), live: true };
    } catch (e) {
      return { names: JSON.parse(localStorage.localProfiles || '[]'), live: false };
    }
  }

  async function openGate() {
    $('gate').hidden = false;
    $('gate-profiles').innerHTML = '<span style="color:#9a9daa;font-size:13px">loading…</span>';
    const { names, live } = await profileNames();
    $('gate-note').textContent = live ? ''
      : 'Server has no profile support yet — profiles are stored in this browser only.';
    $('gate-profiles').innerHTML = names.length
      ? names.map(n => '<button data-profile="' + esc(n) + '">' + esc(n) + '</button>').join('')
      : '<span style="color:#9a9daa;font-size:13px">No profiles yet — create one below.</span>';
  }

  async function createProfile(name) {
    name = String(name || '').trim();
    if (!name) return;
    const act = R.actionOf(rootDoc, 'create_profile');
    let stored = false;
    if (act) {
      try {
        await api(act.href, {
          method: act.method || 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name }),
        });
        stored = true;
      } catch (e) {
        if (e.status === 409 || /exist/i.test(e.message)) stored = true; // duplicate is fine
      }
    }
    if (!stored) {
      const l = JSON.parse(localStorage.localProfiles || '[]');
      if (!l.includes(name)) l.push(name);
      localStorage.localProfiles = JSON.stringify(l);
    }
    selectProfile(name);
  }

  async function selectProfile(name) {
    profile = name;
    localStorage.profileName = name;
    setProfileCookie(name);
    $('profile-chip').textContent = '👤 ' + name;
    $('gate').hidden = true;
    // Who is asking changes what the documents say: the root's resume link,
    // the work's progress, the item's place.
    forget();
    rootDoc = remember(await api(ROOT));
    await applyRoute();
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
    if (gate) { selectProfile(gate.dataset.profile); return; }
    const chip = t.closest('[data-genre]');
    if (chip) { libState.genre = chip.dataset.genre || null; paintLibrary(); return; }
    // The three buttons that end a sitting rather than following a relation:
    // Back out of the player, Back out of a passage, and Cancel on up next.
    if (t.closest('#upnext-cancel')) { e.stopPropagation(); root.Player.cancelUpNext(); return; }
    if (t.closest('#passage-back')) {
      const id = currentItem && currentItem.id;
      closeStage();
      if (id != null) replaceHash(itemHash(id, null));
      return;
    }
    if (t.closest('#detail-back')) { closeStage(); applyRoute(); return; }
    const goto_ = t.closest('[data-goto]');
    if (goto_) { root.Player.goTo(Number(goto_.dataset.goto), goto_.dataset.mode || 'nav'); return; }
    const seek = t.closest('[data-seek]');
    if (seek) { root.Player.playFrom(currentItem, Number(seek.dataset.seek)); return; }
    const actEl = t.closest('[data-act]');
    if (actEl) {
      e.preventDefault();
      e.stopPropagation();
      // A control that also names a route is TAKEN there: the resume shelf's
      // rows are the case — the action is `play` on the item, and the item's
      // own page is where the place to pick up from is written.
      if (actEl.dataset.nav) {
        pendingAutoplay = Number(actEl.dataset.nav.replace('#/item/', ''));
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
      navigate(navEl.dataset.nav);
    }
  }

  // Invoking a control is the same three lines whatever it is: the action's
  // method, the action's href, and the answer handed to whoever owns it.
  async function invoke(el) {
    const name = el.dataset.act;
    const req = { href: el.dataset.href, method: el.dataset.method || 'POST', name };
    if (el.dataset.clear) req.body = { clear: true };
    // `read` is the kernel's: a book's sitting is the reader pane, not the
    // device. Everything else is the device's, and it takes the action as it
    // was written on the button.
    if (name === 'read') { await openReader(currentItem, null, req); return; }
    // The two minting actions are the same answer shown the same way.
    if (name === 'passage' || name === 'link') { await mint(req); return; }
    await root.Player.invoke(req, { item: currentItem, work: currentWork });
  }

  // --- boot --------------------------------------------------------------------

  async function boot() {
    document.addEventListener('click', onClick);
    window.addEventListener('hashchange', applyRoute);
    window.addEventListener('beforeunload', () => root.Player.close());
    $('profile-chip').onclick = openGate;
    $('gate-create').onclick = () => createProfile($('gate-name').value);
    $('gate-name').onkeydown = e => { if (e.key === 'Enter') createProfile($('gate-name').value); };
    $('scan').onclick = scan;

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

    if (profile) { $('profile-chip').textContent = '👤 ' + profile; setProfileCookie(profile); }
    if (!location.hash) history.replaceState(null, '', '#/');

    root.Player.init(kernel);

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
    profile: who, showStage, renderSession, closeStage, loadContinue, mint,
    currentRoute: () => currentRoute,
    currentWork: () => currentWork,
    currentItem: () => currentItem,
    setPendingPlay: p => { pendingPlay = p; },
    note,
  };
  root.Kernel = kernel;
})(typeof window !== 'undefined' ? window : this);
