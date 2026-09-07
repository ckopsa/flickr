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
// in, mark out, copy the link) — that half is the server's now, on the
// session document (docs/hypermedia.md §Passages are session state), and
// what stayed here is the grammar and the clock. Everything in this file is
// pure: it is loaded by index.html as a plain script (globals) and by
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

  // --- making a passage: not here any more -------------------------------------
  //
  // Marking in and out, the snap to a chapter start, the swap of a backwards
  // pair, the run's `until` and the minted link were all in this file. They
  // are the SERVER's now (cmd/server/session.go and cmd/server/passage.go):
  // the player posts mark_in / mark_out to the session and renders the marks
  // and the sentence that come back. What is left here is the clock — the
  // one thing the browser owns, because it is the browser that is playing.

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
  // Which SECTION (or, for a PDF, which PAGE) a locator lands in is not here
  // any more: the reading session answers it (`passage.from_section` /
  // `from_page`, cmd/server/read.go), resolved against the book the server
  // probed rather than against what the client happens to know about it.
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

  // --- the other bound in the same clock: a chapter ---------------------------
  //
  // Not a passage. The sleep timer's "end of chapter" (web/player.js) is a
  // bound in the item's own clock like every bound above, and it lives here
  // for the same reason they do: this file is the client's pure clock, the
  // half that can be tested without a browser.
  //
  // Where the CURRENT chapter ends is where the next one starts, so the bound
  // is the first start STRICTLY after `seconds` — a position sitting exactly
  // on a mark is in that chapter, not the one before it. Marks in any order,
  // entries with no clock ignored, and null when there is no boundary left at
  // all: a file with no chapters, one chapter, or a position past the last
  // mark, none of which is an end to stop at.
  function nextChapterStart(chapters, seconds) {
    const from = typeof seconds === 'number' && Number.isFinite(seconds) ? seconds : 0;
    let next = null;
    for (const ch of chapters || []) {
      if (!ch || typeof ch.start_seconds !== 'number' || !Number.isFinite(ch.start_seconds)) continue;
      if (ch.start_seconds <= from) continue;
      if (next == null || ch.start_seconds < next) next = ch.start_seconds;
    }
    return next;
  }

  const api = { splitHash, parsePassage, passageQuery, isTimedPassage,
                nextChapterStart,
                passageEndAt, passageEnded, runContinues, passageForNext,
                parseLocator, formatLocator, sectionFromCFI,
                isTextPassage, textPassageEnded };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else Object.assign(root, api);
})(typeof window !== 'undefined' ? window : this);
