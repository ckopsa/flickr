// Run with: node --test web/passage_test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
// passage.js is a plain browser script with a CommonJS export bolted on;
// Node cannot see named exports through its IIFE, so take the default.
import passage from './passage.js';
const { splitHash, parsePassage, passageQuery, isTimedPassage,
        passageEndAt, passageEnded, runContinues, passageForNext,
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

// Making a passage — markTime, snapToChapter, markBounds, clearMark,
// marksOn, markPassage, passageLink — is the SERVER's now (hyper 4): its
// tests are cmd/server/session_test.go, over the same rules.

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
