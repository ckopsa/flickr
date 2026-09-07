// flickr — the reader pane: EPUB books on epub.js (vendored under web/vendor,
// so a LAN-only engine loads nothing from the internet at read time).
//
// A book is read, not played, but it shares the player's contract with
// index.html, which hooks in through four calls:
//   Reader.open(item, opts)   Read pressed, a continue-reading tap, or a
//                             from/to route arriving
//   Reader.close()            the route left the item (closePlayer)
//   Reader.isOpen(id)         is this item open in the pane?
//   Reader.passage()          the passage record the pane was opened with
// Everything else — the book, the TOC, paging, the readout — lives here.
//
// Position leaves through opts.onPlace({ locator, fraction, section }), which
// index.html routes into saveProgress(): the one gate for /api/progress, closed
// while a passage is set. This file never writes progress on its own, and in
// passage mode it does not even call onPlace — two locks on the same door.
//
// Locators (parseLocator etc.) and the end predicate are pure functions in
// passage.js; this file resolves them against the open book.
(function (root) {
  'use strict';

  const MISSING = 'The reader library is missing: web/vendor/epub.min.js and web/vendor/jszip.min.js ' +
                  'are not vendored (see web/vendor/README.md).';
  const PLACE_DELAY_MS = 800;   // debounce for position reports (a page turn per second is bursty)
  const LOCATION_CHARS = 1024;  // epub.js location granularity (chars per location)

  let container = null, ui = null;
  let item = null, opts = null, book = null, rendition = null, compareCFI = null;
  let passage = null;           // the route passage the pane was opened with; null = normal reading
  let from = null, to = null;   // its bounds, parsed
  let ended = false;            // the end bound has been reached: no more advancing
  let locationsReady = false;   // book.locations generated or loaded: fractions are the book's own
  let openSeq = 0;              // stale async work from an earlier open checks this
  let placeTimer = null, pendingPlace = null;
  let lastLocation = null;

  // --- the pane ---------------------------------------------------------------

  function build(c) {
    if (ui && container === c) return;
    container = c;
    c.innerHTML =
      '<div class="rd-bar">' +
        '<button class="rd-toc-btn" title="Contents">☰</button>' +
        '<div class="rd-title"></div>' +
        '<div class="rd-readout"></div>' +
        '<button class="rd-close" title="Close the book">✕ Back</button>' +
      '</div>' +
      '<div class="rd-body">' +
        '<div class="rd-toc" hidden></div>' +
        '<div class="rd-view"></div>' +
      '</div>' +
      '<div class="rd-nav">' +
        '<button class="rd-prev" title="Previous page (←)">‹ Prev</button>' +
        '<div class="rd-msg"></div>' +
        '<button class="rd-next" title="Next page (→)">Next ›</button>' +
      '</div>' +
      '<div class="rd-end" hidden>' +
        '<div class="rd-end-title">End of the passage</div>' +
        '<div class="rd-end-sub"></div>' +
        '<button class="rd-keep">Keep reading</button>' +
        '<button class="rd-back">Back</button>' +
      '</div>';
    const q = sel => c.querySelector(sel);
    ui = {
      title: q('.rd-title'), readout: q('.rd-readout'), toc: q('.rd-toc'), view: q('.rd-view'),
      msg: q('.rd-msg'), end: q('.rd-end'), endSub: q('.rd-end-sub'),
      prev: q('.rd-prev'), next: q('.rd-next'), tocBtn: q('.rd-toc-btn'), close: q('.rd-close'),
      keep: q('.rd-keep'), back: q('.rd-back'),
    };
    ui.prev.onclick = prev;
    ui.next.onclick = next;
    ui.tocBtn.onclick = () => { ui.toc.hidden = !ui.toc.hidden; };
    ui.close.onclick = () => opts && opts.onBack && opts.onBack();
    ui.back.onclick = () => opts && opts.onBack && opts.onBack();
    ui.keep.onclick = keepReading;
    document.addEventListener('keydown', onKey);
  }

  function onKey(e) {
    if (!isOpen() || e.defaultPrevented) return;
    const t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'SELECT' || t.tagName === 'TEXTAREA')) return;
    if (e.key === 'ArrowLeft') { prev(); e.preventDefault(); }
    else if (e.key === 'ArrowRight') { next(); e.preventDefault(); }
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, ch =>
      ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
  }
  function titleOf(it) {
    return (it.enrichment && it.enrichment.title) ||
           (it.media_info && it.media_info.document && it.media_info.document.title) ||
           (it.identity && it.identity.title) || String(it.object_key || '').split('/').pop();
  }
  function sectionCount() {
    if (book && book.spine && book.spine.length > 0) return book.spine.length;
    const mi = item && item.media_info;
    return (mi && (mi.sections || (mi.chapters && mi.chapters.length))) || 0;
  }
  function clamp01(f) { return !(f > 0) ? 0 : f > 1 ? 1 : f; }

  // The TOC comes from media_info.chapters — one entry per spine item, in
  // order, titled from the book's own nav/NCX by the prober — so section i
  // (1-based) is spine item i-1 and a tap needs no lookup in the book.
  function renderTOC() {
    const chs = (item.media_info && item.media_info.chapters) || [];
    ui.toc.innerHTML = '';
    if (!chs.length) { ui.toc.innerHTML = '<div class="rd-toc-empty">No contents listed.</div>'; return; }
    chs.forEach((ch, i) => {
      const b = document.createElement('button');
      b.dataset.section = String(i + 1);
      b.textContent = ch.title || ('Section ' + (i + 1));
      b.onclick = () => { goSection(i + 1); ui.toc.hidden = true; };
      ui.toc.appendChild(b);
    });
  }
  function markTOC(section) {
    for (const b of ui.toc.querySelectorAll('button')) {
      b.classList.toggle('current', Number(b.dataset.section) === section);
    }
  }

  function fail(msg) {
    ui.msg.textContent = msg;
    ui.view.innerHTML = '<div class="rd-fail">' + esc(msg) + '</div>';
    ui.prev.disabled = ui.next.disabled = true;
  }

  // --- opening ----------------------------------------------------------------

  // opts: { container, passage, clientId, onPlace(place), onKeep(), onBack() }
  async function open(it, o) {
    const seq = ++openSeq;
    teardown();
    item = it;
    opts = o || {};
    build(opts.container);
    passage = root.isTextPassage(opts.passage) ? opts.passage : null;
    from = passage ? root.parseLocator(passage.from) : null;
    to = passage ? root.parseLocator(passage.to) : null;
    // A `to` before `from` is no bound, the way an `end` before `t` is
    // dropped — judged only where the two are comparable without the book.
    if (from && to && from.kind === to.kind &&
        ((to.kind === 'ch' && to.n < from.n) || (to.kind === 'pct' && to.f < from.f))) to = null;
    ended = false;
    locationsReady = false;
    lastLocation = null;
    container.hidden = false;
    ui.end.hidden = true;
    ui.prev.disabled = ui.next.disabled = false;
    ui.title.textContent = titleOf(it);
    ui.readout.textContent = passage ? 'in passage' : '';
    ui.msg.textContent = 'Opening…';
    ui.toc.hidden = true;
    renderTOC();

    if (typeof root.ePub !== 'function') { fail(MISSING); return; }
    try {
      compareCFI = (a, b) => new root.ePub.CFI().compare(a, b);
      book = root.ePub('/api/items/' + it.id + '/book', { openAs: 'epub' });
      rendition = book.renderTo(ui.view, { width: '100%', height: '100%', flow: 'paginated', spread: 'none' });
      // A paper page in a dark UI.
      rendition.themes.default({ body: { background: '#f4f1ea', color: '#1c1c1c' } });
      rendition.on('relocated', loc => { if (seq === openSeq) onRelocated(loc); });
      rendition.on('displayError', err => { if (seq === openSeq) ui.msg.textContent = 'Could not display: ' + err; });
      // Tap the left/right edge of the page to turn it (links still work).
      rendition.on('click', e => {
        if (seq !== openSeq || (e.target && e.target.closest && e.target.closest('a'))) return;
        const w = ui.view.clientWidth || 1, x = e.clientX;
        if (x < w * 0.25) prev(); else if (x > w * 0.75) next();
      });
      await book.ready;
      if (seq !== openSeq) return;
      const target = passage ? await targetFor(from) : await savedTarget();
      if (seq !== openSeq) return;
      await rendition.display(target);
      if (seq !== openSeq) return;
      ui.msg.textContent = '';
      // The book's own percentage needs epub.js locations; generate them in
      // the background (cached per item+etag in this browser) and refresh
      // the readout once they exist. Until then the fraction is approximated
      // from the spine position.
      ensureLocations().then(ok => {
        if (ok && seq === openSeq && lastLocation) onRelocated(lastLocation);
      });
    } catch (e) {
      if (seq === openSeq) fail('Could not open the book: ' + (e && e.message || e));
    }
  }

  function teardown() {
    flushPlace();
    if (rendition) { try { rendition.destroy(); } catch (e) { /* already gone */ } }
    if (book) { try { book.destroy(); } catch (e) { /* already gone */ } }
    rendition = book = null;
    if (ui) ui.view.innerHTML = '';
  }

  function close() {
    if (!ui) return;
    teardown();
    container.hidden = true;
    item = null;
    passage = from = to = null;
    ended = false;
    lastLocation = null;
  }

  function isOpen(id) {
    return !!(ui && item && !container.hidden && (id == null || item.id === id));
  }

  // --- locations and the fraction --------------------------------------------

  function locationsKey() { return 'epub-locations:' + item.id + ':' + (item.etag || ''); }

  async function ensureLocations() {
    if (locationsReady) return true;
    if (!book) return false;
    const key = locationsKey();
    try {
      const cached = localStorage.getItem(key);
      if (cached) { book.locations.load(cached); locationsReady = true; return true; }
    } catch (e) { /* storage unavailable: generate every time */ }
    try {
      await book.locations.generate(LOCATION_CHARS);
      locationsReady = true;
      try { localStorage.setItem(key, book.locations.save()); } catch (e) { /* quota: fine, regenerate next time */ }
      return true;
    } catch (e) {
      return false;
    }
  }

  // Where the reader is, in the shape textPassageEnded and the progress
  // report want: the page's CFI, its 1-based section, the fraction and
  // whether this is the book's last page.
  function placeOf(loc) {
    const s = (loc && loc.start) || {};
    const n = sectionCount();
    let section = typeof s.index === 'number' ? s.index + 1 : (root.sectionFromCFI(s.cfi) || 0);
    let pct = null;
    if (locationsReady && s.cfi) {
      try { pct = book.locations.percentageFromCfi(s.cfi); } catch (e) { pct = null; }
    }
    if (typeof pct !== 'number' || !Number.isFinite(pct)) {
      // Spine approximation: sections done, plus the page's share of this one.
      const d = s.displayed || {};
      const within = d.total > 0 ? (Math.max(1, d.page) - 1) / d.total : 0;
      pct = n > 0 && section > 0 ? ((section - 1) + within) / n : 0;
    }
    return { cfi: s.cfi || '', section, pct: clamp01(pct), atEnd: !!(loc && loc.atEnd) };
  }

  // "ch. 7 · 34%", the same words the server puts in progress_text.
  function readout(place) {
    const pct = Math.round(place.pct * 100) + '%';
    const text = place.section > 0 ? 'ch. ' + place.section + ' · ' + pct : pct;
    return passage ? text + ' · in passage' : text;
  }

  function onRelocated(loc) {
    lastLocation = loc;
    const place = placeOf(loc);
    ui.readout.textContent = readout(place);
    markTOC(place.section);
    if (passage) {
      if (!ended && root.textPassageEnded(to, place, compareCFI)) endPassage();
      return; // a passage reports no place
    }
    schedulePlace(place);
  }

  // --- the position report ----------------------------------------------------

  function schedulePlace(place) {
    pendingPlace = { locator: place.cfi, fraction: Math.round(place.pct * 10000) / 10000, section: place.section };
    if (placeTimer) clearTimeout(placeTimer);
    placeTimer = setTimeout(flushPlace, PLACE_DELAY_MS);
  }
  function flushPlace() {
    if (placeTimer) { clearTimeout(placeTimer); placeTimer = null; }
    const place = pendingPlace;
    pendingPlace = null;
    if (place && !passage && opts && opts.onPlace) opts.onPlace(place);
  }

  // --- locators against the open book ----------------------------------------

  // A display target for a locator: a CFI, a spine href, or undefined (the
  // beginning). pct wants the book's locations; when they cannot be made the
  // proportional section stands in.
  async function targetFor(loc) {
    if (!loc || !book) return undefined;
    if (loc.kind === 'cfi') return loc.cfi;
    if (loc.kind === 'ch') return hrefOfSection(loc.n);
    if (loc.kind === 'pct') {
      // The book's own percentage wants its locations, which take a moment
      // to generate on the first open: say so rather than sit on "Opening…".
      if (!locationsReady) ui.msg.textContent = 'Finding ' + Math.round(loc.f * 100) + '% of the book…';
      if (await ensureLocations()) {
        try { return book.locations.cfiFromPercentage(loc.f); } catch (e) { /* fall through */ }
      }
      return hrefOfSection(root.locatorSection(loc, sectionCount()));
    }
    return undefined;
  }
  function hrefOfSection(n) {
    const count = sectionCount();
    if (!(n > 0) || !count) return undefined;
    const sec = book.spine.get(Math.min(n, count) - 1);
    return sec ? sec.href : undefined;
  }

  // Normal reading resumes from the saved locator (a CFI) when there is one.
  async function savedTarget() {
    try {
      const r = await fetch('/api/progress?item_id=' + item.id + '&client_id=' + encodeURIComponent(opts.clientId || 'anonymous'));
      const j = await r.json();
      const loc = root.parseLocator(j && j.locator);
      return loc && loc.kind === 'cfi' ? loc.cfi : undefined;
    } catch (e) {
      return undefined;
    }
  }

  // --- paging -----------------------------------------------------------------

  function prev() { if (rendition) rendition.prev(); }
  function next() {
    if (!rendition) return;
    if (ended) return; // the passage is over: Keep reading or Back
    // A ch bound is inclusive: on the last page of section n the passage is
    // over without turning into n+1.
    if (passage && to && to.kind === 'ch' && lastLocation) {
      const e = lastLocation.end || lastLocation.start || {};
      const d = e.displayed || {};
      if (typeof e.index === 'number' && e.index + 1 === to.n && d.total > 0 && d.page >= d.total) { endPassage(); return; }
    }
    rendition.next();
  }
  function goSection(n) {
    const href = hrefOfSection(n);
    if (href && rendition) rendition.display(href);
  }

  // --- passage end ------------------------------------------------------------

  function endPassage() {
    ended = true;
    ui.next.disabled = true;
    ui.endSub.textContent = !to ? 'The book has ended'
      : to.kind === 'ch' ? 'Through section ' + to.n
      : to.kind === 'pct' ? 'At ' + Math.round(to.f * 100) + '%'
      : 'At the marked place';
    ui.end.hidden = false;
  }
  // "Keep reading": leave passage mode — the bound is cleared, normal reading
  // (progress reports included) resumes from here. index.html drops the
  // passage from the route and opens the gate; then the current place counts.
  function keepReading() {
    passage = from = to = null;
    ended = false;
    ui.end.hidden = true;
    ui.next.disabled = false;
    if (opts && opts.onKeep) opts.onKeep();
    if (lastLocation) onRelocated(lastLocation);
  }

  root.Reader = { open, close, isOpen, passage: () => passage };
})(typeof window !== 'undefined' ? window : this);
