// Run with: node --test web/passage_test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
// passage.js is a plain browser script with a CommonJS export bolted on;
// Node cannot see named exports through its IIFE, so take the default.
import passage from './passage.js';
const { splitHash, parsePassage, passageQuery, isTimedPassage,
        passageEndAt, passageEnded, runContinues, passageForNext,
        parseEpisodeCode, findEpisode, resolveShowPassage } = passage;

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

// --- show-addressed passages -------------------------------------------------

// a show's members as /api/items lists them: zero season/episode omitted,
// bonus material carrying an episode-shaped name
const show = [
  { id: 40, identity: { kind: 'episode', title: 'Ninjago', season: 1, episode: 1 } },
  { id: 41, identity: { kind: 'episode', title: 'Ninjago', season: 1, episode: 2 } },
  { id: 58, identity: { kind: 'episode', title: 'Ninjago', season: 2, episode: 5 } },
  { id: 59, identity: { kind: 'episode', title: 'Ninjago', season: 2, episode: 6 } },
  { id: 60, identity: { kind: 'episode', title: 'Ninjago', season: 2, episode: 7 } },
  { id: 61, identity: { kind: 'episode', title: 'Ninjago' } },                       // unnumbered rip
  { id: 70, identity: { kind: 'extra', title: 'Ninjago', season: 2, episode: 5 } },  // deleted scene named S02E05
];

test('parseEpisodeCode reads SxxEyy in any case and nothing else', () => {
  assert.deepEqual(parseEpisodeCode('S02E05'), { season: 2, episode: 5 });
  assert.deepEqual(parseEpisodeCode('s2e5'), { season: 2, episode: 5 });
  assert.deepEqual(parseEpisodeCode(' S10E123 '), { season: 10, episode: 123 });
  assert.equal(parseEpisodeCode('E05'), null);
  assert.equal(parseEpisodeCode('S02E05x'), null);
  assert.equal(parseEpisodeCode(''), null);
  assert.equal(parseEpisodeCode(null), null);
});

test('findEpisode matches season/episode numbers, never bonus material', () => {
  assert.equal(findEpisode(show, { season: 2, episode: 5 }).id, 58);
  assert.equal(findEpisode(show, { season: 1, episode: 1 }).id, 40);
  assert.equal(findEpisode(show, { season: 2, episode: 9 }), null);
  assert.equal(findEpisode(show, { season: 0, episode: 0 }).id, 61); // an unnumbered file is S00E00
  assert.equal(findEpisode([], { season: 2, episode: 5 }), null);
  assert.equal(findEpisode(show, null), null);
});

test('resolveShowPassage: a scene of one episode becomes the item form', () => {
  const r = resolveShowPassage(parsePassage('ep=S02E05&t=60&end=300'), show);
  assert.equal(r.error, null);
  assert.equal(r.item.id, 58);
  assert.deepEqual(r.passage, P({ t: 60, end: 300 }));
  assert.equal(passageQuery(r.passage), '?t=60&end=300');
});

test('resolveShowPassage: a run resolves until to the last episode\'s id', () => {
  const r = resolveShowPassage(parsePassage('ep=s2e5&t=120&until=S02E07'), show);
  assert.equal(r.error, null);
  assert.equal(r.item.id, 58);
  assert.deepEqual(r.passage, P({ t: 120, until: 60 }));
  // an item-id until passes through untouched
  assert.equal(resolveShowPassage(parsePassage('ep=S02E05&until=60'), show).passage.until, 60);
});

test('resolveShowPassage: ep alone is a plain episode route', () => {
  const r = resolveShowPassage(parsePassage('ep=S01E02'), show);
  assert.equal(r.item.id, 41);
  assert.equal(passageQuery(r.passage), '');
  assert.equal(isTimedPassage(r.passage), false);
});

test('resolveShowPassage: an ep that names no episode is one sentence, no item', () => {
  let r = resolveShowPassage(parsePassage('ep=S02E09&t=60'), show);
  assert.equal(r.item, null);
  assert.equal(r.passage, null);
  assert.equal(r.error, 'This show has no episode S02E09.');
  r = resolveShowPassage(parsePassage('ep=finale&t=60'), show);
  assert.equal(r.item, null);
  assert.equal(r.error, '"finale" is not an episode code like S02E05.');
  r = resolveShowPassage(parsePassage('ep=S02E05&until=S02E99'), show);
  assert.equal(r.item, null);
  assert.equal(r.error, 'This show has no episode S02E99 to run until.');
  assert.equal(resolveShowPassage(parsePassage('t=60'), show).item, null); // no ep at all
});
