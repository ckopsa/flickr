// flickr — the PDF pane: one page at a time on pdf.js (vendored under
// web/vendor as an ES module that leaves `pdfjsLib` on window, its worker
// loaded from the same directory), drawn to a canvas fit to the pane's
// width, with Prev / Next, a page field and the keyboard's arrows.
//
// It keeps the EPUB pane's contract exactly (see reader.js, whose Reader
// facade hands a session whose `format` is "pdf" here): open / close /
// isOpen / passage, all four taking the READING SESSION document. The bytes
// are `links.book`, the count is `page_count`, the resume page is `locator`,
// the bounds are `passage` (already resolved to pages) and the place is
// posted to `actions.progress.href` — where a 409 is the server saying a
// passage is on. In passage mode this file reports nothing at all: two locks
// on the same door, as in reader.js. A PDF's place is a page: the server
// stores { page, fraction } in the same locator column an EPUB's CFI goes
// in, and reads it back as `p. 213 / 400`.
//
// Locators (parseLocator etc.) and the end predicate (textPassageEnded)
// are the pure functions in passage.js; pg:<n> is the spelling a page
// passage uses, pct:<f> lands on the proportional page, and the EPUB
// spellings (ch:, cfi:) mean nothing here and are ignored.
(function (root) {
  'use strict';

  const MISSING = 'The PDF library is missing: web/vendor/pdf.min.mjs and web/vendor/pdf.worker.min.mjs ' +
                  'are not vendored (see web/vendor/README.md).';
  const WORKER_SRC = '/vendor/pdf.worker.min.mjs';
  const PLACE_DELAY_MS = 800;  // debounce for position reports (a page a second is bursty)
  const RESIZE_DELAY_MS = 200; // re-fit after the window settles

  let container = null, ui = null, keysBound = false;
  let sess = null, opts = null, doc = null, count = 0, page = 0;
  let passage = null;          // the session's passage; null = ordinary reading
  let from = null, to = null;  // its bounds, parsed
  let ended = false;           // the end bound has been reached: no more turning
  let refused = false;         // the server answered 409: a passage is on after all
  let openSeq = 0, drawSeq = 0; // stale async work from an earlier open / draw checks these
  let renderTask = null;       // the pdf.js render in flight, cancelled by the next one
  let placeTimer = null, pendingPlace = null, resizeTimer = null;

  // --- the pane ---------------------------------------------------------------

  function build(c) {
    // Built once per container — unless the EPUB pane has since taken the
    // container over, in which case this pane's elements are detached and
    // are built anew.
    if (ui && container === c && ui.title.isConnected) return;
    container = c;
    c.innerHTML =
      '<div class="rd-bar">' +
        '<div class="rd-title"></div>' +
        '<div class="rd-readout"></div>' +
        '<button class="rd-theme" title="Paper or dark page">◐</button>' +
        '<button class="rd-pictures" title="This book on the dark page: inverted with the page, or as printed">🖼</button>' +
        '<button class="rd-close" title="Close the book">✕ Back</button>' +
      '</div>' +
      '<div class="rd-body">' +
        '<div class="rd-view rd-pdf"><canvas class="rd-canvas"></canvas></div>' +
      '</div>' +
      '<div class="rd-nav">' +
        '<button class="rd-prev" title="Previous page (←)">‹ Prev</button>' +
        '<div class="rd-msg"></div>' +
        '<label class="rd-page">p. <input class="rd-page-in" type="number" min="1" step="1" inputmode="numeric" title="Go to page"> ' +
          '<span class="rd-page-of"></span></label>' +
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
      title: q('.rd-title'), readout: q('.rd-readout'), view: q('.rd-view'), canvas: q('.rd-canvas'),
      msg: q('.rd-msg'), end: q('.rd-end'), endSub: q('.rd-end-sub'),
      prev: q('.rd-prev'), next: q('.rd-next'), pageIn: q('.rd-page-in'), pageOf: q('.rd-page-of'),
      close: q('.rd-close'), keep: q('.rd-keep'), back: q('.rd-back'),
    };
    ui.prev.onclick = prev;
    ui.next.onclick = next;
    ui.theme.onclick = () => { ReaderTheme.toggle(); applyPage(); };
    ui.pictures.onclick = setPictures;
    applyPage();
    ui.close.onclick = () => opts && opts.onBack && opts.onBack();
    ui.back.onclick = () => opts && opts.onBack && opts.onBack();
    ui.keep.onclick = keepReading;
    // The page field goes on Enter or when it loses focus; a number the
    // book does not have is clamped, nonsense restores the current page.
    ui.pageIn.onchange = () => {
      const n = Number(ui.pageIn.value);
      if (Number.isInteger(n) && n > 0) goTo(n); else ui.pageIn.value = String(page || '');
    };
    ui.pageIn.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); ui.pageIn.blur(); } };
    // Tap the left/right edge of the page to turn it.
    ui.view.onclick = e => {
      const w = ui.view.clientWidth || 1, x = e.clientX - ui.view.getBoundingClientRect().left;
      if (x < w * 0.25) prev(); else if (x > w * 0.75) next();
    };
    if (!keysBound) {
      document.addEventListener('keydown', onKey);
      window.addEventListener('resize', onResize);
      keysBound = true;
    }
  }

  function onKey(e) {
    if (!isOpen() || e.defaultPrevented) return;
    const t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'SELECT' || t.tagName === 'TEXTAREA')) return;
    if (e.key === 'ArrowLeft') { prev(); e.preventDefault(); }
    else if (e.key === 'ArrowRight') { next(); e.preventDefault(); }
  }
  function onResize() {
    if (resizeTimer) clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => { resizeTimer = null; if (isOpen() && doc && page > 0) draw(page, false); }, RESIZE_DELAY_MS);
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, ch =>
      ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
  }
  function clamp01(f) { return !(f > 0) ? 0 : f > 1 ? 1 : f; }

  function fail(msg) {
    ui.msg.textContent = msg;
    ui.view.innerHTML = '<div class="rd-fail">' + esc(msg) + '</div>';
    ui.canvas = null;
    ui.prev.disabled = ui.next.disabled = ui.pageIn.disabled = true;
  }

  // --- opening ----------------------------------------------------------------

  // s is the reading session document; o is { container, onKeep(session), onBack() }
  async function open(s, o) {
    const seq = ++openSeq;
    teardown();
    sess = s;
    applyPage();
    opts = o || {};
    build(opts.container);
    if (!ui.canvas) { ui.view.innerHTML = '<canvas class="rd-canvas"></canvas>'; ui.canvas = ui.view.firstChild; }
    // The passage came resolved: from/to as locators, and the pages they
    // land on. A `to` before `from` was dropped by the server, not here.
    passage = root.isTextPassage(sess.passage) ? sess.passage : null;
    from = passage ? root.parseLocator(passage.from) : null;
    to = passage ? root.parseLocator(passage.to) : null;
    ended = false;
    refused = false;
    page = 0;
    count = sess.page_count || 0; // the prober's count until the file says
    container.hidden = false;
    ui.end.hidden = true;
    ui.prev.disabled = ui.next.disabled = ui.pageIn.disabled = false;
    ui.title.textContent = sess.title || '';
    ui.readout.textContent = passage ? 'in passage' : '';
    ui.msg.textContent = 'Opening…';
    ui.pageIn.value = '';
    ui.pageOf.textContent = count ? '/ ' + count : '';

    const bytes = (sess.links && sess.links.book && sess.links.book.href) || '';
    if (!bytes) { fail('This reading session names no book to open.'); return; }
    const lib = root.pdfjsLib;
    if (!lib || typeof lib.getDocument !== 'function') { fail(MISSING); return; }
    try {
      if (lib.GlobalWorkerOptions && !lib.GlobalWorkerOptions.workerSrc) lib.GlobalWorkerOptions.workerSrc = WORKER_SRC;
      // The book answers Range requests, so pdf.js reads the cross-reference
      // first and then only the objects of the page shown.
      const task = lib.getDocument({ url: bytes });
      const d = await task.promise;
      if (seq !== openSeq) { try { d.destroy(); } catch (e) { /* already gone */ } return; }
      doc = d;
      count = d.numPages || count;
      ui.pageOf.textContent = count ? '/ ' + count : '';
      ui.pageIn.max = String(count || 1);
      const start = passage ? pageFor(from) : savedPage();
      if (seq !== openSeq) return;
      ui.msg.textContent = '';
      await goTo(start || 1);
    } catch (e) {
      if (seq === openSeq) fail('Could not open the PDF: ' + (e && e.message || e));
    }
  }

  function teardown() {
    flushPlace();
    if (renderTask) { try { renderTask.cancel(); } catch (e) { /* already done */ } renderTask = null; }
    if (doc) { try { doc.destroy(); } catch (e) { /* already gone */ } }
    doc = null;
    if (ui && ui.canvas) {
      const ctx = ui.canvas.getContext('2d');
      if (ctx) ctx.clearRect(0, 0, ui.canvas.width, ui.canvas.height);
    }
  }

  function close() {
    if (!ui) return;
    teardown();
    container.hidden = true;
    sess = null;
    passage = from = to = null;
    ended = refused = false;
    page = count = 0;
  }

  function isOpen(id) {
    return !!(ui && sess && !container.hidden && ui.title.isConnected && (id == null || sess.item_id === id));
  }

  // --- pages ------------------------------------------------------------------

  // The page a locator names. The SESSION worked it out against the probed
  // count (`passage.from_page`); this stands in for the passage's own start
  // when the file would not say how many pages it has, and for a page the
  // reader is sent to by anything but the passage. pg is itself, pct lands
  // proportionally (0 on the first page, 1 on the last), the EPUB spellings
  // name nothing here. Clamped into the book; null when it says nothing.
  function pageFor(loc) {
    if (loc === from && passage && passage.from_page > 0) return clampPage(passage.from_page);
    if (!loc) return null;
    let n = null;
    if (loc.kind === 'pg') n = loc.n;
    else if (loc.kind === 'pct' && count > 0) n = Math.floor(loc.f * count) + 1;
    return n == null ? null : clampPage(n);
  }
  function clampPage(n) { return count > 0 ? Math.min(count, Math.max(1, n)) : Math.max(1, n); }

  // Ordinary reading resumes from the saved page, which came WITH the
  // session (a row an EPUB reader wrote for this item carries no page and
  // starts over).
  function savedPage() {
    const loc = sess && sess.locator;
    return loc && Number.isInteger(loc.page) && loc.page > 0 ? loc.page : null;
  }

  // goTo shows page n (clamped) and reports it; draw alone re-fits the page
  // already shown (a resize) without reporting again.
  async function goTo(n) {
    if (!doc) return;
    n = Math.floor(Number(n));
    if (!Number.isFinite(n)) return;
    n = count > 0 ? Math.min(count, Math.max(1, n)) : Math.max(1, n);
    if (await draw(n, true)) { page = n; onPage(); }
  }

  async function draw(n, announce) {
    const seq = ++drawSeq;
    if (renderTask) { try { renderTask.cancel(); } catch (e) { /* already done */ } renderTask = null; }
    try {
      const pg = await doc.getPage(n);
      if (seq !== drawSeq || !ui.canvas) return false;
      // Fit to width: the page's own width at scale 1 against the pane's,
      // drawn at the device's pixel ratio so text stays crisp on a phone.
      const base = pg.getViewport({ scale: 1 });
      const width = Math.max(1, ui.view.clientWidth - 12);
      const scale = width / base.width;
      const dpr = window.devicePixelRatio || 1;
      const vp = pg.getViewport({ scale });
      const canvas = ui.canvas, ctx = canvas.getContext('2d');
      canvas.width = Math.floor(vp.width * dpr);
      canvas.height = Math.floor(vp.height * dpr);
      canvas.style.width = Math.floor(vp.width) + 'px';
      canvas.style.height = Math.floor(vp.height) + 'px';
      const task = pg.render({ canvasContext: ctx, viewport: vp, transform: dpr !== 1 ? [dpr, 0, 0, dpr, 0, 0] : null });
      renderTask = task;
      await task.promise;
      if (renderTask === task) renderTask = null;
      if (seq !== drawSeq) return false;
      ui.view.scrollTop = 0;
      return true;
    } catch (e) {
      // A cancelled render is the next page arriving, not a failure.
      if (seq === drawSeq && !(e && e.name === 'RenderingCancelledException') && announce) {
        ui.msg.textContent = 'Could not draw page ' + n + ': ' + (e && e.message || e);
      }
      return false;
    }
  }

  // Where the reader is, in the shape textPassageEnded and the progress
  // report want: the page, the fraction (page over count) and whether this
  // is the last page. No section and no CFI: a PDF has neither.
  function placeOf() {
    return { page, pct: count > 0 ? clamp01(page / count) : 0, section: 0, cfi: '', atEnd: count > 0 && page >= count };
  }

  // "p. 213 / 400", the same words the server puts in progress_text.
  function readout(place) {
    const text = 'p. ' + place.page + (count > 0 ? ' / ' + count : '');
    return passage ? text + ' · in passage' : text;
  }

  function onPage() {
    const place = placeOf();
    ui.readout.textContent = readout(place);
    ui.pageIn.value = String(page);
    ui.prev.disabled = page <= 1;
    ui.next.disabled = ended || (count > 0 && page >= count);
    if (passage) {
      if (!ended && root.textPassageEnded(to, place, null)) endPassage();
      return; // a passage reports no place
    }
    schedulePlace(place);
  }

  // --- the position report ----------------------------------------------------

  function schedulePlace(place) {
    pendingPlace = { page: place.page, fraction: Math.round(place.pct * 10000) / 10000 };
    if (placeTimer) clearTimeout(placeTimer);
    placeTimer = setTimeout(flushPlace, PLACE_DELAY_MS);
  }
  function flushPlace() {
    if (placeTimer) { clearTimeout(placeTimer); placeTimer = null; }
    const place = pendingPlace;
    pendingPlace = null;
    if (place && !passage && !refused) postPlace(place);
  }
  // The place goes to the session's own progress action — this pane knows no
  // addresses of its own. A 409 is the server saying a passage is on: an
  // answer, not an error, and the answer is to stop reporting until it is
  // left. Anything else is the network, which is not the reader's business.
  // A PDF is one picture of a page, so the book's choice (session.display)
  // is the whole dark page: 'themed' inverts the canvas, 'printed' keeps
  // the white page under the dark chrome. The choice is the book's, kept
  // by the server; the pane posts the action and takes the answer.
  function images() { return (sess && sess.display && sess.display.images) || 'themed'; }
  function applyPage() {
    ReaderTheme.apply(container, images());
    if (ui && ui.pictures) {
      const printed = images() === 'printed';
      ui.pictures.textContent = printed ? '🖼' : '🎨';
      ui.pictures.title = printed
        ? 'This book is shown as printed — tap to invert it with the dark page'
        : 'This book is inverted with the dark page — tap to show it as printed';
      ui.pictures.hidden = !(sess && sess.actions && sess.actions.set_display);
    }
  }
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

  function postPlace(place) {
    const act = sess && sess.actions && sess.actions.progress;
    if (!act || !act.href) return;
    fetch(act.href, {
      method: act.method || 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(place),
    }).then(r => { if (r.status === 409) refused = true; }).catch(() => {});
  }

  // --- paging -----------------------------------------------------------------

  function prev() { if (doc && page > 1) goTo(page - 1); }
  function next() {
    if (!doc) return;
    if (ended) return; // the passage is over: Keep reading or Back
    if (count > 0 && page >= count) return;
    goTo(page + 1);
  }

  // --- passage end ------------------------------------------------------------

  // The two ways out are the session document's: the keep_reading action,
  // labelled as the server labels it, and the back link, which names what
  // going back goes back to.
  function endPassage() {
    ended = true;
    ui.next.disabled = true;
    ui.endSub.textContent = !to ? 'The book has ended'
      : to.kind === 'pg' ? 'Through page ' + to.n
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
  // from the route in onKeep; then the current page counts.
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
    if (opts && opts.onKeep) opts.onKeep(sess);
    if (page > 0) onPage();
  }

  root.PDFReader = { open, close, isOpen, passage: () => passage };
})(typeof window !== 'undefined' ? window : this);
