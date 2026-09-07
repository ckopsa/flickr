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
//                            text passages for the future epub reader;
//                            parsed and preserved here, no player behaviour
//   #/show/<title>?ep=S02E05&t=…&end=…[&until=S02E07]
//                            the same passage addressed by show and episode
//                            code, for a minter that knows no item ids; it
//                            is resolved against the show's episodes at open
//                            time (resolveShowPassage) and then behaves
//                            exactly like the item form
//
// Seconds may be integers or decimals. Everything in this file is pure: it
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
  // judged, by resolveShowPassage. An end at or before the start is no bound
  // and is dropped.
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

  // 'S02E05' / 's2e5' → { season: 2, episode: 5 }; anything else → null.
  function parseEpisodeCode(s) {
    const m = /^\s*s(\d{1,3})e(\d{1,4})\s*$/i.exec(String(s == null ? '' : s));
    return m ? { season: Number(m[1]), episode: Number(m[2]) } : null;
  }
  function formatEpisodeCode(c) {
    return 'S' + String(c.season).padStart(2, '0') + 'E' + String(c.episode).padStart(2, '0');
  }
  // The episode of a show matching a code. items are the show's members as
  // the server lists them: identity.season / identity.episode, with zero
  // values omitted; bonus material (kind "extra") named after an episode is
  // never that episode.
  function findEpisode(items, code) {
    if (!code) return null;
    for (const it of items || []) {
      const id = it && it.identity;
      if (!id || (id.kind != null && id.kind !== 'episode')) continue;
      if ((id.season || 0) === code.season && (id.episode || 0) === code.episode) return it;
    }
    return null;
  }

  // resolveShowPassage turns a show-addressed passage into the item form:
  // { item, passage, error }. item is the episode `ep` names and passage is
  // ready for itemHash (until resolved to an id, ep dropped); or item is
  // null and error is one plain sentence for the show page. A run whose
  // `until` names no episode is an error too — silently shortening the run
  // would play something other than what the link says.
  function resolveShowPassage(p, items) {
    const fail = error => ({ item: null, passage: null, error });
    const want = parseEpisodeCode(p && p.ep);
    if (!want) return fail(`"${p && p.ep != null ? p.ep : ''}" is not an episode code like S02E05.`);
    const item = findEpisode(items, want);
    if (!item) return fail(`This show has no episode ${formatEpisodeCode(want)}.`);
    let until = p.until;
    if (p.untilEp != null) {
      const w = parseEpisodeCode(p.untilEp);
      const u = findEpisode(items, w);
      if (!u) return fail(`This show has no episode ${w ? formatEpisodeCode(w) : p.untilEp} to run until.`);
      until = u.id;
    }
    return {
      item,
      passage: { t: p.t, end: p.end, until, untilEp: null, ep: null, from: p.from, to: p.to },
      error: null,
    };
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

  const api = { splitHash, parsePassage, passageQuery, isTimedPassage,
                passageEndAt, passageEnded, runContinues, passageForNext,
                parseEpisodeCode, findEpisode, resolveShowPassage };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else Object.assign(root, api);
})(typeof window !== 'undefined' ? window : this);
