// flickr — the reader pane: EPUB books on epub.js (vendored under web/vendor,
// so a LAN-only engine loads nothing from the internet at read time).
//
// A book is read, not played, but it is read through a SESSION, the way a
// film is played through one: POST /api/items/{id}/read answers the session
// document and THAT is what this pane is handed. Everything it used to work
// out or guess is on it — `links.book` for the bytes, `sections` for the
// contents, `locator` for where the reader left off, `passage` for the bounds
// (already resolved to section indexes), `actions.progress` for the place and
// `actions.keep_reading` for the way out. This file composes no URL at all.
//
// index.html hooks in through four calls:
//   Reader.open(session, opts)  Read pressed, a continue-reading tap, or a
//                               from/to route arriving
//   Reader.close()              the route left the item (closePlayer)
//   Reader.isOpen(id)           is this item open in the pane?
//   Reader.passage()            the passage the session was opened under
// Everything else — the book, the TOC, paging, the readout — lives here.
// Reader is a facade over two panes sharing one container: this file's EPUB
// pane, and the PDF pane in pdfreader.js, which the session's `format` sends
// a PDF to (see the bottom of this file). Both keep the contract above.
//
// The place is posted to `actions.progress.href`, and a 409 there is the
// server saying a passage is on — an answer, not an error: the pane stops
// reporting until the passage is left. In passage mode it does not report at
// all, so the rule holds on both sides of the wire.
//
// Locators (parseLocator etc.) and the end predicate are pure functions in
// passage.js; this file resolves them against the open book. Which SECTION a
// locator lands in is the session's answer now, not this file's.
(function (root) {
  'use strict';

// --- the page's theme: paper or dark ----------------------------------------
// One choice for both readers, remembered per browser (localStorage
// readerTheme); the first visit follows the app, which is dark. epub.js
// takes it as a named theme inside the book's frame; the PDF pane takes
// it as a class on the container and inverts its canvas in CSS.
if (!root.ReaderTheme) root.ReaderTheme = (() => {
  // The page's rules for the book's frame. `images` is the BOOK's choice,
  // read off the session document's `display`: 'printed' leaves pictures
  // as printed; 'themed' recolours them with the dark page (a diagram
  // reads on dark; a photo becomes a negative — hence per book).
  function rules(theme, images) {
    if (theme !== 'dark') return { body: { background: '#f4f1ea', color: '#1c1c1c' } };
    const r = {
      body: { background: '#1b1d24', color: '#d6d6d0' },
      'p, div, span, li, td, th, h1, h2, h3, h4, h5, h6, blockquote, dt, dd': { color: '#d6d6d0 !important', 'background-color': 'transparent !important' },
      a: { color: '#8ab4f8 !important' },
    };
    r['img, svg, image'] = images === 'themed'
      ? { filter: 'invert(.91) hue-rotate(180deg)' }
      : { filter: 'none' };
    return r;
  }
  function current() {
    let t = null;
    try { t = localStorage.readerTheme; } catch (e) { /* no storage */ }
    return t === 'paper' || t === 'dark' ? t : 'dark';
  }
  function toggle() {
    const t = current() === 'dark' ? 'paper' : 'dark';
    try { localStorage.readerTheme = t; } catch (e) { /* no storage */ }
    return t;
  }
  // Classes on the pane: rd-dark for the page, rd-printed when the book's
  // pictures stay as printed (the PDF pane's canvas reads it).
  function apply(c, images) {
    if (!c) return;
    c.classList.toggle('rd-dark', current() === 'dark');
    c.classList.toggle('rd-printed', images === 'printed');
  }
  // One stylesheet, by id, in every section of the open book: removed and
  // re-added on a change, so a toggle never leaves an older rule behind.
  function inject(contents, theme, images) {
    try {
      const old = contents.document.getElementById('flickr-page');
      if (old) old.remove();
      contents.addStylesheetRules(rules(theme, images), 'flickr-page');
    } catch (e) { /* a section not yet attached */ }
  }
  return { rules, current, toggle, apply, inject };
})();

  const MISSING = 'The reader library is missing: web/vendor/epub.min.js and web/vendor/jszip.min.js ' +
                  'are not vendored (see web/vendor/README.md).';
  const PLACE_DELAY_MS = 800;   // debounce for position reports (a page turn per second is bursty)
  const LOCATION_CHARS = 1024;  // epub.js location granularity (chars per location)

  let container = null, ui = null, keysBound = false;
  let sess = null, opts = null, book = null, rendition = null, compareCFI = null;
  let passage = null;           // the session's passage; null = ordinary reading
  let from = null, to = null;   // its bounds, parsed
  let ended = false;            // the end bound has been reached: no more advancing
  let refused = false;          // the server answered 409: a passage is on after all
  let locationsReady = false;   // book.locations generated or loaded: fractions are the book's own
  let openSeq = 0;              // stale async work from an earlier open checks this
  let placeTimer = null, pendingPlace = null;
  let lastLocation = null;

  // --- the pane ---------------------------------------------------------------

  function build(c) {
    // Built once per container — unless the PDF pane has since taken the
    // container over, in which case this pane's elements are detached and
    // are built anew.
    if (ui && container === c && ui.title.isConnected) return;
    container = c;
    c.innerHTML =
      '<div class="rd-bar">' +
        '<button class="rd-toc-btn" title="Contents">☰</button>' +
        '<div class="rd-title"></div>' +
        '<div class="rd-readout"></div>' +
        '<button class="rd-theme" title="Paper or dark page">◐</button>' +
        '<button class="rd-pictures" title="This book\'s pictures on the dark page: as printed, or recoloured with the page">🖼</button>' +
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
      theme: q('.rd-theme'), pictures: q('.rd-pictures'),
      title: q('.rd-title'), readout: q('.rd-readout'), toc: q('.rd-toc'), view: q('.rd-view'),
      msg: q('.rd-msg'), end: q('.rd-end'), endSub: q('.rd-end-sub'),
      prev: q('.rd-prev'), next: q('.rd-next'), tocBtn: q('.rd-toc-btn'), close: q('.rd-close'),
      keep: q('.rd-keep'), back: q('.rd-back'),
    };
    ui.prev.onclick = prev;
    ui.next.onclick = next;
    ui.tocBtn.onclick = () => { ui.toc.hidden = !ui.toc.hidden; };
    ui.theme.onclick = () => { ReaderTheme.toggle(); applyPage(); };
    ui.pictures.onclick = setPictures;
    ui.close.onclick = () => opts && opts.onBack && opts.onBack();
    ui.back.onclick = () => opts && opts.onBack && opts.onBack();
    ui.keep.onclick = keepReading;
    if (!keysBound) { document.addEventListener('keydown', onKey); keysBound = true; }
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
  // One reach into the document, so a relation that is not there reads as
  // absent rather than throwing halfway down a chain.
  function hrefOf(group, name) {
    const entry = sess && sess[group] && sess[group][name];
    return entry && entry.href ? entry.href : null;
  }
  function sections() { return (sess && sess.sections) || []; }
  function sectionCount() {
    if (book && book.spine && book.spine.length > 0) return book.spine.length;
    return sections().length;
  }
  function clamp01(f) { return !(f > 0) ? 0 : f > 1 ? 1 : f; }

  // The contents are the session's `sections` — one entry per spine item, in
  // order, titled from the book's own nav/NCX by the prober — so an entry's
  // `index` is the 1-based section and a tap needs no lookup in the book.
  function renderTOC() {
    const secs = sections();
    ui.toc.innerHTML = '';
    if (!secs.length) { ui.toc.innerHTML = '<div class="rd-toc-empty">No contents listed.</div>'; return; }
    for (const s of secs) {
      const b = document.createElement('button');
      b.dataset.section = String(s.index);
      b.textContent = s.title || ('Section ' + s.index);
      b.onclick = () => { goSection(s.index); ui.toc.hidden = true; };
      ui.toc.appendChild(b);
    }
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

  // s is the reading session document; o is { container, onKeep(session), onBack() }
  async function open(s, o) {
    const seq = ++openSeq;
    teardown();
    sess = s;
    opts = o || {};
    build(opts.container);
    // The passage came resolved: from/to as locators, and the sections they
    // land in. A `to` before `from` was dropped by the server, not here.
    passage = root.isTextPassage(sess.passage) ? sess.passage : null;
    from = passage ? root.parseLocator(passage.from) : null;
    to = passage ? root.parseLocator(passage.to) : null;
    ended = false;
    refused = false;
    locationsReady = false;
    lastLocation = null;
    container.hidden = false;
    ui.end.hidden = true;
    ui.prev.disabled = ui.next.disabled = false;
    ui.title.textContent = sess.title || '';
    ui.readout.textContent = passage ? 'in passage' : '';
    ui.msg.textContent = 'Opening…';
    ui.toc.hidden = true;
    renderTOC();

    const bytes = hrefOf('links', 'book');
    if (!bytes) { fail('This reading session names no book to open.'); return; }
    if (typeof root.ePub !== 'function') { fail(MISSING); return; }
    try {
      compareCFI = (a, b) => new root.ePub.CFI().compare(a, b);
      book = root.ePub(bytes, { openAs: 'epub' });
      rendition = book.renderTo(ui.view, { width: '100%', height: '100%', flow: 'paginated', spread: 'none' });
      // The page: paper, or dark for the dark UI, with the book's own
      // choice about its pictures (session.display). Injected as one
      // stylesheet into every section as it loads, and re-injected on a
      // toggle; the book's own colours yield to the page (!important),
      // because a publisher's black-on-white paragraph rule would
      // otherwise survive the swap.
      rendition.hooks.content.register(c => ReaderTheme.inject(c, ReaderTheme.current(), images()));
      applyPage();
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
      const target = passage ? await targetFor(from) : savedTarget();
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
    sess = null;
    passage = from = to = null;
    ended = refused = false;
    lastLocation = null;
  }

  function isOpen(id) {
    return !!(ui && sess && !container.hidden && ui.title.isConnected && (id == null || sess.item_id === id));
  }

  // --- locations and the fraction --------------------------------------------

  function locationsKey() { return 'epub-locations:' + sess.item_id + ':' + (sess.etag || ''); }

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
    if (place && !passage && !refused) postPlace(place);
  }
  // The place goes to the session's own progress action — the reader knows
  // no addresses of its own. A 409 is the server saying a passage is on: an
  // answer, not an error, and the answer is to stop reporting until it is
  // left. Anything else is the network, which is not the reader's business.
  function postPlace(place) {
    const act = sess && sess.actions && sess.actions.progress;
    if (!act || !act.href) return;
    fetch(act.href, {
      method: act.method || 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(place),
    }).then(r => { if (r.status === 409) refused = true; }).catch(() => {});
  }

  // --- locators against the open book ----------------------------------------

  // A display target for a locator: a CFI, a spine href, or undefined (the
  // beginning). pct wants the book's locations; when they cannot be made,
  // the section the SESSION says it lands in stands in.
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
      return hrefOfSection(passage && passage.from_section);
    }
    return undefined;
  }
  function hrefOfSection(n) {
    const count = sectionCount();
    if (!(n > 0) || !count) return undefined;
    const sec = book.spine.get(Math.min(n, count) - 1);
    return sec ? sec.href : undefined;
  }

  // Ordinary reading resumes from the saved locator, which came WITH the
  // session: the reader asks no second address where the place is.
  function savedTarget() {
    const loc = root.parseLocator(sess.locator && sess.locator.cfi);
    return loc && loc.kind === 'cfi' ? loc.cfi : undefined;
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

  // The two ways out are the session document's: the keep_reading action,
  // labelled as the server labels it, and the back link, which names what
  // going back goes back to.
  function endPassage() {
    ended = true;
    ui.next.disabled = true;
    ui.endSub.textContent = !to ? 'The book has ended'
      : to.kind === 'ch' ? 'Through section ' + to.n
      : to.kind === 'pct' ? 'At ' + Math.round(to.f * 100) + '%'
      : 'At the marked place';
    const keep = sess && sess.actions && sess.actions.keep_reading;
    ui.keep.textContent = (keep && keep.label) || 'Keep reading';
    const back = sess && sess.links && sess.links.back;
    ui.back.title = back && back.title ? 'Back to ' + back.title : '';
    ui.end.hidden = false;
  }
  // "Keep reading": leave the passage. It is the SESSION's passage, so
  // leaving it is a write — the server clears it and answers the session as
  // it now is, progress accepted from here on. index.html drops the passage
  // from the route in onKeep; then the current place counts.
  // The book's choice about its pictures, off the session document.
  function images() { return (sess && sess.display && sess.display.images) || 'printed'; }

  // Re-read the page's rules into the open book and the pane.
  function applyPage() {
    ReaderTheme.apply(container, images());
    if (rendition) rendition.getContents().forEach(c => ReaderTheme.inject(c, ReaderTheme.current(), images()));
    if (ui && ui.pictures) {
      const printed = images() === 'printed';
      ui.pictures.textContent = printed ? '🖼' : '🎨';
      ui.pictures.title = printed
        ? "This book's pictures are shown as printed — tap to recolour them with the dark page"
        : "This book's pictures are recoloured with the dark page — tap to show them as printed";
      ui.pictures.hidden = !(sess && sess.actions && sess.actions.set_display);
    }
  }

  // The choice is the BOOK's, kept by the server for every profile and
  // device: post the action, take the answered session, redraw.
  async function setPictures() {
    const act = sess && sess.actions && sess.actions.set_display;
    if (!act) return;
    const next = images() === 'printed' ? 'themed' : 'printed';
    try {
      const r = await fetch(act.href, {
        method: act.method || 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ images: next }),
      });
      if (r.ok) sess = await r.json();
    } catch (e) { /* the pane keeps the document it has */ }
    applyPage();
  }

  async function keepReading() {
    const act = sess && sess.actions && sess.actions.keep_reading;
    if (act) {
      try {
        const r = await fetch(act.href, { method: act.method || 'POST' });
        if (r.ok) sess = await r.json();
      } catch (e) { /* the session may already be over; the pane leaves the passage either way */ }
    }
    passage = from = to = null;
    ended = refused = false;
    ui.end.hidden = true;
    ui.next.disabled = false;
    if (opts && opts.onKeep) opts.onKeep(sess);
    if (lastLocation) onRelocated(lastLocation);
  }

  // --- the facade -------------------------------------------------------------
  // A PDF is read page by page in the PDF pane (pdfreader.js); every other
  // book is an EPUB and opens here. WHICH is the session document's answer
  // (`format`), not a guess at the file name. The two panes share the
  // container, so opening one closes the other, and index.html asks the
  // facade, never a pane. A PDF with no PDF pane loaded gets one sentence,
  // not an epub.js error about a zip.
  function isPDF(s) { return !!s && s.format === 'pdf'; }
  function pdfPane() { return root.PDFReader || null; }
  root.Reader = {
    open(s, o) {
      if (!isPDF(s)) {
        if (pdfPane()) pdfPane().close();
        return open(s, o);
      }
      close();
      if (pdfPane()) return pdfPane().open(s, o);
      sess = s;
      opts = o || {};
      build(opts.container);
      container.hidden = false;
      ui.title.textContent = (s && s.title) || '';
      fail('The PDF pane is missing: web/pdfreader.js did not load.');
      return undefined;
    },
    close() { close(); if (pdfPane()) pdfPane().close(); },
    isOpen(id) { return isOpen(id) || !!(pdfPane() && pdfPane().isOpen(id)); },
    passage() { return pdfPane() && pdfPane().isOpen() ? pdfPane().passage() : passage; },
  };
})(typeof window !== 'undefined' ? window : this);
