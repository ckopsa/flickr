// Run with: node --test web/passage_test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
// passage.js is a plain browser script with a CommonJS export bolted on;
// Node cannot see named exports through its IIFE, so take the default.
import passage from './passage.js';
const { splitHash, parsePassage, passageQuery, isTimedPassage,
        passageEndAt, passageEnded, runContinues, passageForNext,
        markTime, snapToChapter, markBounds, clearMark, marksOn, markPassage, passageLink,
        parseLocator, formatLocator, sectionFromCFI, locatorSection,
        isTextPassage, textPassageEnded } = passage;

// a full passage record with every field null except the overrides
const P = o => ({ t: null, end: null, until: null, untilEp: null, ep: null, from: null, to: null, ...o });

test('splitHash separates the hash path from its query', () => {
  assert.deepEqual(splitHash('#/item/51?t=4740&end=5070'), { path: '#/item/51', query: 't=4740&end=5070' });
  assert.deepEqual(splitHash('#/item/51'), { path: '#/item/51', query: '' });
  assert.deepEqual(splitHash('#/show/Some%3FTitle'), { path: '#/show/Some%3FTitle', query: '' });
  assert.deepEqual(splitHash('#/show/Ninjago?ep=S02E05&t=60'), { path: '#/show/Ninjago', query: 'ep=S02E05&t=60' });
  assert.deepEqual(splitHash(''), { path: '', query: '' });
});

test('parsePassage: a scene', () => {
  assert.deepEqual(parsePassage('t=4740&end=5070'), P({ t: 4740, end: 5070 }));
  assert.deepEqual(parsePassage('?t=4740&end=5070'), P({ t: 4740, end: 5070 }));
});

test('parsePassage: decimals are seconds too', () => {
  const p = parsePassage('t=79.5&end=330.25');
  assert.equal(p.t, 79.5);
  assert.equal(p.end, 330.25);
});

test('parsePassage: an episode run by item id', () => {
  assert.deepEqual(parsePassage('t=120&until=61'), P({ t: 120, until: 61 }));
  assert.deepEqual(parsePassage('until=61&end=900'), P({ end: 900, until: 61 }));
});

test('parsePassage: the show form keeps ep and an episode-code until as written', () => {
  assert.deepEqual(parsePassage('ep=S02E05&t=60&end=300'), P({ t: 60, end: 300, ep: 'S02E05' }));
  assert.deepEqual(parsePassage('ep=s2e5&until=S02E07'), P({ ep: 's2e5', untilEp: 'S02E07' }));
  assert.deepEqual(parsePassage('ep=nonsense'), P({ ep: 'nonsense' })); // judged at resolution, not here
});

test('parsePassage: text locators are preserved, decoded, and not timed', () => {
  const p = parsePassage('from=epubcfi(%2F6%2F4!%2F4%2F2)&to=ch03');
  assert.deepEqual(p, P({ from: 'epubcfi(/6/4!/4/2)', to: 'ch03' }));
  assert.equal(isTimedPassage(p), false);
  assert.equal(isTimedPassage(parsePassage('t=1')), true);
  assert.equal(isTimedPassage(parsePassage('until=9')), true);
  assert.equal(isTimedPassage(null), false);
});

test('parsePassage: nothing there is null, garbage is absent', () => {
  assert.equal(parsePassage(''), null);
  assert.equal(parsePassage(null), null);
  assert.equal(parsePassage('foo=bar'), null);
  assert.equal(parsePassage('t=abc&end=-5'), null);
  assert.deepEqual(parsePassage('t=abc&end=90'), P({ end: 90 }));
  assert.equal(parsePassage('t=&end=&until=&ep='), null);
});

test('parsePassage: an end at or before the start is dropped', () => {
  assert.equal(parsePassage('t=100&end=100').end, null);
  assert.equal(parsePassage('t=100&end=90').end, null);
  assert.equal(parsePassage('t=100&end=90').t, 100);
});

test('passageQuery round-trips and is stable', () => {
  for (const q of ['?t=4740&end=5070', '?t=120&until=61', '?end=900&until=61',
                   '?t=79.5&end=330.25', '?from=epubcfi(%2F6%2F4!%2F4%2F2)&to=ch03',
                   '?t=60&end=300&until=S02E07&ep=S02E05']) {
    assert.equal(passageQuery(parsePassage(q)), q);
  }
  assert.equal(passageQuery(null), '');
  assert.equal(passageQuery(parsePassage('foo=1')), '');
  // field order is fixed regardless of input order
  assert.equal(passageQuery(parsePassage('until=61&t=120')), '?t=120&until=61');
});

test('passageEndAt: end belongs to the scene item or the last item of a run', () => {
  const scene = parsePassage('t=10&end=20');
  assert.equal(passageEndAt(scene, 51), 20);
  const run = parsePassage('t=10&end=900&until=61');
  assert.equal(passageEndAt(run, 59), null);
  assert.equal(passageEndAt(run, 61), 900);
  assert.equal(passageEndAt(parsePassage('t=10'), 51), null);
  assert.equal(passageEndAt(null, 51), null);
});

test('passageEnded fires at or past the bound and never on nonsense', () => {
  assert.equal(passageEnded(5069.75, 5070), false);
  assert.equal(passageEnded(5070, 5070), true);
  assert.equal(passageEnded(5070.4, 5070), true);
  assert.equal(passageEnded(5070, null), false);
  assert.equal(passageEnded(NaN, 5070), false);
  assert.equal(passageEnded(undefined, 5070), false);
});

test('runContinues until the named item has ended', () => {
  const run = parsePassage('until=61');
  assert.equal(runContinues(run, 59), true);
  assert.equal(runContinues(run, 61), false);
  assert.equal(runContinues(parsePassage('t=1&end=2'), 59), false);
  assert.equal(runContinues(null, 59), false);
});

test('passageForNext keeps until and drops the first item\'s t/end', () => {
  assert.deepEqual(passageForNext(parsePassage('t=120&end=900&until=61')), P({ until: 61 }));
  assert.equal(passageQuery(passageForNext(parsePassage('t=120&until=61'))), '?until=61');
  assert.equal(passageForNext(parsePassage('t=120&end=900')), null);
  assert.equal(passageForNext(null), null);
});

// The show form (#/show/<title>?ep=S02E05) is resolved by the SERVER —
// internal/passage, answered by GET /api/-/route — and its cases live in
// internal/passage/passage_test.go under these same names. What is left here
// is what the browser still owns: the parse of a passage it is handed, the
// clock predicates, and the marks it makes.

// --- making a passage while watching ----------------------------------------

// a mark state with every field null except the overrides
const M = o => ({ t: null, inItem: null, end: null, endItem: null, ...o });
const BASE = 'http://flickr.lan:8099/';

test('markTime spells seconds the way the grammar does', () => {
  assert.equal(markTime(4740), 4740);
  assert.equal(markTime(79.5), 79.5);
  assert.equal(markTime(79.96), 80);
  assert.equal(markTime(79.04), 79);
  assert.equal(markTime(1.25), 1.3);
  assert.equal(String(markTime(60.0)), '60');
  assert.equal(markTime(-3), 0);
  assert.equal(markTime(NaN), 0);
  assert.equal(markTime(undefined), 0);
});

test('snapToChapter takes the nearest chapter start within tolerance, else the position', () => {
  const starts = [0, 600, 1200.5];
  assert.equal(snapToChapter(601.4, starts), 600);
  assert.equal(snapToChapter(598.2, starts), 600);
  assert.equal(snapToChapter(1202.5, starts), 1200.5); // exactly at the tolerance counts
  assert.equal(snapToChapter(603, starts), 603);
  assert.equal(snapToChapter(1.5, starts), 0);
  assert.equal(snapToChapter(605, starts, 5), 600);   // a wider tolerance
  assert.equal(snapToChapter(605, starts, 0), 605);   // none at all
  assert.equal(snapToChapter(300, []), 300);
  assert.equal(snapToChapter(300, null), 300);
  assert.equal(snapToChapter(599.3, [598, 601]), 598); // two within reach: the nearer
  assert.equal(snapToChapter(300, [NaN, null, 'x']), 300);
});

test('markBounds: in then out on one item', () => {
  let s = markBounds(null, 'in', 4740.03, 51);
  assert.deepEqual(s, M({ t: 4740, inItem: 51 }));
  s = markBounds(s, 'out', 5070.5, 51);
  assert.deepEqual(s, M({ t: 4740, inItem: 51, end: 5070.5, endItem: 51 }));
  assert.equal(passageLink(BASE, s), BASE + '#/item/51?t=4740&end=5070.5');
});

test('markBounds: tapping a set chip again re-marks at the new position', () => {
  let s = markBounds(M({ t: 4740, inItem: 51, end: 5070, endItem: 51 }), 'in', 4800, 51);
  assert.deepEqual(s, M({ t: 4800, inItem: 51, end: 5070, endItem: 51 }));
  s = markBounds(s, 'out', 5000, 51);
  assert.deepEqual(s, M({ t: 4800, inItem: 51, end: 5000, endItem: 51 }));
});

test('markBounds: an out before the in swaps them, and so does an in past the out', () => {
  let s = markBounds(M({ t: 4740, inItem: 51 }), 'out', 4000, 51);
  assert.deepEqual(s, M({ t: 4000, inItem: 51, end: 4740, endItem: 51 }));
  s = markBounds(M({ t: 4000, inItem: 51, end: 4740, endItem: 51 }), 'in', 5000, 51);
  assert.deepEqual(s, M({ t: 4740, inItem: 51, end: 5000, endItem: 51 }));
});

test('markBounds: out first is fine; in later completes the passage', () => {
  let s = markBounds(null, 'out', 5070, 51);
  assert.deepEqual(s, M({ end: 5070, endItem: 51 }));
  assert.equal(passageLink(BASE, s), null); // no in point, no link yet
  s = markBounds(s, 'in', 4740, 51);
  assert.deepEqual(s, M({ t: 4740, inItem: 51, end: 5070, endItem: 51 }));
  assert.equal(passageLink(BASE, s), BASE + '#/item/51?t=4740&end=5070');
});

test('markBounds: the same instant twice is a point, not a passage', () => {
  const s = markBounds(M({ t: 4740, inItem: 51 }), 'out', 4740.04, 51);
  assert.deepEqual(s, M({ t: 4740, inItem: 51 }));
});

test('markBounds: an episode run — out on a later episode sets until', () => {
  const order = [58, 59, 60];
  let s = markBounds(null, 'in', 120, 58, order);
  s = markBounds(s, 'out', 900, 60, order);
  assert.deepEqual(s, M({ t: 120, inItem: 58, end: 900, endItem: 60 }));
  assert.deepEqual(markPassage(s), P({ t: 120, end: 900, until: 60 }));
  assert.equal(passageLink(BASE, s), BASE + '#/item/58?t=120&end=900&until=60');
  // the link plays back as the run it describes
  const p = parsePassage(splitHash(passageLink(BASE, s).slice(BASE.length)).query);
  assert.equal(passageEndAt(p, 58), null);
  assert.equal(passageEndAt(p, 59), null);
  assert.equal(passageEndAt(p, 60), 900);
  assert.equal(runContinues(p, 59), true);
  assert.equal(runContinues(p, 60), false);
});

test('markBounds: across episodes the later episode is later even at an earlier second', () => {
  const order = [58, 59, 60];
  // out on the next episode at 0:10, in was at 1:00:00 of the previous — no swap
  let s = markBounds(M({ t: 3600, inItem: 58 }), 'out', 10, 59, order);
  assert.deepEqual(s, M({ t: 3600, inItem: 58, end: 10, endItem: 59 }));
  // out on an EARLIER episode than the in: swapped, the run goes forward
  s = markBounds(M({ t: 120, inItem: 60 }), 'out', 900, 58, order);
  assert.deepEqual(s, M({ t: 900, inItem: 58, end: 120, endItem: 60 }));
  // moving the in point onto the out point's episode collapses the run
  s = markBounds(M({ t: 120, inItem: 58, end: 900, endItem: 60 }), 'in', 300, 60, order);
  assert.deepEqual(s, M({ t: 300, inItem: 60, end: 900, endItem: 60 }));
  assert.equal(markPassage(s).until, null);
  // ids outside the order (a film) compare by seconds alone
  s = markBounds(M({ t: 120, inItem: 7 }), 'out', 60, 7, order);
  assert.deepEqual(s, M({ t: 60, inItem: 7, end: 120, endItem: 7 }));
});

test('clearMark removes one end and returns null when none is left', () => {
  const both = M({ t: 120, inItem: 58, end: 900, endItem: 60 });
  assert.deepEqual(clearMark(both, 'in'), M({ end: 900, endItem: 60 }));
  assert.deepEqual(clearMark(both, 'out'), M({ t: 120, inItem: 58 }));
  assert.equal(clearMark(M({ t: 120, inItem: 58 }), 'in'), null);
  assert.equal(clearMark(null, 'out'), null);
});

test('marksOn shows each flag only on the item it was set in', () => {
  const run = M({ t: 120, inItem: 58, end: 900, endItem: 60 });
  assert.deepEqual(marksOn(run, 58), { t: 120, end: null });
  assert.deepEqual(marksOn(run, 59), { t: null, end: null });
  assert.deepEqual(marksOn(run, 60), { t: null, end: 900 });
  assert.deepEqual(marksOn(M({ t: 120, inItem: 58, end: 900, endItem: 58 }), 58), { t: 120, end: 900 });
  assert.deepEqual(marksOn(null, 58), { t: null, end: null });
});

test('passageLink builds the absolute item form with the grammar\'s spelling', () => {
  assert.equal(passageLink('http://localhost:8099/', M({ t: 79.5, inItem: 51, end: 330.25, endItem: 51 })),
               'http://localhost:8099/#/item/51?t=79.5&end=330.25');
  assert.equal(passageLink('http://localhost:8099/', M({ t: 120, inItem: 58 })),
               'http://localhost:8099/#/item/58?t=120'); // an in point alone: start there, play on
  assert.equal(passageLink('http://localhost:8099/', M({ end: 900, endItem: 58 })), null);
  assert.equal(passageLink('http://localhost:8099/', null), null);
  assert.equal(markPassage(null), null);
});

// --- text locators -----------------------------------------------------------

test('parseLocator reads the three spellings and nothing else', () => {
  assert.deepEqual(parseLocator('cfi:epubcfi(/6/14!/4/2/1:0)'), { kind: 'cfi', cfi: 'epubcfi(/6/14!/4/2/1:0)' });
  assert.deepEqual(parseLocator('epubcfi(/6/14!/4/2)'), { kind: 'cfi', cfi: 'epubcfi(/6/14!/4/2)' }); // bare, as progress reports it
  assert.deepEqual(parseLocator('ch:7'), { kind: 'ch', n: 7 });
  assert.deepEqual(parseLocator('CH:7'), { kind: 'ch', n: 7 });
  assert.deepEqual(parseLocator('pct:0.34'), { kind: 'pct', f: 0.34 });
  assert.deepEqual(parseLocator('pct:1'), { kind: 'pct', f: 1 });
  assert.deepEqual(parseLocator('pct:0'), { kind: 'pct', f: 0 });
  assert.deepEqual(parseLocator('pct:.5'), { kind: 'pct', f: 0.5 });
  assert.deepEqual(parseLocator(' ch:3 '), { kind: 'ch', n: 3 });
  assert.deepEqual(parseLocator('pg:213'), { kind: 'pg', n: 213 });
  assert.deepEqual(parseLocator('PG:1'), { kind: 'pg', n: 1 });
  for (const bad of ['ch:0', 'ch:-1', 'ch:3.5', 'ch:', 'pct:1.5', 'pct:-0.1', 'pct:abc', 'cfi:nope',
                     'ch03', 'page:12', 'pg:0', 'pg:-2', 'pg:3.5', 'pg:', 'pg:abc', 'p:12',
                     '', null, undefined, 'cfi:']) {
    assert.equal(parseLocator(bad), null, `${bad} should not parse`);
  }
});

test('formatLocator is the canonical spelling and round-trips', () => {
  for (const s of ['cfi:epubcfi(/6/14!/4/2/1:0)', 'ch:7', 'pct:0.34', 'pg:213']) {
    assert.equal(formatLocator(parseLocator(s)), s);
  }
  assert.equal(formatLocator(parseLocator('PG:9')), 'pg:9');
  assert.equal(formatLocator(parseLocator('epubcfi(/6/2!/4)')), 'cfi:epubcfi(/6/2!/4)');
  assert.equal(formatLocator(parseLocator('CH:7')), 'ch:7');
  assert.equal(formatLocator(null), null);
  assert.equal(formatLocator({ kind: 'nonsense' }), null);
});

test('sectionFromCFI reads the spine step like the server does', () => {
  assert.equal(sectionFromCFI('epubcfi(/6/14!/4/2/1:0)'), 7);
  assert.equal(sectionFromCFI('epubcfi(/6/14[ch07]!/4/2/1:0)'), 7);
  assert.equal(sectionFromCFI('epubcfi(/6/2!/4/2)'), 1);
  assert.equal(sectionFromCFI(' epubcfi(/6/40!/4) '), 20);
  assert.equal(sectionFromCFI('epubcfi(/6/13!/4)'), null); // odd: not an element step
  assert.equal(sectionFromCFI('epubcfi(/6)'), null);
  assert.equal(sectionFromCFI('/6/14!/4'), null);
  assert.equal(sectionFromCFI(''), null);
  assert.equal(sectionFromCFI(null), null);
});

test('locatorSection lands every kind in a 1-based section, clamped', () => {
  assert.equal(locatorSection(parseLocator('ch:3'), 12), 3);
  assert.equal(locatorSection(parseLocator('ch:99'), 12), 12);
  assert.equal(locatorSection(parseLocator('pct:0'), 12), 1);
  assert.equal(locatorSection(parseLocator('pct:0.5'), 12), 7);
  assert.equal(locatorSection(parseLocator('pct:1'), 12), 12);
  assert.equal(locatorSection(parseLocator('cfi:epubcfi(/6/14!/4/2)'), 12), 7);
  assert.equal(locatorSection(parseLocator('cfi:epubcfi(/6/14!/4/2)'), 3), 3);
  assert.equal(locatorSection(parseLocator('cfi:epubcfi(/6)'), 12), null);
  assert.equal(locatorSection(parseLocator('ch:3'), 0), null);
  assert.equal(locatorSection(parseLocator('pg:3'), 12), null); // a page says nothing about sections
  assert.equal(locatorSection(null, 12), null);
});

test('isTextPassage: from or to, and never a timed one', () => {
  assert.equal(isTextPassage(parsePassage('from=ch:3&to=ch:4')), true);
  assert.equal(isTextPassage(parsePassage('from=ch:3')), true);
  assert.equal(isTextPassage(parsePassage('to=pct:0.5')), true);
  assert.equal(isTextPassage(parsePassage('t=10&end=20')), false);
  assert.equal(isTextPassage(parsePassage('until=9')), false);
  assert.equal(isTextPassage(null), false);
  // the two grammars ride the same query without meeting
  const p = parsePassage('from=ch:3&to=ch:4');
  assert.equal(isTimedPassage(p), false);
  assert.equal(passageQuery(p), '?from=ch%3A3&to=ch%3A4');
  assert.deepEqual(parsePassage(passageQuery(p)), p);
});

// a fake CFI order for the predicate: compares the spine step, then the
// rest lexically — enough to stand in for epub.js's EpubCFI.compare
const cmp = (a, b) => {
  const sa = sectionFromCFI(a), sb = sectionFromCFI(b);
  if (sa !== sb) return sa < sb ? -1 : 1;
  return a < b ? -1 : a > b ? 1 : 0;
};
const at = o => ({ cfi: 'epubcfi(/6/8!/4/2)', section: 4, pct: 0.3, atEnd: false, ...o });

test('textPassageEnded: a ch bound is inclusive, points are reached at or after', () => {
  const ch4 = parseLocator('ch:4');
  assert.equal(textPassageEnded(ch4, at({ section: 3 }), cmp), false);
  assert.equal(textPassageEnded(ch4, at({ section: 4 }), cmp), false); // still inside chapter 4
  assert.equal(textPassageEnded(ch4, at({ section: 5 }), cmp), true);
  const half = parseLocator('pct:0.5');
  assert.equal(textPassageEnded(half, at({ pct: 0.49 }), cmp), false);
  assert.equal(textPassageEnded(half, at({ pct: 0.5 }), cmp), true);
  assert.equal(textPassageEnded(half, at({ pct: NaN }), cmp), false);
  assert.equal(textPassageEnded(half, at({ pct: null }), cmp), false);
  const point = parseLocator('cfi:epubcfi(/6/8!/4/6)');
  assert.equal(textPassageEnded(point, at({ cfi: 'epubcfi(/6/8!/4/2)' }), cmp), false);
  assert.equal(textPassageEnded(point, at({ cfi: 'epubcfi(/6/8!/4/6)' }), cmp), true);
  assert.equal(textPassageEnded(point, at({ cfi: 'epubcfi(/6/10!/4/2)' }), cmp), true);
  assert.equal(textPassageEnded(point, at({ cfi: 'epubcfi(/6/8!/4/8)' }), undefined), false); // no order, no verdict
  assert.equal(textPassageEnded(point, at({}), () => { throw new Error('bad cfi'); }), false);
});

test('textPassageEnded: a pg bound ends the passage on the page itself', () => {
  const pg240 = parseLocator('pg:240');
  const onPage = n => ({ page: n, pct: n / 400, section: 0, cfi: '', atEnd: n >= 400 });
  assert.equal(textPassageEnded(pg240, onPage(213), undefined), false);
  assert.equal(textPassageEnded(pg240, onPage(239), undefined), false);
  assert.equal(textPassageEnded(pg240, onPage(240), undefined), true); // page 240 is shown: the passage's last page
  assert.equal(textPassageEnded(pg240, onPage(241), undefined), true);
  assert.equal(textPassageEnded(pg240, onPage(400), undefined), true); // the book ending ends it too
  assert.equal(textPassageEnded(pg240, at({ page: undefined }), cmp), false); // an EPUB reader reports no page
  // The other spellings work on a page position: pct is a point, ch and cfi
  // never fire without a section or a CFI to judge.
  assert.equal(textPassageEnded(parseLocator('pct:0.5'), onPage(200), undefined), true);
  assert.equal(textPassageEnded(parseLocator('pct:0.5'), onPage(199), undefined), false);
  assert.equal(textPassageEnded(parseLocator('ch:4'), onPage(213), undefined), false);
  assert.equal(textPassageEnded(parseLocator('cfi:epubcfi(/6/8!/4/6)'), onPage(213), cmp), false);
});

test('textPassageEnded: the book ending ends any passage, nothing else fires on nothing', () => {
  assert.equal(textPassageEnded(parseLocator('ch:99'), at({ section: 12, atEnd: true }), cmp), true);
  assert.equal(textPassageEnded(parseLocator('pct:1'), at({ pct: 0.98, atEnd: true }), cmp), true);
  assert.equal(textPassageEnded(null, at({ atEnd: true }), cmp), false);
  assert.equal(textPassageEnded(parseLocator('ch:4'), null, cmp), false);
  assert.equal(textPassageEnded(parseLocator('ch:4'), at({ section: undefined }), cmp), false);
});
