// flickr — passage grammar and end detection.
//
// A PASSAGE is a start and an end within a work: a scene from 1:19:00 to
// 1:24:30, or episodes 5–6. Other systems (the household's day planner)
// mint links to passages, so the grammar is fixed and documented in
// README.md ("Deep links and passages"). The query rides on the hash path:
//
//   #/item/<id>?t=<start_seconds>&end=<end_seconds>
//     &until=<item_id>       episode RUN: auto-advance continues through
//                            episodes and stops after the item with that id
//                            (`end` then applies within that last item)
//     ?from=<locator>&to=<locator>
//                            a TEXT passage: the reader (reader.js, or
//                            pdfreader.js for a PDF) opens at from and stops
//                            at to. A locator is cfi:<epub cfi>, ch:<n>
//                            (1-based spine section), pct:<0..1> or pg:<n>
//                            (a PDF's 1-based page); the grammar and the end
//                            predicate are the text-locator functions below
//   #/show/<title>?ep=S02E05&t=…&end=…[&until=S02E07]
//                            the same passage addressed by show and episode
//                            code, for a minter that knows no item ids. The
//                            SERVER resolves it (internal/passage, answered
//                            by GET /api/-/route) and the client is handed
//                            the item form
//
// Seconds may be integers or decimals. The player also MAKES passages (mark
// in, mark out, copy the link — the second half of this file); the link it
// mints is exactly this grammar. Everything in this file is pure: it
// is loaded by index.html as a plain script (globals) and by
// web/passage_test.mjs through the CommonJS export at the bottom, so the
// grammar has tests without a build step.
(function (root) {
  'use strict';

  // '#/item/51?t=4740&end=5070' → { path: '#/item/51', query: 't=4740&end=5070' }
  function splitHash(hash) {
    const h = String(hash || '');
    const i = h.indexOf('?');
    return i < 0 ? { path: h, query: '' } : { path: h.slice(0, i), query: h.slice(i + 1) };
  }

  // Non-negative seconds, integer or decimal ('4740', '79.5'); anything else
  // is treated as absent rather than guessed at.
  function parseSeconds(v) {
    if (v == null || !/^\d+(\.\d+)?$/.test(v)) return null;
    const n = Number(v);
    return Number.isFinite(n) ? n : null;
  }
  function parseItemId(v) {
    return v != null && /^\d+$/.test(v) ? Number(v) : null;
  }
  function nonEmpty(v) {
    return v == null || v === '' ? null : v;
  }

  // parsePassage takes the hash's query part (with or without the leading
  // '?') and returns { t, end, until, untilEp, ep, from, to } — every field
  // present, null when absent or malformed — or null when the query names no
  // passage at all. `until` is an item id in the item form; a non-numeric
  // `until` (an episode code, show form) lands in untilEp as written, and
  // `ep` is kept as written too — both are resolved, and their spelling
  // judged, by the server (internal/passage.ResolveShow). An end at or before
  // the start is no bound and is dropped.
  function parsePassage(query) {
    if (query == null) return null;
    let q = String(query);
    if (q.startsWith('?')) q = q.slice(1);
    if (!q) return null;
    const params = new URLSearchParams(q);
    const untilRaw = nonEmpty(params.get('until'));
    const untilId = parseItemId(untilRaw);
    const p = {
      t: parseSeconds(params.get('t')),
      end: parseSeconds(params.get('end')),
      until: untilId,
      untilEp: untilId == null ? untilRaw : null,
      ep: nonEmpty(params.get('ep')),
      from: nonEmpty(params.get('from')),
      to: nonEmpty(params.get('to')),
    };
    if (p.end != null && p.t != null && p.end <= p.t) p.end = null;
    if (Object.values(p).every(v => v == null)) return null;
    return p;
  }

  // passageQuery is the inverse of parsePassage: '' for null, else
  // '?t=…&end=…&until=…&ep=…&from=…&to=…' with only the present fields, in
  // that order, so the same passage always spells the same hash.
  function passageQuery(p) {
    if (!p) return '';
    const parts = [];
    if (p.t != null) parts.push('t=' + String(p.t));
    if (p.end != null) parts.push('end=' + String(p.end));
    if (p.until != null) parts.push('until=' + String(p.until));
    else if (p.untilEp != null) parts.push('until=' + encodeURIComponent(p.untilEp));
    if (p.ep != null) parts.push('ep=' + encodeURIComponent(p.ep));
    if (p.from != null) parts.push('from=' + encodeURIComponent(p.from));
    if (p.to != null) parts.push('to=' + encodeURIComponent(p.to));
    return parts.length ? '?' + parts.join('&') : '';
  }

  // A passage puts the PLAYER into passage mode only when it says something
  // about time or episodes; from/to alone are the reader's business.
  function isTimedPassage(p) {
    return !!p && (p.t != null || p.end != null || p.until != null);
  }

  // The end bound that applies while itemId is playing: `end` belongs to
  // the single item of a scene, or to the LAST item of a run. Earlier items
  // of a run play out to their natural end.
  function passageEndAt(p, itemId) {
    if (!p || p.end == null) return null;
    if (p.until != null && itemId !== p.until) return null;
    return p.end;
  }

  // The end-detection predicate, fed with position() — which already
  // includes the HLS seek offset — or the cast receiver's current time.
  function passageEnded(pos, endAt) {
    return typeof pos === 'number' && typeof endAt === 'number' &&
      Number.isFinite(pos) && Number.isFinite(endAt) && pos >= endAt;
  }

  // After the item with endedItemId ended naturally: does the run go on?
  function runContinues(p, endedItemId) {
    return !!p && p.until != null && endedItemId !== p.until;
  }

  // The passage the NEXT item of a run is routed with: `until` carries on,
  // t/end were the first item's (and from/to are the reader's).
  function passageForNext(p) {
    if (!p || p.until == null) return null;
    return { t: null, end: null, until: p.until, untilEp: null, ep: null, from: null, to: null };
  }

  // --- making a passage while watching ----------------------------------------
  //
  // The producer side. The player keeps a MARK STATE — { t, inItem, end,
  // endItem }: the in point (seconds into the item with id inItem) and the
  // out point (seconds into endItem) — and these functions are the whole
  // logic of it: where a mark lands, what marking does to the state, which
  // flags an item shows, and the link that comes out. Nothing is stored
  // anywhere: a passage IS its URL, and the day planner keeps those.

  // A mark's seconds as the grammar spells them: whole seconds stay
  // integers, anything else is rounded to one decimal (79.5). Nonsense is 0.
  function markTime(pos) {
    const n = Number(pos);
    if (!Number.isFinite(n) || n < 0) return 0;
    return Math.round(n * 10) / 10;
  }

  // snapToChapter: the chapter start within `tolerance` seconds of pos (the
  // nearest, if several), else pos itself. A mark set a beat after a scene
  // change meant the scene change.
  function snapToChapter(pos, chapterStarts, tolerance) {
    const tol = tolerance == null ? 2 : tolerance;
    let best = pos, bestD = Infinity;
    for (const s of chapterStarts || []) {
      if (typeof s !== 'number' || !Number.isFinite(s)) continue;
      const d = Math.abs(s - pos);
      if (d <= tol && d < bestD) { best = s; bestD = d; }
    }
    return best;
  }

  const EMPTY_MARKS = { t: null, inItem: null, end: null, endItem: null };

  // markBounds(state, kind, pos, itemId, order): the state after marking
  // `kind` ('in' | 'out') at pos seconds into itemId. `order` is the ids of
  // the work's items in playing order (a show's episodes; omit for a single
  // film), so a mark on a later episode counts as later. An out point at or
  // before the in point swaps the two — and marking in past the out point
  // does the same; the person said where the passage is, not which end they
  // meant. Two marks on the very same instant are one point, not a passage:
  // the out is dropped.
  function markBounds(state, kind, pos, itemId, order) {
    const s = Object.assign({}, EMPTY_MARKS, state || {});
    const idx = id => { const i = (order || []).indexOf(id); return i < 0 ? 0 : i; };
    const cmp = (a, b) => idx(a.item) !== idx(b.item) ? idx(a.item) - idx(b.item) : a.pos - b.pos;
    const m = { item: itemId, pos: markTime(pos) };
    let i = s.t != null ? { item: s.inItem, pos: s.t } : null;
    let o = s.end != null ? { item: s.endItem, pos: s.end } : null;
    if (kind === 'in') i = m; else o = m;
    if (i && o) {
      const c = cmp(i, o);
      if (c === 0) o = null;
      else if (c > 0) [i, o] = [o, i];
    }
    return { t: i ? i.pos : null, inItem: i ? i.item : null,
             end: o ? o.pos : null, endItem: o ? o.item : null };
  }

  // clearMark removes one end; null when nothing is left.
  function clearMark(state, kind) {
    const s = Object.assign({}, EMPTY_MARKS, state || {});
    if (kind === 'in') { s.t = null; s.inItem = null; } else { s.end = null; s.endItem = null; }
    return s.t == null && s.end == null ? null : s;
  }

  // marksOn: the bounds itemId shows on its scrubber — each mark only on
  // the item it was set in.
  function marksOn(state, itemId) {
    if (!state) return { t: null, end: null };
    return { t: state.inItem === itemId ? state.t : null,
             end: state.endItem === itemId ? state.end : null };
  }

  // markPassage: the mark state as a passage record — `until` carries the
  // out point's item when it is a later episode than the in point's.
  function markPassage(state) {
    if (!state) return null;
    const until = state.endItem != null && state.endItem !== state.inItem ? state.endItem : null;
    return { t: state.t, end: state.end, until, untilEp: null, ep: null, from: null, to: null };
  }

  // passageLink: the absolute URL of the marked passage, or null until an
  // in point exists. base is the page's own address (origin + pathname).
  function passageLink(base, state) {
    if (!state || state.t == null || state.inItem == null) return null;
    return String(base || '') + '#/item/' + state.inItem + passageQuery(markPassage(state));
  }

  // --- text locators ---------------------------------------------------------
  // A text passage's bounds are LOCATORS, four spellings of a place in a
  // book: 'cfi:<epub cfi>' (a point, as the EPUB reader itself reports it),
  // 'ch:<n>' (the n-th spine section, 1-based, the way media_info.chapters
  // lists them), 'pct:<0..1>' (the book's own percentage) or 'pg:<n>' (the
  // n-th page of a PDF, 1-based, as the PDF reader reports it). A bare
  // 'epubcfi(…)' is read as a cfi locator too — the reader's progress speaks
  // that form. Anything else is no locator. cfi and ch mean nothing to a
  // PDF and pg nothing to an EPUB: the reader that gets one ignores it.
  function parseLocator(s) {
    if (s == null) return null;
    const v = String(s).trim();
    if (!v) return null;
    if (/^epubcfi\(.*\)$/.test(v)) return { kind: 'cfi', cfi: v };
    const i = v.indexOf(':');
    if (i < 0) return null;
    const kind = v.slice(0, i).toLowerCase(), rest = v.slice(i + 1);
    if (kind === 'cfi') return /^epubcfi\(.*\)$/.test(rest) ? { kind, cfi: rest } : null;
    if (kind === 'ch' || kind === 'pg') return /^[1-9]\d*$/.test(rest) ? { kind, n: Number(rest) } : null;
    if (kind === 'pct') return /^(0(\.\d+)?|1(\.0+)?|\.\d+)$/.test(rest) ? { kind, f: Number(rest) } : null;
    return null;
  }
  // formatLocator is the inverse of parseLocator: the canonical spelling.
  function formatLocator(loc) {
    if (!loc) return null;
    if (loc.kind === 'cfi') return 'cfi:' + loc.cfi;
    if (loc.kind === 'ch') return 'ch:' + loc.n;
    if (loc.kind === 'pct') return 'pct:' + loc.f;
    if (loc.kind === 'pg') return 'pg:' + loc.n;
    return null;
  }
  // The 1-based spine section a CFI points into, read off its spine step:
  // the itemref is an even child index, so 'epubcfi(/6/14!/4/2)' is section
  // 14/2 = 7. Mirrors model.SectionFromCFI on the server. null when the
  // string carries no such step.
  function sectionFromCFI(cfi) {
    const m = /^\s*epubcfi\(\/\d+\/(\d+)/.exec(String(cfi == null ? '' : cfi));
    if (!m) return null;
    const n = Number(m[1]);
    return n >= 2 && n % 2 === 0 ? n / 2 : null;
  }
  // The spine section (1-based) a locator lands in, for a book of `sections`
  // spine items: ch is itself, pct is proportional (1 lands in the last
  // section), cfi is read off the string; clamped into [1, sections]. null
  // when it cannot be told (a page says nothing about sections), or the book
  // has no sections.
  function locatorSection(loc, sections) {
    if (!loc || !(sections > 0)) return null;
    let n = null;
    if (loc.kind === 'ch') n = loc.n;
    else if (loc.kind === 'pct') n = Math.floor(loc.f * sections) + 1;
    else if (loc.kind === 'cfi') n = sectionFromCFI(loc.cfi);
    if (n == null) return null;
    return Math.min(sections, Math.max(1, n));
  }
  // A passage that says something about the text: from or to present. The
  // reader's counterpart of isTimedPassage; an item is a film or a book, so
  // the two never both apply to one route.
  function isTextPassage(p) {
    return !!p && (p.from != null || p.to != null);
  }
  // The reader's end predicate. `to` is a parsed locator; `pos` is where the
  // reader is — { cfi, section (1-based), pct (0..1), atEnd } for the page
  // shown, plus { page (1-based) } from the PDF reader. A 'ch:n' bound is
  // INCLUSIVE: the passage runs through section n and ends once the reader
  // is past it, the way an episode run's `until` plays the named episode
  // out. A 'pg:n' bound is inclusive too, and a page is its own last page:
  // the passage ends once page n is the page shown — it stays on view, the
  // reader just turns no further. 'cfi' and 'pct' bounds are points, reached
  // when the shown page starts at or after them. The book ending ends any
  // passage. compareCFI orders two CFI strings (-1/0/1) and comes from the
  // reader (epub.js's EpubCFI.compare); without it a cfi bound never fires.
  function textPassageEnded(to, pos, compareCFI) {
    if (!to || !pos) return false;
    if (pos.atEnd) return true;
    if (to.kind === 'ch') return typeof pos.section === 'number' && pos.section > to.n;
    if (to.kind === 'pg') return typeof pos.page === 'number' && pos.page >= to.n;
    if (to.kind === 'pct') return typeof pos.pct === 'number' && Number.isFinite(pos.pct) && pos.pct >= to.f;
    if (to.kind === 'cfi') {
      if (typeof pos.cfi !== 'string' || typeof compareCFI !== 'function') return false;
      try { return compareCFI(pos.cfi, to.cfi) >= 0; } catch (e) { return false; }
    }
    return false;
  }

  const api = { splitHash, parsePassage, passageQuery, isTimedPassage,
                passageEndAt, passageEnded, runContinues, passageForNext,
                markTime, snapToChapter, markBounds, clearMark, marksOn, markPassage, passageLink,
                parseLocator, formatLocator, sectionFromCFI, locatorSection,
                isTextPassage, textPassageEnded };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else Object.assign(root, api);
})(typeof window !== 'undefined' ? window : this);
