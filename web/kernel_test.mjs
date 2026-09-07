// Run with: node --test web/kernel_test.mjs
//
// The kernel's own contract, checked where it can be checked without a
// browser: what it knows by heart, what it follows, and what it never builds.
//
// kernel.js and player.js are written against `window`, so they are not
// imported here — what is asserted instead is the SOURCE and the shape of the
// documents it reads. That is the part worth holding: the moment either file
// starts composing an address the client is a fat client again, and the
// moment a renderer emits a <video> the buffer stops surviving a re-render.
import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import R from './renderers.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const read = f => fs.readFileSync(path.join(here, f), 'utf8');
const golden = n => JSON.parse(read(path.join('testdata', 'hyper', n + '.json')));

// The comments explain the rule and are entitled to say '/api/'; the code is
// what must not.
function code(src) {
  return src.replace(/\/\/[^\n]*/g, '').replace(/\/\*[\s\S]*?\*\//g, '');
}
function apiLiterals(src) {
  return [...code(src).matchAll(/['"`](\/api[^'"`]*)['"`]/g)].map(m => m[1]);
}

test('the kernel knows exactly one address by heart: the root', () => {
  const found = apiLiterals(read('kernel.js'));
  assert.deepEqual([...new Set(found)], ['/api/'],
    'the root is the one address a client may hold: everything else is a ' +
    'relation followed out of a document.');
});

test('the device composes no address at all', () => {
  assert.deepEqual(apiLiterals(read('player.js')), []);
  assert.deepEqual(apiLiterals(read('renderers.js')), []);
});

test('index.html carries markup and script tags, and one line of logic', () => {
  const html = read('index.html');
  const inline = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)]
    .map(m => m[1].trim()).filter(Boolean);
  assert.deepEqual(inline, ['Kernel.boot();'],
    'the only logic left in the page is the boot call');
  // The three client files, in dependency order: the renderers define Render,
  // the device reads it, the kernel drives both.
  const srcs = [...html.matchAll(/<script[^>]*\bsrc="([^"]+)"/g)].map(m => m[1]);
  const order = ['renderers.js', 'player.js', 'kernel.js'].map(f => srcs.indexOf(f));
  assert.ok(order.every(i => i >= 0), 'all three client files are loaded: ' + srcs);
  assert.deepEqual(order, [...order].sort((a, b) => a - b), 'loaded out of dependency order');
  // Comments are allowed to name the tag they are explaining; the markup is not.
  const markup = html.replace(/<!--[\s\S]*?-->/g, '');
  assert.ok(!/<video\b/i.test(markup),
    'the page must not carry a <video>: the device makes exactly one, and it ' +
    'outlives every render');
});

test('the shell lists every file the page loads, and the cache is bumped with them', () => {
  const html = read('index.html');
  const sw = read('sw.js');
  const shell = JSON.parse(sw.match(/const SHELL = (\[[\s\S]*?\]);/)[1].replace(/'/g, '"'));
  const local = [...html.matchAll(/<(?:script|link)[^>]*(?:src|href)="(\/[^"]+|[a-z][^":]*\.(?:js|css|mjs))"/g)]
    .map(m => (m[1].startsWith('/') ? m[1] : '/' + m[1]));
  for (const f of local) {
    if (f.startsWith('/icon') || f === '/manifest.json') continue;
    assert.ok(shell.includes(f), `${f} is loaded by index.html but is not in sw.js's SHELL`);
  }
  assert.match(sw, /const CACHE = 'flickr-shell-v\d+';/);
});

// --- the documents the kernel follows ----------------------------------------

test('the root names everything the client needs to reach', () => {
  const root = golden('root');
  for (const rel of ['library', 'search', 'continue', 'artists', 'scan', 'system', 'profiles']) {
    assert.ok(root.links[rel] && root.links[rel].href, `the root has no ${rel} relation`);
  }
  // The route resolver is the one address the kernel would otherwise have had
  // to know: it is named here, so the client holds only '/api/'.
  assert.ok(root.actions.route && root.actions.route.href, 'the root does not name the route resolver');
  assert.equal(root.actions.route.method, 'GET');
  assert.ok(root.actions.route.input.hash, 'the route action does not take a hash');
  assert.ok(root.actions.create_profile, 'the root does not offer a new profile');
});

test('a route answer says which view, and names the document to draw it from', () => {
  const r = golden('route-text-passage');
  assert.equal(r.kind, 'route');
  assert.equal(r.view, 'item');
  assert.equal(r.document.kind, 'item');
  assert.equal(r.autoplay, true);
  // The passage arrives resolved: the reader is handed a place, not a spelling.
  assert.equal(r.passage.from_section, 3);
  assert.equal(r.passage.to_section, 4);
});

// The search document is a place, not a form: every hit says where it goes
// and what to draw, and none of them offers an action to take there.
test('a search answers groups of hits, each drawable and addressable', () => {
  const doc = golden('search');
  assert.equal(doc.kind, 'search');
  assert.ok(doc.groups.length, 'the fixture query matches something');
  for (const g of doc.groups) {
    assert.ok(g.key && g.title && g.items.length, `the ${g.key} group is not drawable`);
    for (const en of g.items) {
      assert.ok(en.title && en.item_id, `${en.self}: nothing to draw or open`);
      assert.ok(en.links.artwork, `${en.title}: the picture is a link, not a guess`);
      assert.equal(en.actions, undefined, `${en.title}: a result offers no action`);
    }
  }
});

test('an item document says where this profile left off, so nothing asks a progress route', () => {
  const doc = golden('continue').items.find(e => e.id === 8);
  assert.ok(doc.resume && doc.resume.position_seconds > 0,
    'the resume position is on the document; the browser used to fetch it by hand');
});

test('a session document carries the passage, the marks and the way out', () => {
  const s = golden('session-play');
  assert.equal(s.kind, 'session');
  assert.ok(s.actions.progress.href.startsWith(s.self),
    'the heartbeat writes to the session, which is what refuses it with 409');
  assert.ok(s.actions.keep_watching, 'a passage session offers the escape hatch');
  assert.ok(s.links.back && s.links.back.title, 'the end panel is told where Back goes');
  assert.deepEqual(Object.keys(s.marks), [], 'nothing is marked yet');
});

test('a reading session is a session too, and the reader takes it whole', () => {
  const s = golden('session-read');
  assert.equal(s.method, 'read');
  assert.ok(s.links.book, 'the bytes are a relation');
  assert.ok(s.sections.length, 'the contents came with it');
  assert.ok(s.actions.keep_reading, 'a text passage has the same way out, in its own words');
  assert.equal(R.session(s, {}), '', 'and no player chrome is drawn for it');
});

test('the resume shelf carries its own pictures and one action per row', () => {
  const doc = golden('continue');
  assert.ok(doc.items.length);
  for (const en of doc.items) {
    assert.ok(en.links.artwork, `${en.title}: the row's picture is a link, not a guess`);
    assert.ok(en.actions.resume, `${en.title}: no resume action`);
    assert.equal(typeof en.percent, 'number');
  }
});

test('every golden the client renders is the one the server writes', () => {
  // The Go side copies them and fails on drift (goldens_shared_test.go); this
  // is the other half of that sentence — the files are actually here.
  const names = fs.readdirSync(path.join(here, 'testdata', 'hyper'));
  for (const n of ['root', 'library', 'search', 'artist', 'continue', 'item-film', 'work-show',
                   'work-album', 'work-audiobook', 'work-book',
                   'session-play', 'session-read', 'session-read-pdf', 'route-text-passage']) {
    assert.ok(names.includes(n + '.json'), `${n}.json is not shared with the client`);
  }
});
