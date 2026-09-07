// flickr — the PDF pane: one page at a time on pdf.js (vendored under
// web/vendor as an ES module that leaves `pdfjsLib` on window, its worker
// loaded from the same directory), drawn to a canvas fit to the pane's
// width, with Prev / Next, a page field and the keyboard's arrows.
//
// It keeps the EPUB pane's contract exactly (see reader.js, whose Reader
// facade hands a .pdf item here): open / close / isOpen / passage. The place
// leaves through opts.onPlace({ page, fraction }) — index.html routes that
// into saveProgress(), the one gate for /api/progress, closed while a
// passage is set — and in passage mode this file does not call onPlace at
// all: two locks on the same door, as in reader.js. A PDF's place is a page:
// the server stores { page, fraction } in the same locator column an EPUB's
// CFI goes in, and reads it back as `p. 213 / 400`.
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
  let item = null, opts = null, doc = null, count = 0, page = 0;
  let passage = null;          // the route passage the pane was opened with; null = normal reading
  let from = null, to = null;  // its bounds, parsed
  let ended = false;           // the end bound has been reached: no more turning
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
      title: q('.rd-title'), readout: q('.rd-readout'), view: q('.rd-view'), canvas: q('.rd-canvas'),
      msg: q('.rd-msg'), end: q('.rd-end'), endSub: q('.rd-end-sub'),
      prev: q('.rd-prev'), next: q('.rd-next'), pageIn: q('.rd-page-in'), pageOf: q('.rd-page-of'),
      close: q('.rd-close'), keep: q('.rd-keep'), back: q('.rd-back'),
    };
    ui.prev.onclick = prev;
    ui.next.onclick = next;
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
  function titleOf(it) {
    return (it.enrichment && it.enrichment.title) ||
           (it.media_info && it.media_info.document && it.media_info.document.title) ||
           (it.identity && it.identity.title) || String(it.object_key || '').split('/').pop();
  }
  function clamp01(f) { return !(f > 0) ? 0 : f > 1 ? 1 : f; }

  function fail(msg) {
    ui.msg.textContent = msg;
    ui.view.innerHTML = '<div class="rd-fail">' + esc(msg) + '</div>';
    ui.canvas = null;
    ui.prev.disabled = ui.next.disabled = ui.pageIn.disabled = true;
  }

  // --- opening ----------------------------------------------------------------

  // opts: { container, passage, clientId, onPlace(place), onKeep(), onBack() }
  async function open(it, o) {
    const seq = ++openSeq;
    teardown();
    item = it;
    opts = o || {};
    build(opts.container);
    if (!ui.canvas) { ui.view.innerHTML = '<canvas class="rd-canvas"></canvas>'; ui.canvas = ui.view.firstChild; }
    passage = root.isTextPassage(opts.passage) ? opts.passage : null;
    from = passage ? root.parseLocator(passage.from) : null;
    to = passage ? root.parseLocator(passage.to) : null;
    // A `to` before `from` is no bound, the way an `end` before `t` is
    // dropped — judged only where the two are comparable without the book.
    if (from && to && from.kind === to.kind &&
        ((to.kind === 'pg' && to.n < from.n) || (to.kind === 'pct' && to.f < from.f))) to = null;
    ended = false;
    page = 0;
    count = (it.media_info && it.media_info.page_count) || 0; // the prober's count until the file says
    container.hidden = false;
    ui.end.hidden = true;
    ui.prev.disabled = ui.next.disabled = ui.pageIn.disabled = false;
    ui.title.textContent = titleOf(it);
    ui.readout.textContent = passage ? 'in passage' : '';
    ui.msg.textContent = 'Opening…';
    ui.pageIn.value = '';
    ui.pageOf.textContent = count ? '/ ' + count : '';

    const lib = root.pdfjsLib;
    if (!lib || typeof lib.getDocument !== 'function') { fail(MISSING); return; }
    try {
      if (lib.GlobalWorkerOptions && !lib.GlobalWorkerOptions.workerSrc) lib.GlobalWorkerOptions.workerSrc = WORKER_SRC;
      // /book answers Range requests, so pdf.js reads the cross-reference
      // first and then only the objects of the page shown.
      const task = lib.getDocument({ url: '/api/items/' + it.id + '/book' });
      const d = await task.promise;
      if (seq !== openSeq) { try { d.destroy(); } catch (e) { /* already gone */ } return; }
      doc = d;
      count = d.numPages || count;
      ui.pageOf.textContent = count ? '/ ' + count : '';
      ui.pageIn.max = String(count || 1);
      const start = passage ? pageFor(from) : await savedPage();
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
    item = null;
    passage = from = to = null;
    ended = false;
    page = count = 0;
  }

  function isOpen(id) {
    return !!(ui && item && !container.hidden && ui.title.isConnected && (id == null || item.id === id));
  }

  // --- pages ------------------------------------------------------------------

  // The page a locator names: pg is itself, pct lands proportionally (0 on
  // the first page, 1 on the last), the EPUB spellings name nothing here.
  // Clamped into the book; null when the locator says nothing.
  function pageFor(loc) {
    if (!loc) return null;
    let n = null;
    if (loc.kind === 'pg') n = loc.n;
    else if (loc.kind === 'pct' && count > 0) n = Math.floor(loc.f * count) + 1;
    if (n == null) return null;
    return count > 0 ? Math.min(count, Math.max(1, n)) : Math.max(1, n);
  }

  // Normal reading resumes from the saved page when there is one (a row an
  // EPUB reader wrote for this item carries no page and starts over).
  async function savedPage() {
    try {
      const r = await fetch('/api/progress?item_id=' + item.id + '&client_id=' + encodeURIComponent(opts.clientId || 'anonymous'));
      const j = await r.json();
      return j && Number.isInteger(j.page) && j.page > 0 ? j.page : null;
    } catch (e) {
      return null;
    }
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
    if (place && !passage && opts && opts.onPlace) opts.onPlace(place);
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

  function endPassage() {
    ended = true;
    ui.next.disabled = true;
    ui.endSub.textContent = !to ? 'The book has ended'
      : to.kind === 'pg' ? 'Through page ' + to.n
      : to.kind === 'pct' ? 'At ' + Math.round(to.f * 100) + '%'
      : 'At the marked place';
    ui.end.hidden = false;
  }
  // "Keep reading": leave passage mode — the bound is cleared, normal reading
  // (progress reports included) resumes from here. index.html drops the
  // passage from the route and opens the gate; then the current page counts.
  function keepReading() {
    passage = from = to = null;
    ended = false;
    ui.end.hidden = true;
    if (opts && opts.onKeep) opts.onKeep();
    if (page > 0) onPage();
  }

  root.PDFReader = { open, close, isOpen, passage: () => passage };
})(typeof window !== 'undefined' ? window : this);
