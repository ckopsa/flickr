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
import vm from 'node:vm';
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

// How the subtitles look is the one setting no document has an opinion about,
// so it is split across three files: the page offers the controls and carries
// the sheet, the kernel writes the ::cue rule into it, and the device — the
// only half that can reach a cue — puts the line on the cues it has loaded.
// The halves have to agree on the ids, which is what this checks.
test('the subtitle look has a control, a rule to write and a device to place it', () => {
  const html = read('index.html');
  const src = read('kernel.js');
  assert.match(html, /<style id="cue-style">/,
    'the page carries no sheet for the kernel to write the ::cue rule into');
  for (const id of ['cue-size', 'cue-ground', 'cue-position']) {
    assert.match(html, new RegExp('<select id="' + id + '"'), id + ' is not on the page');
    assert.ok(code(src).includes("'" + id + "'"), id + ' is not driven by the kernel');
  }
  assert.match(code(src), /::cue \{/, 'the kernel writes no ::cue rule');
  assert.match(code(read('player.js')), /setCueLine/,
    'the device is never told where the cues sit');
});

// --- background audio ---------------------------------------------------------
//
// A phone with its screen locked is still playing, and the way that stops being
// true is a listener: pause on visibilitychange, tear the element down on
// pagehide, drop the buffer on blur. None of them is here, and this is the test
// that keeps it that way. `beforeunload` is allowed and wanted — that is the
// page actually going away, which is when the sitting should end.
test('nothing in the kernel stops the sound when the page goes to the background', () => {
  const src = code(read('kernel.js'));
  for (const ev of ['visibilitychange', 'pagehide', 'blur']) {
    assert.ok(!src.includes(ev),
      `kernel.js listens for ${ev}: a locked phone would stop playing mid-chapter`);
  }
  assert.match(src, /'beforeunload'/, 'nothing ends the sitting when the page goes away');
  // A write that has to outlive the page says so, and api() hands opts straight
  // to fetch rather than picking the fields it passes on.
  assert.match(src, /opts\.quiet \|\| opts\.keepalive/,
    'a keepalive beacon would be taken for a refusal and open the sign-in door');
  assert.match(src, /fetch\(href, opts\)/, 'api() does not pass keepalive through to fetch');
});

// --- installing ---------------------------------------------------------------
//
// The panel's install row: the renderer is pure and tested over its four states
// in web/render_test.mjs, so what is checked here is that the kernel holds the
// halves the renderer cannot — the event, which can only be answered while it
// is held, and the two questions that decide which of the four states it is.
test('the kernel keeps the browser\'s install offer and draws the row from it', () => {
  const src = code(read('kernel.js'));
  assert.match(src, /'beforeinstallprompt'/, 'the offer is never caught');
  assert.match(src, /'appinstalled'/, 'the button outlives the install');
  assert.match(src, /\(display-mode: standalone\)/, 'an installed page is still offered an install');
  assert.match(src, /navigator\.standalone/, 'iOS says it is installed the other way');
  assert.match(src, /R\.install\(/, 'the row is drawn somewhere other than the renderer');
  // The page carries the box the kernel fills, and it starts out empty.
  assert.match(read('index.html'), /id="settings-install-row" hidden/,
    'the settings panel has no row for the install');
});

// --- the documents the kernel follows ----------------------------------------

test('the root names everything the client needs to reach', () => {
  const root = golden('root');
  for (const rel of ['library', 'search', 'continue', 'artists', 'scan', 'system', 'profiles',
                     'activity']) {
    assert.ok(root.links[rel] && root.links[rel].href, `the root has no ${rel} relation`);
  }
  // The route resolver is the one address the kernel would otherwise have had
  // to know: it is named here, so the client holds only '/api/'.
  assert.ok(root.actions.route && root.actions.route.href, 'the root does not name the route resolver');
  assert.equal(root.actions.route.method, 'GET');
  assert.ok(root.actions.route.input.hash, 'the route action does not take a hash');
  assert.ok(root.actions.create_profile, 'the root does not offer a new profile');
  // The faces the gate offers are the server's list, and the create takes one:
  // the browser invents no emoji of its own.
  assert.ok(Array.isArray(root.avatars) && root.avatars.length,
    'the root offers the gate no faces to pick from');
  assert.ok(root.actions.create_profile.input.avatar, 'a new profile cannot be given a face');
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
      // An artist's shelf is opened by the name on it; every other hit
      // carries the member a tap opens at.
      assert.ok(en.title && (en.item_id || g.key === 'artists'),
        `${en.self}: nothing to draw or open`);
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
    assert.ok(en.actions.forget, `${en.title}: no way to take the row off the shelf`);
    assert.equal(typeof en.percent, 'number');
  }
});

// My List: the one shelf a person writes rather than one the library
// derives. Every half of it is the document's — the root names it, the home
// carries it, and a tile says which way the next press goes.
test('my list is a shelf of tiles, and the bookmark says which way it goes', () => {
  const doc = golden('list');
  assert.equal(doc.kind, 'list');
  assert.equal(doc.count, doc.items.length);
  for (const t of doc.items) {
    assert.equal(t.actions.unsave.method, 'DELETE', `${t.title}: no way off the list`);
    assert.ok(!t.actions.save, `${t.title}: on the list and offered a save as well`);
  }
  assert.ok(golden('root').links.list, 'the root does not name the shelf');
  // The library carries the same shelf, so the home draws the row without a
  // second fetch.
  assert.deepEqual(golden('library').list.map(t => t.self), doc.items.map(t => t.self));
  // A press against it moves a shelf, so the kernel re-reads the view rather
  // than patching the tile where it stands.
  assert.match(read('kernel.js'), /name === 'save' \|\| name === 'unsave'/,
    'the bookmark is not one of the writes the kernel re-reads after');
});

// The household dashboard behind the gear: a list of live sittings, each
// drawable from its own words, and not one action anywhere — the panel shows
// who is playing what, it does not reach across the room.
test('the activity document is a read, and every row draws itself', () => {
  const doc = golden('activity');
  assert.equal(doc.kind, 'activity');
  assert.equal(doc.actions, undefined, 'the dashboard offers an action');
  assert.ok(doc.items.length, 'the fixture has a sitting in it');
  for (const a of doc.items) {
    assert.ok(a.profile, 'a sitting with nobody in it');
    assert.ok(a.work_title || a.title, `${a.session_id}: nothing to name it by`);
    assert.equal(typeof a.position, 'string', 'the place is words, not a sum');
    assert.ok(a.method && a.started_at, `${a.session_id}: how and since when`);
    assert.equal(a.actions, undefined, `${a.session_id}: a row offers an action`);
  }
  // The panel reads it by following the root, and polls only while it is up.
  const src = read('kernel.js');
  assert.match(src, /links\.activity/, 'the panel composes the address itself');
  assert.match(src, /watchActivity\(false\)/, 'the poll never stops');
});

// The mark is one action with two labels, and the value to send back rides in
// its `input`: the client never works out which way the press goes.
test('an item says how it is marked watched, and which way the mark goes', () => {
  for (const [name, doc] of [['item-film', golden('item-film')],
                             ['a member', golden('work-show').members[0]]]) {
    const act = doc.actions.watched;
    assert.ok(act, `${name}: no watched action`);
    assert.equal(act.method, 'POST');
    assert.ok(/^Mark (un)?watched$/.test(act.label), `${name}: label is ${act.label}`);
    assert.ok(act.input.watched === 'true' || act.input.watched === 'false',
      `${name}: the action does not carry the value to send back`);
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

// --- the sign-in door ---------------------------------------------------------
//
// The one test here that RUNS the kernel rather than reading it: kernel.js is
// written against `window`, so it is given one — a sandbox with the few
// globals the file touches as it loads, and a fetch answering what a server
// with nobody signed in answers. Nothing is mounted; api() is the surface.
function loadKernel(over) {
  const sandbox = Object.assign({
    Render: R,
    Player: {},
    console: console,
    document: { getElementById: () => null, addEventListener() {}, cookie: '' },
    localStorage: {},
    matchMedia: () => ({ matches: false }),
    navigator: { userAgent: '' },
    location: { pathname: '/', search: '', hash: '#/item/8', assign() {} },
    fetch: () => Promise.reject(new Error('no fetch in this test')),
  }, over || {});
  sandbox.window = sandbox;
  vm.runInNewContext(read('kernel.js'), sandbox, { filename: 'kernel.js' });
  return sandbox.Kernel;
}

// What the server answers an anonymous read with: the sentence, and the door.
const UNAUTHENTICATED = {
  type: 'unauthenticated',
  title: 'Sign in to use flickr',
  status: 401,
  detail: 'This address answers a signed-in household member.',
  remedy: {
    text: 'Sign in with the household account.',
    link: { href: '/auth/login', title: 'Sign in' },
  },
};

function refusal(body) {
  return {
    status: 401, ok: false, statusText: 'Unauthorized',
    json: () => Promise.resolve(body),
  };
}

test('a 401 with a remedy sends the browser to sign in, once, carrying where it was', async () => {
  const K = loadKernel({ fetch: () => Promise.resolve(refusal(UNAUTHENTICATED)) });
  const went = [];
  K.leave = href => went.push(href);

  // The refusal is still an answer: it is thrown, and the remedy reads as the
  // server's own sentence (Remedy is {text, link}).
  const e = await K.api('/api/').then(() => null, x => x);
  assert.ok(e instanceof K.Problem);
  assert.equal(e.status, 401);
  assert.equal(e.remedyText, 'Sign in with the household account.');

  // Where it was includes the HASH, which is the place — added to the server's
  // own href, which carries none because the server cannot see one.
  assert.deepEqual(went, ['/auth/login?return_to=' + encodeURIComponent('/#/item/8')]);

  // Twice is a loop. A second 401 refuses as before and goes nowhere.
  await K.api('/api/').then(() => null, x => x);
  assert.equal(went.length, 1, 'the door was opened twice');
});

test('a best-effort request fails quietly rather than taking the page away', async () => {
  const K = loadKernel({ fetch: () => Promise.resolve(refusal(UNAUTHENTICATED)) });
  const went = [];
  K.leave = href => went.push(href);
  await K.api('/api/', { method: 'POST', quiet: true }).then(() => null, x => x);
  await K.api('/api/', { method: 'POST', keepalive: true }).then(() => null, x => x);
  assert.deepEqual(went, [], 'a beacon took the page out from under the viewer');
});

test('a 401 with no remedy is a refusal like any other', async () => {
  const K = loadKernel({ fetch: () => Promise.resolve(refusal({ title: 'No.', status: 401 })) });
  const went = [];
  K.leave = href => went.push(href);
  const e = await K.api('/api/').then(() => null, x => x);
  assert.equal(e.status, 401);
  assert.deepEqual(went, []);
});

// --- the back gesture ---------------------------------------------------------
//
// Installed on a home screen there is no browser chrome, so the Android back
// gesture is the only back there is: it has to leave an item for the page it was
// opened from rather than closing the app. Nothing handles it, and nothing
// should — every navigation here is a hash, and setting a hash PUSHES an entry.
// So what is held is that the kernel still navigates that way. A location with a
// history stack behind it is enough to see it: `navigate` grows the stack, and
// `replaceHash` — a hash rewritten in place — does not.
function historyStack(start) {
  const stack = [start];
  let at = 0;
  const location = {
    pathname: '/', search: '',
    get hash() { return stack[at]; },
    set hash(v) {
      if (v === stack[at]) return;
      stack.length = at + 1;
      stack.push(v);
      at = stack.length - 1;
    },
    replace(v) { stack[at] = v; },
    assign() {},
  };
  return {
    location,
    history: { back() { if (at > 0) at--; }, replaceState(_s, _t, v) { stack[at] = v; } },
    entries: () => stack.slice(0, at + 1),
  };
}

test('navigating pushes history, so the back gesture leaves an item for the library', () => {
  const h = historyStack('#/');
  const K = loadKernel({ location: h.location, history: h.history });

  K.navigate('#/item/8');   // as a poster's tap sends it
  assert.equal(h.location.hash, '#/item/8');
  assert.deepEqual(h.entries(), ['#/', '#/item/8'],
    'the item got no entry of its own: back would close the app');

  h.history.back();
  assert.equal(h.location.hash, '#/', 'back from an item does not land on the library');

  // A hash rewritten in PLACE — the passage dropped off the item that is
  // playing, the reader keeping its book — is not somewhere the gesture should
  // have to stop on the way home.
  h.location.hash = '#/item/8?t=90';
  K.replaceHash('#/item/8');
  assert.deepEqual(h.entries(), ['#/', '#/item/8'], 'replacing a hash grew the history');
});
