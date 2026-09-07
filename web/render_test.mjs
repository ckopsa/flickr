// Run with: node --test web/render_test.mjs
//
// The renderers, over the SERVER's OWN GOLDENS. web/testdata/hyper/*.json is
// a copy of cmd/server/testdata/hyper/*.json that a Go test keeps honest
// (cmd/server/goldens_shared_test.go fails the moment the two differ), so the
// two sides of the wire are tested against one truth rather than two fixtures
// that agree until somebody edits one.
//
// What is asserted here is the CONTRACT, not the pixels:
//
//   * every action the document offers becomes a control carrying the
//     document's own label (bar the ones a screen uses without drawing);
//   * every address in the output was copied out of the document — nothing is
//     composed, and the only things that are not addresses are hashes;
//   * no renderer emits a <video>: the media element is the device's, created
//     once, and a re-render must never make a second one;
//   * unknown fields are ignored, and a missing action is a control that is
//     simply not there.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// renderers.js is a plain browser script with a CommonJS export bolted on;
// Node cannot see named exports through its IIFE, so take the default.
import R from './renderers.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const goldenDir = path.join(here, 'testdata', 'hyper');

function golden(name) {
  return JSON.parse(fs.readFileSync(path.join(goldenDir, name + '.json'), 'utf8'));
}

// Every href="…" and data-href="…" the output carries.
function addresses(html) {
  const out = [];
  for (const m of html.matchAll(/(?:^|\s)(?:data-)?href="([^"]*)"/g)) out.push(unesc(m[1]));
  for (const m of html.matchAll(/\ssrc="([^"]*)"/g)) out.push(unesc(m[1]));
  return out;
}
function unesc(s) {
  return s.replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"').replace(/&#39;/g, "'");
}

// Every action name the document offers that a screen is expected to draw.
function drawnActions(doc) {
  return Object.keys((doc && doc.actions) || {}).filter(n => !R.SILENT_ACTIONS.has(n));
}

// The whole document as one string, so "did this address come out of the
// document?" is one substring test rather than a walk over every shape.
function flat(doc) { return JSON.stringify(doc); }

// --- the six renderers over their goldens ------------------------------------

const cases = [
  { name: 'library', golden: 'library', render: d => R.library(d, {}) },
  { name: 'continue', golden: 'continue', render: d => R.continueShelf(d) },
  { name: 'work (show)', golden: 'work-show', render: d => R.work(d) },
  { name: 'work (album)', golden: 'work-album', render: d => R.work(d) },
  { name: 'work (audiobook)', golden: 'work-audiobook', render: d => R.work(d) },
  { name: 'work (book)', golden: 'work-book', render: d => R.work(d) },
  { name: 'artist', golden: 'artist', render: d => R.artist(d) },
  { name: 'item (film)', golden: 'item-film', render: d => R.item(d, {}) },
  { name: 'session (play)', golden: 'session-play', render: d => R.session(d, {}) },
];

for (const c of cases) {
  test(`${c.name}: every action becomes a control with its label`, () => {
    const doc = golden(c.golden);
    const html = c.render(doc);
    for (const name of drawnActions(doc)) {
      assert.match(html, new RegExp('data-act="' + name + '"'),
        `${c.golden}: no control for action ${name}`);
      const label = doc.actions[name].label;
      assert.ok(html.includes(R.esc(label)),
        `${c.golden}: control ${name} does not carry its label ${JSON.stringify(label)}`);
      assert.ok(html.includes('data-href="' + R.esc(doc.actions[name].href) + '"'),
        `${c.golden}: control ${name} does not carry its href`);
    }
  });

  test(`${c.name}: every address came out of the document`, () => {
    const doc = golden(c.golden);
    const html = c.render(doc);
    const body = flat(doc);
    for (const a of addresses(html)) {
      if (a.startsWith('#')) continue; // a hash is the client's own grammar
      assert.ok(body.includes(JSON.stringify(a).slice(1, -1)),
        `${c.golden}: ${a} is not an address this document gave`);
    }
  });

  test(`${c.name}: no renderer makes a media element`, () => {
    const html = c.render(golden(c.golden));
    assert.ok(!/<video\b/i.test(html), `${c.golden}: a renderer emitted a <video>`);
    assert.ok(!/<audio\b/i.test(html), `${c.golden}: a renderer emitted an <audio>`);
  });

  test(`${c.name}: an unknown field changes nothing`, () => {
    const doc = golden(c.golden);
    const grown = JSON.parse(JSON.stringify(doc));
    grown.something_the_server_added_later = { deep: [1, 2, 3] };
    assert.equal(c.render(grown), c.render(doc));
  });
}

// --- the rules, said once ----------------------------------------------------

test('the renderer source composes no address', () => {
  const src = fs.readFileSync(path.join(here, 'renderers.js'), 'utf8');
  // The comments say '/api/' when they explain the rule; the CODE must not.
  const code = src.replace(/\/\/[^\n]*/g, '').replace(/\/\*[\s\S]*?\*\//g, '');
  assert.ok(!code.includes("'/api"), "renderers.js composes an /api address");
  assert.ok(!code.includes('"/api'), 'renderers.js composes an /api address');
  assert.ok(!code.includes('http://') && !code.includes('https://'),
    'renderers.js names a host');
});

test('the session chrome hands the device a slot, never a player', () => {
  const html = R.session(golden('session-play'), {});
  const slots = html.match(/id="device-slot"/g) || [];
  assert.equal(slots.length, 1,
    'the chrome must carry exactly one #device-slot: the persistent media ' +
    'element is moved into it, so the buffer survives a document swap');
});

test('a reading session has no player chrome at all', () => {
  assert.equal(R.session(golden('session-read'), {}), '');
  assert.equal(R.session(golden('session-read-pdf'), {}), '');
});

test('a missing action is a control that is not there', () => {
  const doc = golden('item-film');
  const stripped = JSON.parse(JSON.stringify(doc));
  delete stripped.actions.play;
  const html = R.item(stripped, {});
  assert.ok(!html.includes('data-act="play"'));
  // …and the reason the document gives is shown in its place.
  assert.ok(html.includes(R.esc(doc.unavailable.read.reason)) || html.includes('unavailable'));
});

// --- what each view actually draws -------------------------------------------

// The titles a stretch of HTML draws, in the order it draws them.
const cardTitles = html => [...html.matchAll(/class="c-title">([^<]*)</g)].map(m => unesc(m[1]));

// The bands are the document's, so the sections are too: one per band the
// document names, under its own heading, holding the tiles that name it.
test('the library draws a headed section per band, in the document order', () => {
  const doc = golden('library');
  const html = R.library(doc, {});

  const headings = [...html.matchAll(/<section class="band"[^>]*>\s*<h3>([^<]*)</g)].map(m => unesc(m[1]));
  assert.deepEqual(headings, ['Recently added'].concat(doc.bands.map(b => b.title)));

  // Every tile is drawn once per section it belongs to, and the bands
  // together draw the document's items in the document's own order.
  const bandsHtml = html.slice(html.indexOf('data-band='));
  assert.deepEqual(cardTitles(bandsHtml), doc.items.map(t => unesc(R.esc(t.title))));
  for (const b of doc.bands) {
    const section = bandsHtml.split('data-band="' + b.key + '"')[1].split('</section>')[0];
    assert.deepEqual(cardTitles(section),
      doc.items.filter(t => t.band === b.key).map(t => unesc(R.esc(t.title))));
  }

  // The count went: a headed row says what "6 titles" was trying to.
  assert.doesNotMatch(html, /\d+ titles?/);
});

// What arrived lately leads, and it is the same tiles the bands hold.
test('recently added is the first row, above the bands', () => {
  const doc = golden('library');
  const html = R.library(doc, {});
  assert.ok(html.indexOf('id="recent-row"') < html.indexOf('data-band='));
  const row = html.split('id="recent-row"')[1].split('</section>')[0];
  assert.deepEqual(cardTitles(row), doc.recently_added.map(t => unesc(R.esc(t.title))));

  // A document with nothing new in it draws no row at all.
  const bare = Object.assign({}, doc, { recently_added: [] });
  assert.ok(!R.library(bare, {}).includes('id="recent-row"'));
});

// Search filters INSIDE the sections, and a section it empties disappears
// rather than standing as a heading over nothing.
test('a section the search empties is not drawn', () => {
  const doc = golden('library');
  const html = R.library(doc, { q: 'dune' });
  const headings = [...html.matchAll(/<section class="band"[^>]*>\s*<h3>([^<]*)</g)].map(m => unesc(m[1]));
  assert.deepEqual(headings, ['Recently added', 'Audiobooks']);
  assert.deepEqual(cardTitles(html.slice(html.indexOf('data-band='))), ['Dune']);
});

// A tile is a div, so it is reachable only if the renderer says so: the
// keyboard's half of the click delegation is worth nothing without this.
test('every tile is focusable and says what activating it does', () => {
  const views = [
    ['library', R.library(golden('library'), {})],
    ['continue', R.continueShelf(golden('continue'))],
    ['work (show)', R.work(golden('work-show'))],
  ];
  const isTile = c => c === 'card' || c === 'cw-card' || c === 'item';
  let seen = 0;
  for (const [name, html] of views) {
    for (const m of html.matchAll(/<div\b[^>]*>/g)) {
      const tag = m[0];
      const cls = (tag.match(/class="([^"]*)"/) || ['', ''])[1].split(/\s+/);
      if (!cls.some(isTile)) continue;
      seen++;
      assert.match(tag, /tabindex="0"/, `${name}: tile is not in the tab order: ${tag}`);
      assert.match(tag, /role="(link|button)"/, `${name}: tile has no role: ${tag}`);
      assert.match(tag, /data-(nav|act)="/, `${name}: focusable tile activates nothing: ${tag}`);
    }
  }
  assert.ok(seen >= 3, 'no tiles were checked');
});

test('the genre chip and the search box only filter', () => {
  const doc = golden('library');
  const comedy = R.libraryGrid(doc, { genre: 'Comedy' });
  assert.equal(comedy.count, 1);
  const dune = R.libraryGrid(doc, { q: 'dune' });
  assert.equal(dune.count, 1);
  assert.equal(R.libraryGrid(doc, { q: 'nothing at all' }).count, 0);
});

test('a tile opens the route its kind spells', () => {
  const doc = golden('library');
  const byTitle = Object.fromEntries(doc.items.map(t => [t.title, R.hashFor(t)]));
  assert.equal(byTitle['The Office'], '#/show/The%20Office');
  assert.equal(byTitle['Radiohead'], '#/artist/Radiohead');
  assert.equal(byTitle['Frozen'], '#/item/4');
});

test('the show pane keeps bonus material out of the run', () => {
  const doc = golden('work-show');
  const html = R.work(doc);
  const eps = doc.members.filter(m => !m.extra);
  const bonus = doc.members.filter(m => m.extra);
  assert.equal(eps.length, 2);
  assert.equal(bonus.length, 1);
  const [list, extras] = html.split('id="extras-list"');
  for (const m of eps) assert.ok(list.includes(R.esc(m.label)), m.label);
  for (const m of bonus) assert.ok(extras.includes(R.esc(m.label)), m.label);
});

test('a record pane lists its members with the server\'s own labels', () => {
  const html = R.work(golden('work-album'));
  assert.ok(html.includes('Track 1 · Airbag'));
  assert.ok(html.includes('Track 2 · Paranoid Android'));
  assert.ok(html.includes('2 tracks'));
});

test('the resume shelf draws the work title, the place and one action', () => {
  const doc = golden('continue');
  const html = R.continueShelf(doc);
  for (const en of doc.items) {
    assert.ok(html.includes(R.esc(en.work_title)), en.work_title);
    assert.ok(html.includes('width:' + en.percent.toFixed(1) + '%'),
      'the bar is the document\'s own percent, in one unit for every medium');
    // Each ROW is a control: the entry's own resume action, with the label the
    // server chose for the medium ("▶ Resume", "Keep reading").
    const act = en.actions.resume;
    assert.ok(html.includes('data-href="' + R.esc(act.href) + '"'), en.title + ': no resume href');
    assert.ok(html.includes(R.esc(act.label)), en.title + ': no resume label');
  }
});

test('the player chrome is the session document, and nothing else', () => {
  const doc = golden('session-play');
  const html = R.session(doc, {});
  assert.ok(html.includes('Keep watching'), 'the escape hatch, labelled as the server labels it');
  assert.ok(html.includes('Up next: ' + R.esc(doc.links.next.title)));
  assert.ok(html.includes('data-act="mark_in"') && html.includes('data-act="mark_out"'));
  // No in point yet: the link is an `unavailable` with its reason, not a button.
  assert.ok(!html.includes('data-act="link"'));
  assert.ok(html.includes('id="btn-prev-ep"') === !!doc.links.prev);
});

test('a mark that has been set shows where it was set', () => {
  const doc = JSON.parse(JSON.stringify(golden('session-play')));
  doc.marks = { in: { item_id: doc.item_id, seconds: 142 } };
  doc.actions.link = { method: 'GET', href: doc.self + '/link', label: 'Copy passage link' };
  delete doc.unavailable.link;
  const html = R.session(doc, {});
  assert.ok(html.includes('In 2:22'), 'the chip says the time the server kept');
  assert.ok(html.includes('markchip set'));
  assert.ok(html.includes('data-act="link"'));
});

test('the curatorial controls are inside the Details fold, and Play is not', () => {
  const doc = golden('item-film');
  const html = R.item(doc, {});
  const [above, folded] = html.split('<details id="detail-details">');
  assert.ok(above.includes('data-act="play"'), 'Play stands alone above the fold');
  for (const n of ['identity', 'reprobe']) {
    assert.ok(!above.includes('data-act="' + n + '"'), n + ' is not a peer of Play');
    assert.ok(folded.includes('data-act="' + n + '"'), n + ' is not inside the fold');
  }
  // An action the document does not afford is its reason, in the same fold.
  assert.ok(folded.includes(R.esc(doc.unavailable.enrich.reason)));
});

test('the identity form is drawn from the action\'s input sketch', () => {
  const doc = golden('item-film');
  const act = doc.actions.identity;
  const html = R.identityForm(act, doc.identity);
  // One control per sketch field, and nothing the sketch did not name.
  const names = [...html.matchAll(/<input name="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual([...names].sort(), Object.keys(act.input).sort());
  // The sketch's type is the control's type, and its '?' is the difference
  // between a required field and an optional one.
  for (const [k, t] of Object.entries(act.input)) {
    const field = html.match(new RegExp('<input name="' + k + '"[^>]*>'))[0];
    assert.match(field, new RegExp('type="' + (t.startsWith('number') ? 'number' : 'text') + '"'), k);
    assert.equal(/\brequired\b/.test(field), !t.endsWith('?'), k + ': wrong requiredness');
  }
  // The current identity fills it in; a field the identity does not carry is
  // blank rather than absent.
  assert.match(html, /<input name="title"[^>]*value="Frozen"/);
  assert.match(html, /<input name="year"[^>]*value="2013"/);
  assert.match(html, /<input name="season"[^>]*value=""/);
  // The submit goes where the action says, by the method it names, under the
  // document's own label.
  assert.ok(html.includes('data-href="' + R.esc(act.href) + '"'));
  assert.ok(html.includes('data-method="POST"'));
  assert.ok(html.includes(R.esc(act.label)));
});

test('a sketch that grows a field grows the form', () => {
  const doc = golden('item-film');
  const act = JSON.parse(JSON.stringify(doc.actions.identity));
  act.input.disc = 'number?';
  const html = R.identityForm(act, doc.identity);
  assert.match(html, /<input name="disc" type="number"/);
  // The known fields keep their order; what is new comes after them.
  const names = [...html.matchAll(/<input name="([^"]+)"/g)].map(m => m[1]);
  assert.deepEqual(names.slice(0, 3), ['kind', 'title', 'year']);
  assert.equal(names[names.length - 1], 'disc');
});

test('the identity form with no identity yet is blank, not broken', () => {
  const html = R.identityForm(golden('item-film').actions.identity, null);
  assert.ok(!/value="[^"]+"/.test(html), 'every field is empty');
  assert.equal(R.identityForm(null, {}), '');
});

test('the decision trace hides behind a disclosure, closed by default', () => {
  const doc = golden('session-play');
  const t = R.trace(doc.decision, 'h264_vaapi', '');
  // The badge line stays out in the open: the verdict, said once.
  assert.ok(t.status.includes('badge direct') && t.status.includes('direct play'));
  assert.ok(t.status.includes('via h264_vaapi'));
  // The steps are all still there, but behind a summary that asks the question
  // the viewer would ask — and nothing opens it for them.
  assert.ok(t.trace.startsWith('<details><summary>Why does this play directly?</summary>'),
    'the steps are closed by default');
  assert.ok(!t.trace.includes('<details open'));
  for (const s of doc.decision.trace) assert.ok(t.trace.includes(R.esc(s.check)), s.check);

  const transcoding = {
    method: 'transcode',
    trace: [{ check: 'video_codec', passed: false, detail: 'file is hevc; client plays h264' }],
  };
  assert.ok(R.trace(transcoding, '', '').trace.includes('<summary>Why is this transcoding?</summary>'));
});
